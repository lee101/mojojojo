package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// RunRequest is the public API shape. It is also what mojojojo.js posts when
// the browser decides it cannot run something locally.
type RunRequest struct {
	Code string `json:"code"`
	// Files and Entrypoint make a real multi-file Mojo project. Paths must be
	// relative .mojo files; Code remains the convenient single-file form.
	Files      map[string]string `json:"files,omitempty"`
	Entrypoint string            `json:"entrypoint,omitempty"`
	Stdin      string            `json:"stdin,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Packages   []string          `json:"packages,omitempty"`
	TimeoutMs  int               `json:"timeout_ms,omitempty"`
	// Language is "python" (the default) or "mojo", which skips the
	// interpreter entirely: the worker builds the program on the host and runs
	// the binary. Anything else is refused before a slot is taken.
	Language string `json:"language,omitempty"`
	GPU      bool   `json:"gpu,omitempty"`
	// MojoLibs opts Python into compatible libraries backed by our Mojo ports.
	MojoLibs bool  `json:"mojo_libs,omitempty"`
	Accel    *bool `json:"accel,omitempty"`
	// AutoInstall defaults to true: imports are read out of the code and the
	// environment is built to match. Set false to get exactly `Packages`.
	AutoInstall *bool             `json:"auto_install,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	// Source is set by the client for telemetry: "api", "web", "js:fallback".
	Source string `json:"source,omitempty"`
}

// requestSource is the stable content identity used by placement history. A
// change in a helper module must not reuse the timing for an older build.
func requestSource(req RunRequest) string {
	if len(req.Files) == 0 {
		return req.Code
	}
	names := make([]string, 0, len(req.Files))
	for name := range req.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("entrypoint:")
	b.WriteString(req.Entrypoint)
	b.WriteByte('\n')
	if req.Code != "" {
		b.WriteString("code:\n")
		b.WriteString(req.Code)
		b.WriteByte('\n')
	}
	for _, name := range names {
		b.WriteString("file:")
		b.WriteString(name)
		b.WriteByte('\n')
		b.WriteString(req.Files[name])
		b.WriteByte('\n')
	}
	return b.String()
}

// RunResult is what the runner returns, plus the billing block the web tier
// adds. Field names are stable API surface.
type RunResult struct {
	OK       bool    `json:"ok"`
	Stdout   string  `json:"stdout"`
	Stderr   string  `json:"stderr"`
	ExitCode int     `json:"exit_code"`
	TimedOut bool    `json:"timed_out"`
	WallMs   float64 `json:"wall_ms"`
	CPUMs    float64 `json:"cpu_ms"`
	// WarmMs is the unbilled tail after the program's last statement, while a
	// compile finishes. Reported so that a run which answers in 300ms and
	// returns in 8s says which of the two it was.
	WarmMs float64 `json:"warm_ms,omitempty"`
	// BuildMs is a Mojo program's compile, which the caller waits for and is
	// never charged for. Zero once anybody has run that exact source.
	BuildMs     float64  `json:"build_ms,omitempty"`
	BuildCached bool     `json:"build_cached,omitempty"`
	Language    string   `json:"language,omitempty"`
	BillableMs  float64  `json:"billable_ms"`
	QueueMs     float64  `json:"queue_ms"`
	Accelerated []string `json:"accelerated"`
	Accel       struct {
		Compiled    int     `json:"compiled"`
		NativeCalls int     `json:"native_calls"`
		CacheHits   int     `json:"cache_hits"`
		CompileMs   float64 `json:"compile_ms"`
		// Queued means the host is compiling those kernels now; the next run
		// of the same code dispatches them natively from its first call.
		Queued int    `json:"queued,omitempty"`
		Error  string `json:"error,omitempty"`
	} `json:"accel"`
	Env struct {
		Key        string   `json:"key"`
		Cached     bool     `json:"cached"`
		BuildMs    float64  `json:"build_ms"`
		Packages   []string `json:"packages"`
		Auto       []string `json:"auto"`
		AutoFailed []string `json:"auto_failed,omitempty"`
	} `json:"env"`
	// WarmStart reports that this ran on a pre-warmed interpreter rather than
	// paying cold start. Latency only — it changes no billing.
	WarmStart bool    `json:"warm_start"`
	Isolated  bool    `json:"isolated"`
	Error     string  `json:"error,omitempty"`
	Charge    *charge `json:"charge,omitempty"`
	// Welcome is set once, on the run that triggered the signup grant.
	Welcome int `json:"welcome_credits,omitempty"`
	// RanOn names the machine when a run did not go to our own box: a rented
	// worker, or "edge".
	RanOn string `json:"ran_on,omitempty"`
	// RetryElsewhere is a tier saying "not me" — the edge refuses a GPU run, a
	// package, or Mojo. It is never a failure and never reaches the caller.
	RetryElsewhere bool `json:"retry_elsewhere,omitempty"`
}

type runnerClient struct {
	base  string
	token string
	http  *http.Client
}

func newRunnerClient(base string) *runnerClient {
	return &runnerClient{
		base: base,
		// The runner enforces its own deadlines; this is only a backstop for a
		// runner that has stopped answering entirely.
		http: &http.Client{Timeout: 15 * time.Minute},
	}
}

// run sends a job to a backend: the local worker, or a rented one when the
// local queue is full. Same protocol either way — a rented worker is the same
// program with a token in front of it.
func (c *runnerClient) runAt(base, token string, req RunRequest) (*RunResult, error) {
	saved, savedToken := c.base, c.token
	c.base, c.token = base, token
	defer func() { c.base, c.token = saved, savedToken }()
	return c.run(req)
}

func (c *runnerClient) run(req RunRequest) (*RunResult, error) {
	body, err := json.Marshal(map[string]any{
		"code":         req.Code,
		"files":        req.Files,
		"entrypoint":   req.Entrypoint,
		"stdin":        req.Stdin,
		"args":         req.Args,
		"packages":     req.Packages,
		"timeout_ms":   req.TimeoutMs,
		"language":     req.Language,
		"gpu":          req.GPU,
		"mojo_libs":    req.MojoLibs,
		"accel":        req.Accel == nil || *req.Accel,
		"auto_install": req.AutoInstall == nil || *req.AutoInstall,
		"env":          req.Env,
	})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest("POST", c.base+"/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("runner unreachable: %w", err)
	}
	defer resp.Body.Close()

	var out RunResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("runner returned unreadable response: %w", err)
	}
	if out.Error != "" {
		return &out, errors.New(out.Error)
	}
	return &out, nil
}

type runnerHealth struct {
	OK          bool `json:"ok"`
	Isolated    bool `json:"isolated"`
	Concurrency int  `json:"concurrency"`
	Queue       struct {
		CPU  int     `json:"cpu"`
		GPU  int     `json:"gpu"`
		ForS float64 `json:"for_s"`
	} `json:"queue"`
	Accel struct {
		Available bool   `json:"available"`
		Mojo      string `json:"mojo"`
		Reason    string `json:"reason"`
	} `json:"accel"`
	Stats struct {
		Runs   int     `json:"runs"`
		Errors int     `json:"errors"`
		Ms     float64 `json:"ms"`
	} `json:"stats"`
	GPUConcurrency int `json:"gpu_concurrency"`
	// Passed through as-is for observability: the worker owns these shapes.
	Pool     map[string]any `json:"pool"`
	Cache    map[string]any `json:"cache"`
	MojoLibs map[string]any `json:"mojo_libs"`
}

func (c *runnerClient) health() (*runnerHealth, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(c.base + "/health")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out runnerHealth
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
