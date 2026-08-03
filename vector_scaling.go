package main

import (
	"archive/tar"
	"compress/gzip"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"

	"mojojojo/scale"
)

// vectorLoad is the local admission queue. Retrieval gets its own queue and
// scaler so an embedding burst cannot rent or return code-execution workers.
type vectorLoad struct {
	mu     sync.Mutex
	queued map[scale.Kind]int
	since  map[scale.Kind]time.Time
	last   time.Time
}

func newVectorLoad() *vectorLoad {
	return &vectorLoad{queued: map[scale.Kind]int{}, since: map[scale.Kind]time.Time{}, last: time.Now()}
}

func scaleKindForProject(project *vectorProject) scale.Kind {
	if project != nil && project.Tier == "performance" {
		return scale.GPU
	}
	return scale.CPU
}

func (s *Server) ensureVectorRuntime() {
	if s.vectorLoad == nil {
		s.vectorLoad = newVectorLoad()
	}
	if s.vectorSlots == nil {
		capacity := s.cfg.VectorConcurrency
		if capacity <= 0 {
			capacity = 8
		}
		s.vectorSlots = make(chan struct{}, capacity)
	}
	if s.reservedVectorSlots == nil {
		s.reservedVectorSlots = make(chan struct{}, max(1, s.cfg.ReservedVectorSlots))
	}
	if s.vectorClients == nil {
		s.vectorClients = map[string]*vectorClient{}
	}
}

func (s *Server) acquireReservedVector() func() {
	s.ensureVectorRuntime()
	s.reservedVectorSlots <- struct{}{}
	return func() { <-s.reservedVectorSlots }
}

func (s *Server) acquireVector(kind scale.Kind) func() {
	s.ensureVectorRuntime()
	select {
	case s.vectorSlots <- struct{}{}:
		s.vectorLoad.mu.Lock()
		s.vectorLoad.last = time.Now()
		s.vectorLoad.mu.Unlock()
	default:
		s.vectorLoad.mu.Lock()
		if s.vectorLoad.queued[kind] == 0 {
			s.vectorLoad.since[kind] = time.Now()
		}
		s.vectorLoad.queued[kind]++
		s.vectorLoad.mu.Unlock()
		s.vectorSlots <- struct{}{}
		s.vectorLoad.mu.Lock()
		s.vectorLoad.queued[kind]--
		if s.vectorLoad.queued[kind] == 0 {
			delete(s.vectorLoad.since, kind)
		}
		s.vectorLoad.last = time.Now()
		s.vectorLoad.mu.Unlock()
	}
	return func() {
		<-s.vectorSlots
		s.vectorLoad.mu.Lock()
		s.vectorLoad.last = time.Now()
		s.vectorLoad.mu.Unlock()
	}
}

// pickVectorBackend spills only when every local vector slot is occupied.
// Performance workers must advertise a verified GPU backend; a GPU-shaped VM
// running the exact CPU fallback is not enough.
func (s *Server) pickVectorBackend(project *vectorProject, userID string) (*vectorClient, string, func()) {
	s.ensureVectorRuntime()
	kind := scaleKindForProject(project)
	if kind == scale.GPU && s.hasActiveVectorReservation(userID) {
		return s.vector, "reserved", s.acquireReservedVector()
	}
	if len(s.vectorSlots) >= cap(s.vectorSlots) && s.vectorScaler != nil {
		for _, worker := range s.vectorScaler.Ready(kind) {
			base := worker.IP
			if !strings.Contains(base, ":") {
				base += ":4342"
			}
			base = "http://" + base
			s.vectorLoad.mu.Lock()
			client := s.vectorClients[base]
			if client == nil {
				client = newVectorClient(base, s.cfg.VectorToken, &http.Client{Timeout: 2 * time.Minute})
				s.vectorClients[base] = client
			}
			s.vectorLoad.mu.Unlock()
			health, err := client.health()
			if err != nil {
				continue
			}
			if kind == scale.GPU {
				gpu, _ := health["gpu"].(bool)
				if !gpu {
					continue
				}
			} else if mojo, _ := health["mojo"].(bool); !mojo {
				continue
			}
			return client, worker.Provider + ":" + worker.ID, func() {}
		}
	}
	return s.vector, "local", s.acquireVector(kind)
}

