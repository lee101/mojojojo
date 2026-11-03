package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

type howFixture struct {
	s    *Server
	key  string
	user *User
	conn *fasthttputil.InmemoryListener
}

func newHowFixture(t *testing.T, upstreamURL string, credits int) *howFixture {
	t.Helper()
	t.Setenv("MOJOJOJO_UPSTREAM_HOW", upstreamURL)
	s := authTestServer(t)
	s.cfg.WelcomeCredits = 0
	s.cfg.CloudToken = "tok"
	u, err := s.auth.createUser("how@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET free_credits=0, plan_credits=0, paid_credits=$2 WHERE id=$1`, u.ID, credits); err != nil {
		t.Fatal(err)
	}
	key, err := s.auth.createKey(u.ID, "how")
	if err != nil {
		t.Fatal(err)
	}
	s.bill, err = newBilling(s.db, s.auth, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.meters = newMeterBuffer()
	ln := fasthttputil.NewInmemoryListener()
	go fasthttp.Serve(ln, s.route)
	t.Cleanup(func() { ln.Close() })
	return &howFixture{s: s, key: key.Secret, user: u, conn: ln}
}

func (f *howFixture) post(t *testing.T, path, body string, headers map[string]string) (*bufio.Reader, net.Conn, string) {
	t.Helper()
	c, err := f.conn.Dial()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\nHost: how.%s\r\nContent-Length: %d\r\nContent-Type: application/json\r\nX-Forwarded-Proto: https\r\n", path, f.s.cfg.BusinessDomain, len(body))
	if f.key != "" {
		fmt.Fprintf(&b, "Authorization: Bearer %s\r\n", f.key)
	}
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n" + body)
	if _, err := c.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	status, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return r, c, status
}

func skipHeaders(t *testing.T, r *bufio.Reader) {
	t.Helper()
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "\r\n" {
			return
		}
	}
}

func TestHowStreamsIncrementallyAndMetersTokens(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: step\ndata: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "event: done\ndata: {\"steps\":1,\"prompt_tokens\":1500,\"completion_tokens\":500,\"web_searches\":2}\n\n")
	}))
	defer up.Close()
	f := newHowFixture(t, up.URL, 100)
	r, c, status := f.post(t, "/v1/ask", `{"q":"x"}`, map[string]string{
		"X-Mojo-Tenant": "evil", "X-Mojo-Token": "evil", "Cookie": "session=secret", "X-Api-Key": "k"})
	defer c.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("status %q", status)
	}
	skipHeaders(t, r)
	got := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 256)
		n, _ := r.Read(buf)
		sb.Write(buf[:n])
		got <- sb.String()
	}()
	select {
	case first := <-got:
		if !strings.Contains(first, "step") {
			t.Fatalf("first chunk %q", first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no chunk before upstream finished: response is buffered")
	}
	close(release)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var all strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(all.String(), "prompt_tokens") {
		n, err := r.Read(buf)
		all.Write(buf[:n])
		if err != nil {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if seen.Get("Authorization") != "" || seen.Get("X-Api-Key") != "" || seen.Get("Cookie") != "" {
		t.Errorf("credentials leaked upstream: %v", seen)
	}
	if seen.Get("X-Mojo-Tenant") != f.user.ID || seen.Get("X-Mojo-Business") != "how" || seen.Get("X-Mojo-Token") != "tok" {
		t.Errorf("gateway headers wrong: %v", seen)
	}
	drained := f.s.meters.drain()
	if drained[meterKey{f.user.ID, "how.ask"}] != 1 {
		t.Errorf("ask meter: %v", drained)
	}
	if drained[meterKey{f.user.ID, "how.tokens"}] != 2 {
		t.Errorf("token meter: %v", drained)
	}
	if drained[meterKey{f.user.ID, "how.websearch"}] != 2 {
		t.Errorf("websearch meter: %v", drained)
	}
}

func TestHowRequiresKeyAndBlocksWhenExhausted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	f := newHowFixture(t, up.URL, 0)
	k := f.key
	f.key = ""
	_, c, status := f.post(t, "/v1/ask", `{}`, nil)
	c.Close()
	if !strings.Contains(status, "401") {
		t.Errorf("no key: %q", status)
	}
	f.key = k
	if _, err := f.s.db.Exec(`INSERT INTO usage_meters(user_id,meter,month,qty,owed_micro,updated_at) VALUES($1,'how.ask',$2,50,0,now())`,
		f.user.ID, meterMonth(time.Now())); err != nil {
		t.Fatal(err)
	}
	_, c, status = f.post(t, "/v1/ask", `{}`, nil)
	c.Close()
	if !strings.Contains(status, "402") {
		t.Errorf("exhausted: %q", status)
	}
}

func TestHowHealthAndLandingArePublic(t *testing.T) {
	t.Setenv("MOJOJOJO_UPSTREAM_HOW", "http://127.0.0.1:1")
	s := authTestServer(t)
	for _, path := range []string{"/health", "/"} {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI(path)
		ctx.Request.Header.SetHost("how." + s.cfg.BusinessDomain)
		s.route(ctx)
		if ctx.Response.StatusCode() != 200 {
			t.Errorf("%s = %d", path, ctx.Response.StatusCode())
		}
	}
}

func TestHowRequestMeters(t *testing.T) {
	_, by := loadBusinesses()
	p := by["how"].Parts
	if requestMeter(p, "POST", "/v1/ask") != "how.ask" || requestMeter(p, "GET", "/v1/search") != "how.search" || requestMeter(p, "GET", "/v1/other") != "" {
		t.Error("how meter routing")
	}
}

func TestOtherBusinessesKeepCloudUpstream(t *testing.T) {
	t.Setenv("MOJOCLOUD_URL", "http://cloud.example:1")
	s := &Server{cfg: loadConfig()}
	_, by := loadBusinesses()
	up := s.upstreamFor(by["kv"])
	if up.own || up.base != "http://cloud.example:1" {
		t.Errorf("kv upstream: %+v", up)
	}
	if h := s.upstreamFor(by["how"]); !h.own || h.base != "http://127.0.0.1:4350" {
		t.Errorf("how upstream: %+v", h)
	}
}
