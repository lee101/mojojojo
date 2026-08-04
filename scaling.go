package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"

	"mojojojo/scale"
)

// The autoscaler, from the web tier's point of view: a thing that watches the
// queue, occasionally rents a machine, and offers a list of extra workers to
// send overflow runs to.
//
// It is off unless credentials exist, and it will not spend money unless
// SCALE_DRY_RUN=false is set explicitly. Both defaults are deliberate: the
// service is correct with no rented capacity — runs simply queue — and a
// controller whose bug costs money is a controller that should have to be
// switched on by hand.

func (s *Server) startScaling() {
	providers := scale.FromEnv()
	if len(providers) == 0 {
		return
	}
	s.scaler = scale.NewManager(providers, s.scaleSignal, s.workerBootstrap)
	if v := os.Getenv("SCALE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			s.scaler.Interval = d
		}
	}
	s.scaler.Start()
}

// scaleSignal answers "is there unmet, paying demand right now?"
func (s *Server) scaleSignal(kind scale.Kind) scale.Snapshot {
	snapshot := scale.Snapshot{Kind: kind}
	if health, err := s.runner.health(); err == nil {
		if kind == scale.GPU {
			snapshot.Queued = health.Queue.GPU
		} else {
			snapshot.Queued = health.Queue.CPU
		}
		snapshot.QueuedFor = time.Duration(health.Queue.ForS * float64(time.Second))
		if snapshot.Queued == 0 {
			snapshot.Idle = s.idleFor()
		}
	}
	// Revenue that already exists, in credits per hour. Welcome credits and
	// the free tier are excluded by construction: they are spent from a
	// balance that was granted, and `runs.credits` counts what actually left
	// the ledger.
	snapshot.PaidCreditsPerHour = s.bill.creditsSince(time.Now().Add(-time.Hour))
	return snapshot
}

// idleFor reports how long since the last run of any kind.
func (s *Server) idleFor() time.Duration {
	last := s.bill.lastRunAt()
	if last.IsZero() {
		return time.Hour
	}
	return time.Since(last)
}

func (s *Server) workerBootstrap(kind scale.Kind) string {
	return scale.BootstrapScript(s.cfg.SiteBaseURL, s.cfg.RunnerToken, kind)
}

// handleScaleStatus exposes what the controller is doing. Admin only: it names
// providers and prices, and it is the sort of thing that gets screenshotted.
func (s *Server) handleScaleStatus(ctx *fasthttp.RequestCtx) {
	u := s.auth.userFromRequest(ctx)
	if u == nil || !u.IsAdmin {
		sendError(ctx, 403, "admin only")
		return
	}
	if s.scaler == nil {
		sendJSON(ctx, map[string]any{"enabled": false,
			"reason": "no provider credentials configured"})
		return
	}
	status := s.scaler.Status()
	status["enabled"] = true
	status["spending"] = os.Getenv("SCALE_DRY_RUN") == "false"
	if s.vectorScaler != nil {
		status["vector"] = s.vectorScaler.Status()
		status["vector_gpu_backend_enabled"] = os.Getenv("VECTOR_GPU_BACKEND_ENABLED") == "true"
	}
	sendJSON(ctx, status)
}

// handleWorkerTarball serves the worker to a machine that is becoming one.
//
// Public on purpose: it is the runner source, it contains no secrets, and a
// rented machine has no credentials at the moment it needs this. What it does
// contain is the code that will execute untrusted programs. A process serves
// one immutable build: deploys restart it, so building once avoids recompressing
// the same bytes for every scale-out without risking cross-deploy drift.
var workerBundle cachedBundle

func (s *Server) handleWorkerTarball(ctx *fasthttp.RequestCtx) {
	body, etag, err := workerBundle.load(func() ([]byte, error) {
		var body bytes.Buffer
		gz := gzip.NewWriter(&body)
		tw := tar.NewWriter(gz)
		err := filepath.Walk("runner", func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			if strings.Contains(path, "__pycache__") || strings.HasSuffix(path, ".pyc") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			header := &tar.Header{
				Name: path, Mode: int64(info.Mode()),
				Size: int64(len(body)), ModTime: info.ModTime(),
			}
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			_, err = tw.Write(body)
			return err
		})
		if err == nil {
			err = tw.Close()
		}
		if closeErr := gz.Close(); err == nil {
			err = closeErr
		}
		return body.Bytes(), err
	})
	if err != nil {
		log.Printf("worker tarball: %v", err)
		ctx.Error("worker bundle unavailable", 500)
		return
	}
	serveCachedBundle(ctx, body, etag)
}

type cachedBundle struct {
	once sync.Once
	body []byte
	etag string
	err  error
}

func (b *cachedBundle) load(build func() ([]byte, error)) ([]byte, string, error) {
	b.once.Do(func() {
		b.body, b.err = build()
		if b.err == nil {
			sum := sha256.Sum256(b.body)
			b.etag = fmt.Sprintf(`"%x"`, sum)
		}
	})
	return b.body, b.etag, b.err
}

func serveCachedBundle(ctx *fasthttp.RequestCtx, body []byte, etag string) {
	ctx.SetContentType("application/gzip")
	ctx.Response.Header.Set("Cache-Control", "public, max-age=300")
	ctx.Response.Header.Set("ETag", etag)
	if string(ctx.Request.Header.Peek("If-None-Match")) == etag {
		ctx.SetStatusCode(fasthttp.StatusNotModified)
		return
	}
	ctx.SetBody(body)
}

// pickBackend chooses where a run goes.
//
// Local first, always: it is the machine with the warm pools and the compile
// cache, and it is already paid for. Rented workers only take traffic the
// local box cannot start immediately — which is exactly the condition that
// caused them to be rented in the first place.
func (s *Server) pickBackend(gpu bool) (string, string, string) {
	if s.scaler == nil {
		return s.cfg.RunnerURL, "", "local"
	}
	kind := scale.CPU
	if gpu {
		kind = scale.GPU
	}
	health, err := s.runner.health()
	if err != nil {
		return s.cfg.RunnerURL, "", "local"
	}
	queued := health.Queue.CPU
	if gpu {
		queued = health.Queue.GPU
	}
	if queued == 0 {
		return s.cfg.RunnerURL, "", "local"
	}
	ready := s.scaler.Ready(kind)
	if len(ready) == 0 {
		return s.cfg.RunnerURL, "", "local"
	}
	// Round robin over ready workers. Nothing cleverer is justified: the local
	// queue depth is the only load signal, and it is already the thing that
	// sent us here.
	n := int(atomic.AddUint64(&s.rrCounter, 1)-1) % len(ready)
	worker := ready[n]
	base := worker.IP
	if !strings.Contains(base, ":") {
		base += ":4342"
	}
	return "http://" + base, s.cfg.RunnerToken, worker.Provider + ":" + worker.ID
}
