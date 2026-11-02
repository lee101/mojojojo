package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

const agentTestSession = "agent-test-session"

func testAgentAuth(t *testing.T) *authStore {
	t.Helper()
	db := testPostgres(t)
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO users (id,email,password_hash,created_at,paid_credits) VALUES ('agent-user','a@b.c','x',$1,10)`,
		now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO sessions (id_hash,user_id,created_at,expires_at,last_seen_at) VALUES ($1,$2,$3,$4,$3)`,
		hashToken(agentTestSession), "agent-user", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return newAuthStore(db)
}

func agentRequest(method, path, body string, authenticated bool) *fasthttp.RequestCtx {
	var req fasthttp.Request
	req.Header.SetMethod(method)
	req.SetRequestURI(path)
	if body != "" {
		req.Header.SetContentType("application/json")
		req.SetBodyString(body)
	}
	if authenticated {
		req.Header.SetCookie(sessionCookie, agentTestSession)
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, nil, nil)
	return ctx
}

func TestAgentProxyRefusesAnonymous(t *testing.T) {
	server := &Server{
		cfg:   Config{LoginURL: "https://mojojojo.cc/auth/login", AgentToken: "agent-service-secret", AgentTokensPerCredit: 100},
		auth:  testAgentAuth(t),
		agent: newAgentProxy("http://127.0.0.1:1", 2),
	}
	ctx := agentRequest(http.MethodPost, "/v1/agent/runs", `{"prompt":"hello"}`, false)
	server.handleAgentProxy(ctx)
	if got := ctx.Response.StatusCode(); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got)
	}
	if !strings.Contains(string(ctx.Response.Body()), "sign in") {
		t.Fatalf("response does not explain authentication: %s", ctx.Response.Body())
	}
}

func TestAgentProxyStreamsSSE(t *testing.T) {
	const stream = "event: run\ndata: {\"id\":\"run-1\"}\n\n" +
		"event: text\ndata: {\"kind\":\"text\",\"text\":\"hello\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/runs" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if _, err := r.Cookie(sessionCookie); err == nil {
			t.Errorf("browser session cookie was forwarded to shared backend")
		}
		if r.Header.Get("Authorization") != "Bearer agent-service-secret" || r.Header.Get("X-Mojojojo-User") != "agent-user" {
			t.Errorf("internal identity contract missing: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("X-Run-ID", "run-1")
		flusher := w.(http.Flusher)
		for _, event := range strings.SplitAfter(stream, "\n\n") {
			_, _ = w.Write([]byte(event))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	server := &Server{
		cfg:   Config{LoginURL: "https://mojojojo.cc/auth/login", AgentToken: "agent-service-secret", AgentTokensPerCredit: 100},
		auth:  testAgentAuth(t),
		agent: newAgentProxy(upstream.URL, 2),
	}
	ctx := agentRequest(http.MethodPost, "/v1/agent/runs", `{"prompt":"hello"}`, true)
	server.handleAgentProxy(ctx)
	if got := ctx.Response.StatusCode(); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := string(ctx.Response.Header.Peek("X-Run-ID")); got != "run-1" {
		t.Fatalf("X-Run-ID = %q", got)
	}
	if got := string(ctx.Response.Body()); got != stream {
		t.Fatalf("stream changed:\n got %q\nwant %q", got, stream)
	}
}

func TestAgentProxyCancelsUpstreamWhenClientDisconnects(t *testing.T) {
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(cancelled)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Run-ID", "run-disconnect")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("event: run\ndata: {\"id\":\"run-disconnect\"}\n\n"))
		flusher.Flush()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte(": keep-alive\n\n"))
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	app := &Server{
		cfg:   Config{LoginURL: "https://mojojojo.cc/auth/login", AgentToken: "agent-service-secret", AgentTokensPerCredit: 100},
		auth:  testAgentAuth(t),
		agent: newAgentProxy(upstream.URL, 2),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	web := &fasthttp.Server{Handler: app.route}
	go func() { _ = web.Serve(listener) }()
	t.Cleanup(func() {
		_ = web.Shutdown()
		_ = listener.Close()
	})

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	body := `{"prompt":"wait"}`
	_, _ = fmt.Fprintf(conn, "POST /v1/agent/runs HTTP/1.1\r\nHost: test\r\n"+
		"Cookie: %s=%s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		sessionCookie, agentTestSession, len(body), body)
	reader := bufio.NewReader(conn)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read proxy stream: %v", readErr)
		}
		if strings.Contains(line, "run-disconnect") {
			break
		}
	}
	_ = conn.Close()

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not cancelled after the client disconnected")
	}
}

