package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/valyala/fasthttp"

	"mojojojo/scale"
)

// Placement: deciding which machine should run a program, and saying what it
// will cost before it runs.
//
// The decision itself lives in web/route.js, because the caller's browser has
// to make it too — it is the only participant that knows whether the program
// can run locally for free. This file supplies the two things the browser
// cannot know: what the fleet looks like right now, and what this exact program
// did the last time anybody ran it.
//
// The estimate is the interesting half. "How long will this take" is
// unanswerable in general and trivial in the case that actually matters: the
// same program, run again. Notebooks re-run cells, agents retry, and a
// homepage example is submitted by every visitor — so a table of
// program-hash -> observed milliseconds answers most of the traffic, and
// answers it well enough to choose a tier and quote a price.

// history is what we have seen a program do before.
type history struct {
	Hash   string  `json:"hash"`
	Ms     float64 `json:"ms"`
	Native bool    `json:"native"`
	GPU    bool    `json:"gpu"`
	Runs   int     `json:"seen"`
}

// programHash identifies a program for estimation. Deliberately the exact
// text: a program that differs by a character may have completely different
// costs, and being wrong here is worse than having no estimate at all.
func programHash(code, language string, gpu bool) string {
	h := sha256.New()
	h.Write([]byte(language))
	if gpu {
		h.Write([]byte("|gpu|"))
	}
	h.Write([]byte("|"))
	h.Write([]byte(code))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func createPlanTables(db *sql.DB) error {
	return migrateDatabase(db)
}

// remember records what a program actually cost, for the next caller's
// estimate. Called after a run is billed and never on the request path — a
// failed write here must not fail a run that already happened.
func (s *Server) remember(code string, req RunRequest, result *RunResult) {
	if s.db == nil || result == nil || result.BillableMs <= 0 {
		return
	}
	hash := programHash(code, languageOf(req), req.GPU)
	native := result.Accel.NativeCalls > 0
	// The *last* observation, and the best one. Last is what a caller should
	// expect; best is what the program can do once its kernels are compiled,
	// which is the number that says whether the compile cache has paid off.
	_, _ = s.db.Exec(`
		INSERT INTO seen (hash, ms, best_ms, native, gpu, runs, updated_at)
		VALUES ($1, $2, $3, $4, $5, 1, $6)
		ON CONFLICT(hash) DO UPDATE SET
			ms = EXCLUDED.ms,
			best_ms = LEAST(seen.best_ms, EXCLUDED.ms),
			native = seen.native OR EXCLUDED.native,
			runs = seen.runs + 1,
			updated_at = EXCLUDED.updated_at`,
		hash, result.BillableMs, result.BillableMs, native, req.GPU, time.Now().UTC())
}

func (s *Server) recall(hash string) *history {
	if s.db == nil {
		return nil
	}
	var h history
	var native, gpu bool
	err := s.db.QueryRow(
		`SELECT hash, ms, native, gpu, runs FROM seen WHERE hash = $1`, hash).
		Scan(&h.Hash, &h.Ms, &native, &gpu, &h.Runs)
	if err != nil {
		return nil
	}
	h.Native, h.GPU = native, gpu
	return &h
}

func languageOf(req RunRequest) string {
	if req.Language == "" {
		return "python"
	}
	return req.Language
}

// fleetView is the snapshot web/route.js decides against.
type fleetView struct {
	Edge   tierView `json:"edge"`
	Home   tierView `json:"home"`
	GPU    tierView `json:"gpu"`
	Rented tierView `json:"rented"`
}

type tierView struct {
	Up     bool `json:"up"`
	Queued int  `json:"queued"`
	// Ready is how many rented workers are up right now.
	Ready int `json:"ready,omitempty"`
}

func (s *Server) fleet() fleetView {
	view := fleetView{Edge: tierView{Up: s.cfg.EdgeURL != ""}}
	if health, err := s.runner.health(); err == nil {
		view.Home = tierView{Up: health.OK, Queued: health.Queue.CPU}
		view.GPU = tierView{Up: health.GPUConcurrency > 0, Queued: health.Queue.GPU}
	}
	if s.scaler != nil {
		ready := len(s.scaler.Ready(scale.CPU)) + len(s.scaler.Ready(scale.GPU))
		view.Rented = tierView{Up: ready > 0, Ready: ready}
	}
	return view
}

// PlanResponse answers "where will this run, how long will it take, and what
// will it cost me" without running anything.
type PlanResponse struct {
	Hash string `json:"hash"`
	// Edge is present only when this caller may go straight to the nearest PoP
	// for this program: a URL and a signed ticket good for one run.
	Edge       *edgeGrant `json:"edge,omitempty"`
	Fleet      fleetView  `json:"fleet"`
	Estimate   *history   `json:"estimate,omitempty"`
	Rates      planRates  `json:"rates"`
	Suggestion suggestion `json:"suggestion"`
}

type edgeGrant struct {
	URL    string `json:"url"`
	Ticket string `json:"ticket"`
	MaxMs  int    `json:"max_ms"`
}

type planRates struct {
	MsPerCredit    float64 `json:"ms_per_credit"`
	MsPerCreditGPU float64 `json:"ms_per_credit_gpu"`
	CreditUSD      float64 `json:"credit_usd"`
}

// suggestion is the server's own reading of the same inputs, for callers that
// are not running route.js — the CLI, the SDK, anything with a socket and no
// JavaScript. The browser is expected to decide for itself, because only it
// knows whether it can run the program for free.
type suggestion struct {
	Where      string  `json:"where"`
	Why        string  `json:"why"`
	EstimateMs float64 `json:"estimate_ms,omitempty"`
	PriceUSD   float64 `json:"price_usd,omitempty"`
	Credits    int     `json:"credits,omitempty"`
}

func (s *Server) handlePlan(ctx *fasthttp.RequestCtx) {
	var req RunRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		sendError(ctx, 400, "invalid json body")
		return
	}
	if strings.TrimSpace(req.Code) == "" && len(req.Files) == 0 {
		sendError(ctx, 400, "code is required")
		return
	}
	if len(req.Files) > 0 && languageOf(req) != "mojo" {
		sendError(ctx, 400, "files and entrypoint are only supported for Mojo")
		return
	}
	if len(req.Files) > 0 && strings.TrimSpace(req.Code) != "" {
		sendError(ctx, 400, "send either code or files, not both")
		return
	}
	hash := programHash(requestSource(req), languageOf(req), req.GPU)
	out := PlanResponse{
		Hash:     hash,
		Fleet:    s.fleet(),
		Estimate: s.recall(hash),
		Rates: planRates{
			MsPerCredit:    s.cfg.MsPerCredit,
			MsPerCreditGPU: s.cfg.MsPerCreditGPU,
			CreditUSD:      usdPerCredit,
		},
	}
	out.Suggestion = s.suggest(req, out)
	// A ticket is only worth minting when the edge is where this belongs, and
	// only for somebody we can bill afterwards.
	if user := s.auth.userFromRequest(ctx); user != nil &&
		out.Suggestion.Where == "edge" && s.edgeSuitable(req) {
		if ticket, maxMs, ok := s.mintTicket(user.ID); ok {
			out.Edge = &edgeGrant{URL: s.cfg.EdgeURL, Ticket: ticket, MaxMs: maxMs}
		}
	}
	sendJSON(ctx, out)
}

