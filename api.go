package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// handleRun is the whole product: take Python, return what it printed, charge
// for the milliseconds it actually used.
//
// Order matters. Auth and the balance check happen before the code runs, so a
// user with no credits is told so instead of being given free compute; the
// charge happens after, because until the program has run nobody knows what it
// cost. A run that fails still costs — it still used the machine — but a run
// rejected before it started never does.
func (s *Server) handleRun(ctx *fasthttp.RequestCtx) {
	var req RunRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		sendError(ctx, 400, "invalid json body")
		return
	}
	if strings.TrimSpace(req.Code) == "" && len(req.Files) == 0 {
		sendError(ctx, 400, "code is required")
		return
	}
	if req.TimeoutMs <= 0 {
		req.TimeoutMs = s.cfg.DefaultTimeoutMs
	}
	if req.TimeoutMs > s.cfg.MaxTimeoutMs {
		req.TimeoutMs = s.cfg.MaxTimeoutMs
	}
	// Refused here rather than passed through: an unknown language reaching the
	// worker would be answered with a 400 anyway, after taking a slot.
	switch req.Language {
	case "", "python":
		req.Language = "python"
	case "mojo":
	default:
		sendError(ctx, 400, "language must be python or mojo")
		return
	}
	if len(req.Files) > 0 && req.Language != "mojo" {
		sendError(ctx, 400, "files and entrypoint are only supported for Mojo")
		return
	}
	if len(req.Files) > 0 && strings.TrimSpace(req.Code) != "" {
		sendError(ctx, 400, "send either code or files, not both")
		return
	}

	user := s.auth.userFromRequest(ctx)
	source := req.Source
	if source == "" {
		source = "api"
	}

	if user == nil && bearerToken(ctx) != "" {
		// Presenting a key that does not work must say so. Falling through to
		// the anonymous free tier would make a revoked or mistyped key look
		// like it was working until the day the free budget ran out.
		sendError(ctx, 401, "invalid or revoked API key")
		return
	}

	if user == nil {
		// No account, no server-side compute. An unauthenticated execution
		// endpoint is a free botnet with a nice API, and the free credits
		// below would be free credits per *request* rather than per person.
		// The browser runtime stays free and unlimited: mojojojo.js runs the
		// same Python locally, which is genuinely nobody's cost but the
		// caller's.
		if !s.cfg.AllowAnonymous {
			sendError(ctx, 401, "sign in with your mojojojo account and add credits to "+
				"run on our machines: "+s.cfg.LoginURL+" — mojojojo.js runs Python "+
				"in your browser for nothing")
			return
		}
		if !s.free.allow(clientIP(ctx)) {
			sendError(ctx, 402, "free daily compute used up — sign in at "+s.cfg.LoginURL)
			return
		}
		if req.GPU || len(req.Packages) > 0 {
			sendError(ctx, 401, "GPU runs and extra packages need an account: "+s.cfg.LoginURL)
			return
		}
		if req.TimeoutMs > s.cfg.FreeTimeoutMs {
			req.TimeoutMs = s.cfg.FreeTimeoutMs
		}
		result, err := s.runner.run(req)
		if err != nil {
			s.runFailed(ctx, result, err)
			return
		}
		s.free.spend(clientIP(ctx), result.BillableMs)
		result.Charge = &charge{BilledMs: result.BillableMs, Credits: 0,
			Balance: 0, MsPerCredi: s.cfg.MsPerCredit}
		sendJSON(ctx, result)
		return
	}

	// The welcome grant lands on first use, but only money the account put in
	// unlocks a run.
	granted, gotWelcome := s.bill.grantWelcome(user)

	if !user.Funded() {
		sendError(ctx, 402, paymentRequired+s.cfg.AccountURL)
		return
	}

	result, where, err := s.execute(req)
	if err != nil {
		s.runFailed(ctx, result, err)
		return
	}
	if where != "local" {
		// Say so in the response: a run on rented capacity is the same price,
		// but it is a different machine and the caller should be able to see
		// that from the outside.
		result.RanOn = where
		source += ":overflow"
	}

	native := result.Accel.NativeCalls > 0
	ch, err := s.bill.charge(user, result.BillableMs, req.GPU, native, source)
	if err != nil {
		// The compute is already spent. Log loudly, hand back the result, and
		// do not invent a charge we failed to record.
		log.Printf("charge failed user=%s ms=%.1f: %v", user.ID, result.BillableMs, err)
	} else {
		result.Charge = &ch
	}
	if gotWelcome {
		result.Welcome = granted
	}
	// What this program actually cost, for the next caller's estimate.
	s.remember(requestSource(req), req, result)
	sendJSON(ctx, result)
}