func TestAgentSettlementAccruesChargesAndIsIdempotent(t *testing.T) {
	s := authTestServer(t)
	s.cfg.AgentToken = "internal-agent-secret"
	s.cfg.AgentTokensPerCredit = 100
	bill, err := newBilling(s.db, s.auth, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.bill = bill
	user, err := s.auth.createUser("agent-billing@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET free_credits=10 WHERE id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}

	settle := func(secret, runID string, tokens int) *fasthttp.RequestCtx {
		body, _ := json.Marshal(map[string]any{
			"user_id": user.ID, "run_id": runID, "model": "gpt-test", "tokens": tokens,
		})
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(http.MethodPost)
		ctx.Request.SetRequestURI("/internal/agent/settle")
		ctx.Request.Header.SetContentType("application/json")
		ctx.Request.Header.Set("Authorization", "Bearer "+secret)
		ctx.Request.SetBody(body)
		s.route(ctx)
		return ctx
	}
	if unauthorized := settle("wrong", "run-unauthorized", 40); unauthorized.Response.StatusCode() != 401 {
		t.Fatalf("unauthorized settlement = %d", unauthorized.Response.StatusCode())
	}
	first := settle(s.cfg.AgentToken, "run-first", 40)
	if first.Response.StatusCode() != 200 || !strings.Contains(string(first.Response.Body()), `"owed_credits":0.4`) {
		t.Fatalf("first settlement: %d %s", first.Response.StatusCode(), first.Response.Body())
	}
	second := settle(s.cfg.AgentToken, "run-second", 60)
	if second.Response.StatusCode() != 200 || !strings.Contains(string(second.Response.Body()), `"charged_credits":1`) {
		t.Fatalf("second settlement: %d %s", second.Response.StatusCode(), second.Response.Body())
	}
	duplicate := settle(s.cfg.AgentToken, "run-second", 60)
	if duplicate.Response.StatusCode() != 200 {
		t.Fatalf("duplicate settlement: %d %s", duplicate.Response.StatusCode(), duplicate.Response.Body())
	}
	var usage, debits, balance int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM agent_usage WHERE user_id=$1`, user.ID).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM credit_ledger WHERE user_id=$1 AND reason LIKE 'agent:%'`, user.ID).Scan(&debits); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT free_credits+plan_credits+paid_credits FROM users WHERE id=$1`, user.ID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if usage != 2 || debits != 1 || balance != 9 {
		t.Fatalf("usage=%d debits=%d balance=%d", usage, debits, balance)
	}
}

func TestConcurrentAgentRunsShareOneReservation(t *testing.T) {
	proxy := newAgentProxy("http://127.0.0.1:1", 2)
	if budget, ok := proxy.acquire("u", 1000); !ok || budget != 1000 {
		t.Fatalf("first run: %d %v", budget, ok)
	}
	if budget, ok := proxy.acquire("u", 1000); !ok || budget != 0 {
		t.Fatalf("second run reserved %d of an already reserved balance", budget)
	}
	proxy.release("u", 0)
	proxy.release("u", 1000)
	if budget, ok := proxy.acquire("u", 1000); !ok || budget != 1000 {
		t.Fatalf("after release: %d %v", budget, ok)
	}
}