// suggest mirrors web/route.js closely enough to be useful and is deliberately
// simpler: it never suggests the browser, because a server cannot know whether
// the caller has one.
func (s *Server) suggest(req RunRequest, plan PlanResponse) suggestion {
	est := 0.0
	native := false
	if plan.Estimate != nil {
		est, native = plan.Estimate.Ms, plan.Estimate.Native
	}
	out := suggestion{EstimateMs: est}
	rate := s.cfg.MsPerCredit
	switch {
	case req.GPU:
		out.Where, out.Why = "home", "a GPU was asked for"
		rate = s.cfg.MsPerCreditGPU
	case languageOf(req) != "python":
		out.Where, out.Why = "home", "compiled on our machines"
	case plan.Fleet.Edge.Up && edgeCapable(req) && !native &&
		(est == 0 || est <= edgeMaxMs):
		out.Where, out.Why = "edge", "small, and the nearest PoP is closer than we are"
	case native:
		out.Where, out.Why = "home", "the compiled kernel for this program is already here"
	default:
		out.Where, out.Why = "home", "our box"
	}
	if est > 0 {
		out.PriceUSD = (est / rate) * usdPerCredit
		out.Credits = int(est / rate)
	}
	return out
}

// edgeMaxMs mirrors EDGE_MAX_MS in web/route.js. Above it, the box we already
// own is cheaper than CPU-milliseconds we rent by the million.
const edgeMaxMs = 200

// edgeCapable is the conservative half of route.js's `capable`: the edge has
// the Python standard library and nothing else.
func edgeCapable(req RunRequest) bool {
	if req.GPU || len(req.Packages) > 0 || languageOf(req) != "python" {
		return false
	}
	for _, name := range importedModules(req.Code) {
		if !edgeStdlib[name] {
			return false
		}
	}
	return true
}

// edgeStdlib mirrors EDGE_STDLIB in web/route.js. Listing the whole standard
// library is impractical; listing what a small program actually reaches for is
// the point — anything else goes to a machine that has packages.
var edgeStdlib = map[string]bool{
	"abc": true, "argparse": true, "array": true, "ast": true, "base64": true,
	"binascii": true, "bisect": true, "calendar": true, "cmath": true,
	"collections": true, "colorsys": true, "contextlib": true, "copy": true,
	"csv": true, "dataclasses": true, "datetime": true, "decimal": true,
	"difflib": true, "enum": true, "fnmatch": true, "fractions": true,
	"functools": true, "gzip": true, "hashlib": true, "heapq": true,
	"hmac": true, "html": true, "io": true, "itertools": true, "json": true,
	"math": true, "operator": true, "os": true, "pathlib": true,
	"pickle": true, "pprint": true, "queue": true, "random": true, "re": true,
	"secrets": true, "statistics": true, "string": true, "struct": true,
	"sys": true, "textwrap": true, "time": true, "timeit": true,
	"typing": true, "unicodedata": true, "unittest": true, "urllib": true,
	"uuid": true, "zlib": true,
}