func (s *Server) startVectorScaling() {
	providers := scale.FromEnv()
	if len(providers) == 0 {
		return
	}
	s.vectorScaler = scale.NewManager(providers, s.vectorScaleSignal, s.vectorWorkerBootstrap)
	if interval := os.Getenv("VECTOR_SCALE_INTERVAL"); interval != "" {
		if duration, err := time.ParseDuration(interval); err == nil {
			s.vectorScaler.Interval = duration
		}
	}
	// GPU launch is gated until a deploy explicitly includes a backend whose
	// worker health probe can return gpu=true. CPU overflow is ready today.
	if os.Getenv("VECTOR_GPU_BACKEND_ENABLED") != "true" {
		limits := s.vectorScaler.Limits[scale.GPU]
		limits.MaxWorkers = 0
		s.vectorScaler.Limits[scale.GPU] = limits
	}
	s.vectorScaler.Start()
}

func (s *Server) vectorScaleSignal(kind scale.Kind) scale.Snapshot {
	s.ensureVectorRuntime()
	s.vectorLoad.mu.Lock()
	queued := s.vectorLoad.queued[kind]
	since := s.vectorLoad.since[kind]
	last := s.vectorLoad.last
	s.vectorLoad.mu.Unlock()
	snapshot := scale.Snapshot{Kind: kind, Queued: queued}
	if !since.IsZero() {
		snapshot.QueuedFor = time.Since(since)
	}
	if queued == 0 {
		snapshot.Idle = time.Since(last)
	}
	snapshot.PaidCreditsPerHour = s.bill.vectorCreditsSince(time.Now().Add(-time.Hour))
	return snapshot
}

func (b *billing) vectorCreditsSince(after time.Time) float64 {
	var credits int
	if err := b.db.QueryRow(`SELECT COALESCE(SUM(charged_credits),0) FROM vector_usage
		WHERE created_at >= $1`, after.UTC()).Scan(&credits); err != nil {
		return 0
	}
	elapsed := time.Since(after).Hours()
	if elapsed <= 0 {
		return 0
	}
	return float64(credits) / elapsed
}

func (s *Server) vectorWorkerBootstrap(kind scale.Kind) string {
	return scale.VectorBootstrapScript(s.cfg.SiteBaseURL, s.cfg.VectorToken, kind)
}

// The vector bundle is public source plus model weights and runtime libraries;
// it contains no account, provider, or storage credentials.
func (s *Server) handleVectorWorkerTarball(ctx *fasthttp.RequestCtx) {
	ctx.SetContentType("application/gzip")
	ctx.Response.Header.Set("Cache-Control", "no-store")
	gz := gzip.NewWriter(ctx)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	files := map[string]string{
		"model/modelint8_512dim.safetensors": "../gobed/model/modelint8_512dim.safetensors",
		"model/tokenizer.json":               "../gobed/model/tokenizer.json",
		"lib/libmojo_embed.so":               "../mojo-embed/build/libmojo_embed.so",
		"runtime/libKGENCompilerRTShared.so": "../mojosub/.pixi/envs/default/lib/libKGENCompilerRTShared.so",
		"runtime/libAsyncRTMojoBindings.so":  "../mojosub/.pixi/envs/default/lib/libAsyncRTMojoBindings.so",
	}
	_ = filepath.Walk("vectorworker", func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && !strings.Contains(path, "__pycache__") && !strings.HasSuffix(path, ".pyc") {
			files[path] = path
		}
		return err
	})
	for name, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			log.Printf("vector worker tarball: %s: %v", path, err)
			return
		}
		info, _ := os.Stat(path)
		header := &tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), ModTime: info.ModTime()}
		if err := tw.WriteHeader(header); err != nil {
			return
		}
		if _, err := tw.Write(body); err != nil {
			return
		}
	}
}
