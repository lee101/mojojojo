package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func gateServer(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	s := authTestServer(t)
	var runs atomic.Int64
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"stdout":"hi\n","billable_ms":5}`))
	}))
	t.Cleanup(runner.Close)
	s.cfg.RunnerURL = runner.URL
	s.cfg.EdgeURL = ""
	s.cfg.WelcomeCredits = 1000
	s.cfg.MsPerCredit, s.cfg.MsPerCreditGPU = 1000, 100
	s.runner = newRunnerClient(runner.URL)
	bill, err := newBilling(s.db, s.auth, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.bill = bill
	return s, &runs
}

func gateUser(t *testing.T, s *Server, email string, free, paid int) (*User, string) {
	t.Helper()
	u, err := s.auth.createUser(email, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET free_credits=$2, paid_credits=$3 WHERE id=$1`,
		u.ID, free, paid); err != nil {
		t.Fatal(err)
	}
	key, err := s.auth.createKey(u.ID, "gate")
	if err != nil {
		t.Fatal(err)
	}
	return u, key.Secret
}

func gateRequest(s *Server, key, method, path, body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.Set("Authorization", "Bearer "+key)
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBodyString(body)
	s.route(ctx)
	return ctx
}

// A grant is not money: a brand new account, or one holding only its welcome
// credits, is refused before anything reaches a runner.
func TestRunNeedsPaidCredits(t *testing.T) {
	s, runs := gateServer(t)
	_, broke := gateUser(t, s, "broke@example.com", 0, 0)
	_, granted := gateUser(t, s, "granted@example.com", 5000, 0)
	_, paid := gateUser(t, s, "paid@example.com", 0, 100)

	for _, key := range []string{broke, granted} {
		ctx := gateRequest(s, key, "POST", "/v1/run", `{"code":"print(1)"}`)
		if ctx.Response.StatusCode() != 402 {
			t.Fatalf("unfunded run: %d %s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
	}
	if runs.Load() != 0 {
		t.Fatalf("%d runs reached the runner without payment", runs.Load())
	}
	ctx := gateRequest(s, paid, "POST", "/v1/run", `{"code":"print(1)"}`)
	if ctx.Response.StatusCode() != 200 || runs.Load() != 1 {
		t.Fatalf("funded run: %d %s runs=%d", ctx.Response.StatusCode(), ctx.Response.Body(), runs.Load())
	}
}

func TestAnActivePlanIsFunded(t *testing.T) {
	s, runs := gateServer(t)
	u, key := gateUser(t, s, "plan@example.com", 0, 0)
	if _, err := s.db.Exec(`UPDATE users SET plan_credits=100, plan_credits_expire_at=$2 WHERE id=$1`,
		u.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ctx := gateRequest(s, key, "POST", "/v1/run", `{"code":"print(1)"}`); ctx.Response.StatusCode() != 402 {
		t.Fatalf("expired plan: %d", ctx.Response.StatusCode())
	}
	if _, err := s.db.Exec(`UPDATE users SET plan_credits_expire_at=$2 WHERE id=$1`,
		u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ctx := gateRequest(s, key, "POST", "/v1/run", `{"code":"print(1)"}`); ctx.Response.StatusCode() != 200 || runs.Load() != 1 {
		t.Fatalf("active plan: %d %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}

func TestEveryComputePathRefusesAnUnfundedAccount(t *testing.T) {
	s, runs := gateServer(t)
	s.cfg.AgentToken = "agent-service-secret"
	var upstream atomic.Int64
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(agent.Close)
	s.agent = newAgentProxy(agent.URL, 2)
	u, key := gateUser(t, s, "free@example.com", 5000, 0)
	now := time.Now().UTC()
	if _, err := s.db.Exec(`INSERT INTO projects (id,user_id,name,slug,created_at,updated_at)
		VALUES ('proj','`+u.ID+`','p','p',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"index.html":    "<p>hi</p>",
		"mojojojo.json": `{"runtime":"app","functions":{"hello":{"language":"python","entrypoint":"hello.py"}}}`,
		"hello.py":      "def handler(request):\n    return 'hi'\n",
	} {
		if _, err := s.db.Exec(`INSERT INTO project_files (project_id,path,content,updated_at)
			VALUES ('proj',$1,$2,$3)`, name, content, now); err != nil {
			t.Fatal(err)
		}
	}
	vec := "vec_gate"
	if _, err := s.db.Exec(`INSERT INTO vector_projects (id,user_id,name,slug,tier,created_at,updated_at)
		VALUES ($1,$2,'v','v','exact',$3,$3)`, vec, u.ID, now); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/run", `{"code":"print(1)"}`},
		{"POST", "/v1/agent/runs", `{"prompt":"hi"}`},
		{"POST", "/v1/agent/runs/run-1/steer", `{"prompt":"more"}`},
		{"POST", "/v1/projects/proj/preview/functions/hello", `{}`},
		{"POST", "/v1/projects/proj/publish", ``},
		{"POST", "/v1/deployments", ``},
		{"POST", "/v1/vector/projects/" + vec + "/upsert", `{"documents":[{"id":"a","text":"a"}]}`},
		{"POST", "/v1/vector/projects/" + vec + "/search", `{"query":"a"}`},
		{"POST", "/v1/vector/reservations", `{}`},
	} {
		ctx := gateRequest(s, key, c.method, c.path, c.body)
		if ctx.Response.StatusCode() != 402 {
			t.Errorf("%s %s: %d %s", c.method, c.path, ctx.Response.StatusCode(), ctx.Response.Body())
		}
	}
	if runs.Load() != 0 || upstream.Load() != 0 {
		t.Fatalf("unfunded work escaped: runner=%d agent=%d", runs.Load(), upstream.Load())
	}
	// Stopping a run must never need money.
	if ctx := gateRequest(s, key, "POST", "/v1/agent/runs/run-1/interrupt", ``); ctx.Response.StatusCode() == 402 {
		t.Fatal("interrupt was refused for want of credits")
	}
	if upstream.Load() != 1 {
		t.Fatalf("interrupt did not reach the agent: %d", upstream.Load())
	}

	if _, err := s.db.Exec(`UPDATE users SET paid_credits=100 WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	ctx := gateRequest(s, key, "POST", "/v1/projects/proj/preview/functions/hello", `{}`)
	if ctx.Response.StatusCode() != 200 || runs.Load() != 1 {
		t.Fatalf("funded preview: %d %s runs=%d", ctx.Response.StatusCode(), ctx.Response.Body(), runs.Load())
	}
	if ctx := gateRequest(s, key, "POST", "/v1/agent/runs", `{"prompt":"hi"}`); ctx.Response.StatusCode() != 200 {
		t.Fatalf("funded agent run: %d %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}

func TestEdgeTicketsNeedPaidCredits(t *testing.T) {
	s := ticketServer(t)
	if _, err := s.db.Exec(`INSERT INTO users (id, email, password_hash, created_at, free_credits)
		VALUES ('user-free', 'free@example.com', 'x', $1, 5000)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.mintTicket("user-free"); ok {
		t.Fatal("minted an edge ticket for an account that has never paid")
	}
	if _, _, ok := s.mintTicket("user-1"); !ok {
		t.Fatal("refused a funded account its edge ticket")
	}
}

// Renewal is the scheduled job: it runs with nobody watching, so it checks
// again and pauses rather than spending a grant on reserved capacity.
func TestScheduledRenewalSkipsUnfundedAccounts(t *testing.T) {
	s, _ := gateServer(t)
	broke, _ := gateUser(t, s, "renew-free@example.com", 5000, 0)
	paid, _ := gateUser(t, s, "renew-paid@example.com", 0, 5000)
	now := time.Now().UTC()
	for _, id := range []string{broke.ID, paid.ID} {
		if _, err := s.db.Exec(`INSERT INTO capacity_reservations
			(id,user_id,product,status,credits_per_hour,current_period_start,next_charge_at,created_at,updated_at)
			VALUES ($1,$1,$2,'active',1500,$3,$3,$3,$3)`, id, vectorCapacityProduct, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.settleCapacityReservation(id, now); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	var free int
	if err := s.db.QueryRow(`SELECT r.status, u.free_credits FROM capacity_reservations r
		JOIN users u ON u.id=r.user_id WHERE r.id=$1`, broke.ID).Scan(&status, &free); err != nil {
		t.Fatal(err)
	}
	if status != "paused" || free != 5000 {
		t.Fatalf("unfunded renewal: status=%s free=%d", status, free)
	}
	var paidLeft int
	if err := s.db.QueryRow(`SELECT r.status, u.paid_credits FROM capacity_reservations r
		JOIN users u ON u.id=r.user_id WHERE r.id=$1`, paid.ID).Scan(&status, &paidLeft); err != nil {
		t.Fatal(err)
	}
	if status != "active" || paidLeft != 3500 {
		t.Fatalf("funded renewal: status=%s paid=%d", status, paidLeft)
	}
}