var importRe = regexp.MustCompile(`(?m)^\s*(?:import\s+([A-Za-z_][\w.]*)|from\s+([A-Za-z_][\w.]*)\s+import)`)

// importedModules is the same scan web/route.js does, kept here so the server
// can answer a caller that is not a browser.
func importedModules(code string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range importRe.FindAllStringSubmatch(code, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if i := strings.Index(name, "."); i > 0 {
			name = name[:i]
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// --- placing an actual run -------------------------------------------------

// execute runs a request wherever it belongs and moves it if that turns out to
// be the wrong place.
//
// The rule for moving is the same one the browser uses (`shouldFailOver` in
// web/route.js): we eat our own failures and bill the caller's. A tier that
// refused the work or fell over is our problem and the run moves; a program
// that raised or timed out has been run correctly, and running it again
// somewhere else would just charge for it twice.
func (s *Server) execute(req RunRequest) (*RunResult, string, error) {
	if s.edgeSuitable(req) {
		result, err := s.runAtEdge(req)
		if err == nil && result != nil && !result.RetryElsewhere {
			return result, "edge", nil
		}
		// Never surfaced to the caller: the edge is an optimisation, and the
		// box behind it can do everything the edge can.
		log.Printf("edge declined (%v) — falling back to our own box", err)
	}

	base, token, where := s.pickBackend(req.GPU)
	result, err := s.runner.runAt(base, token, req)
	if err == nil && req.GPU && outOfMemory(result) {
		// The canonical "right program, wrong machine": one GPU, shared, and
		// this program wanted more of it than was left. Rented GPUs are whole
		// cards, so the retry is not hope.
		if alt, altToken, altWhere := s.rentedGPU(); alt != "" {
			log.Printf("gpu out of memory locally — retrying on %s", altWhere)
			if retried, rerr := s.runner.runAt(alt, altToken, req); rerr == nil &&
				!outOfMemory(retried) {
				return retried, altWhere, nil
			}
		}
	}
	return result, where, err
}

// edgeSuitable is the server-side half of the same decision route.js makes:
// small, pure-Python, no packages, nothing we have already compiled a kernel
// for, and an edge that exists.
func (s *Server) edgeSuitable(req RunRequest) bool {
	if s.cfg.EdgeURL == "" || !edgeCapable(req) {
		return false
	}
	seen := s.recall(programHash(requestSource(req), languageOf(req), req.GPU))
	if seen == nil {
		return true // never seen: small enough to find out cheaply
	}
	// A program with a compiled kernel is 6-40x faster on the box that holds
	// the kernel, and a long program costs us more in rented CPU-milliseconds
	// than in electricity.
	return !seen.Native && seen.Ms <= edgeMaxMs
}

func outOfMemory(result *RunResult) bool {
	if result == nil || result.OK {
		return false
	}
	text := result.Stderr + " " + result.Error
	for _, needle := range []string{
		"out of memory", "OutOfMemory", "CUDA error", "failed to allocate",
		"no CUDA-capable", "cudaErrorMemoryAllocation",
	} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// rentedGPU is a rented card, if one is up.
func (s *Server) rentedGPU() (string, string, string) {
	if s.scaler == nil {
		return "", "", ""
	}
	ready := s.scaler.Ready(scale.GPU)
	if len(ready) == 0 {
		return "", "", ""
	}
	worker := ready[0]
	base := worker.IP
	if !strings.Contains(base, ":") {
		base += ":4342"
	}
	return "http://" + base, s.cfg.RunnerToken, "rented:" + worker.ID
}

// runAtEdge sends a run to the Cloudflare Worker in wasmworker/.
//
// The edge speaks the same JSON as the worker on this box, minus everything it
// cannot do, so the only translation needed is noticing when it says "not me".
func (s *Server) runAtEdge(req RunRequest) (*RunResult, error) {
	body, err := json.Marshal(map[string]any{
		"code":       req.Code,
		"stdin":      req.Stdin,
		"args":       req.Args,
		"timeout_ms": req.TimeoutMs,
		"language":   languageOf(req),
	})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest("POST",
		strings.TrimRight(s.cfg.EdgeURL, "/")+"/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if s.cfg.EdgeToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+s.cfg.EdgeToken)
	}
	// Short on purpose: the whole reason to use the edge is that it is quick,
	// so a slow edge is a wrong edge and the local box should have it instead.
	client := &http.Client{Timeout: edgeTimeout(req)}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out RunResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode == 409 || out.RetryElsewhere {
		out.RetryElsewhere = true
		return &out, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("edge: %s", out.Error)
	}
	out.RanOn = "edge"
	return &out, nil
}

func edgeTimeout(req RunRequest) time.Duration {
	ms := req.TimeoutMs
	if ms <= 0 || ms > 30000 {
		ms = 30000
	}
	return time.Duration(ms+2000) * time.Millisecond
}