const paymentRequired = "add credits to run on our machines — top up at "

// paying is the gate in front of every path that starts or schedules compute:
// the account must have put money in. The welcome grant still lands, but a
// grant alone never unlocks our machines.
func (s *Server) paying(ctx *fasthttp.RequestCtx, u *User) (*User, bool) {
	if u != nil && s.bill != nil {
		s.bill.grantWelcome(u)
		u = s.auth.userByID(u.ID)
	}
	if !u.Funded() {
		sendError(ctx, 402, paymentRequired+s.cfg.AccountURL)
		return nil, false
	}
	return u, true
}

func (s *Server) runFailed(ctx *fasthttp.RequestCtx, result *RunResult, err error) {
	status := 502
	msg := err.Error()
	if result != nil && result.Error != "" {
		// The runner rejected the request itself (bad package spec, oversized
		// code): that is the caller's fault, not ours.
		status = 400
		msg = result.Error
	}
	log.Printf("run failed: %v", err)
	sendError(ctx, status, msg)
}

// --- API keys ---

func (s *Server) handleKeysList(ctx *fasthttp.RequestCtx) {
	u := s.auth.userFromSession(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	keys, err := s.auth.listKeys(u.ID)
	if err != nil {
		sendError(ctx, 500, err.Error())
		return
	}
	sendJSON(ctx, map[string]any{"keys": keys})
}

func (s *Server) handleKeyCreate(ctx *fasthttp.RequestCtx) {
	// Session only, never an API key: a key must not be able to mint another.
	u := s.auth.userFromSession(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(ctx.PostBody(), &req)
	if req.Name == "" {
		req.Name = string(ctx.FormValue("name"))
	}
	key, err := s.auth.createKey(u.ID, req.Name)
	if err != nil {
		sendError(ctx, 500, err.Error())
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "key": key.Secret, "id": key.ID,
		"name": key.Name, "note": "shown once — store it now"})
}

func (s *Server) handleKeyRevoke(ctx *fasthttp.RequestCtx) {
	u := s.auth.userFromSession(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(ctx.PostBody(), &req)
	if req.ID == "" {
		req.ID = string(ctx.FormValue("id"))
	}
	if err := s.auth.revokeKey(u.ID, req.ID); err != nil {
		sendError(ctx, 500, err.Error())
		return
	}
	sendJSON(ctx, map[string]any{"ok": true})
}

// --- free tier ---

// freeTier hands anonymous callers a daily millisecond budget per address so
// the homepage demo runs for real. In memory on purpose: it resets on deploy,
// which is the right failure direction for a giveaway, and it costs nothing.
type freeTier struct {
	perDay float64
	mu     sync.Mutex
	used   map[string]*freeUse
}

type freeUse struct {
	ms    float64
	day   string
	seen  time.Time
	burst int
}

func newFreeTier(perDay float64) *freeTier {
	return &freeTier{perDay: perDay, used: map[string]*freeUse{}}
}

func (f *freeTier) allow(ip string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweep()
	day := time.Now().UTC().Format("2006-01-02")
	u, ok := f.used[ip]
	if !ok || u.day != day {
		u = &freeUse{day: day}
		f.used[ip] = u
	}
	u.seen = time.Now()
	return u.ms < f.perDay
}

func (f *freeTier) spend(ip string, ms float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.used[ip]; ok {
		u.ms += ms
	}
}

// sweep drops addresses nobody has used for a day. Called under the lock.
func (f *freeTier) sweep() {
	if len(f.used) < 10000 {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for ip, u := range f.used {
		if u.seen.Before(cutoff) {
			delete(f.used, ip)
		}
	}
}

// clientIP identifies the caller for the anonymous free tier.
//
// X-Real-IP only, and only because nginx overwrites it on every request —
// X-Forwarded-For is appended to rather than replaced, so trusting it would let
// anyone mint a fresh free-tier identity per request by sending their own
// header. When there is no proxy header, the socket peer is the truth.
func clientIP(ctx *fasthttp.RequestCtx) string {
	if v := strings.TrimSpace(string(ctx.Request.Header.Peek("X-Real-IP"))); v != "" {
		return v
	}
	return ctx.RemoteIP().String()
}
