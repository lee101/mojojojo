package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	stripe "github.com/stripe/stripe-go/v86"
	"github.com/valyala/fasthttp"
)

type fakeGateway struct {
	mu       sync.Mutex
	sessions []*stripe.CheckoutSessionCreateParams
}

func (g *fakeGateway) CreateCustomer(_ context.Context, p *stripe.CustomerCreateParams) (*stripe.Customer, error) {
	return &stripe.Customer{ID: "cus_fake"}, nil
}
func (g *fakeGateway) CreateCheckoutSession(_ context.Context, p *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions = append(g.sessions, p)
	return &stripe.CheckoutSession{ID: "cs_fake_" + string(rune('a'+len(g.sessions))), ClientSecret: "cs_secret_fake"}, nil
}
func (g *fakeGateway) RetrieveCheckoutSession(context.Context, string) (*stripe.CheckoutSession, error) {
	return nil, nil
}
func (g *fakeGateway) CreatePortalSession(context.Context, *stripe.BillingPortalSessionCreateParams) (*stripe.BillingPortalSession, error) {
	return nil, nil
}
func (g *fakeGateway) RetrieveSubscription(context.Context, string) (*stripe.Subscription, error) {
	return nil, nil
}
func (g *fakeGateway) ConstructEvent([]byte, string, string) (stripe.Event, error) {
	return stripe.Event{}, nil
}

type acctResp struct {
	status int
	body   map[string]any
	raw    string
	cookie string
}

func acctCall(t *testing.T, s *Server, host, method, path, body, cookie string, hdr map[string]string) acctResp {
	t.Helper()
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.SetHost(host + "." + s.cfg.BusinessDomain)
	ctx.Request.Header.Set("X-Forwarded-Proto", "https")
	if body != "" {
		ctx.Request.SetBodyString(body)
		ctx.Request.Header.SetContentType("application/json")
	}
	if cookie != "" {
		ctx.Request.Header.SetCookie(sessionCookie, cookie)
	}
	for k, v := range hdr {
		ctx.Request.Header.Set(k, v)
	}
	s.route(ctx)
	out := acctResp{status: ctx.Response.StatusCode(), raw: string(ctx.Response.Body())}
	_ = json.Unmarshal(ctx.Response.Body(), &out.body)
	ctx.Response.Header.VisitAllCookie(func(k, v []byte) {
		if string(k) == sessionCookie {
			var c fasthttp.Cookie
			if c.ParseBytes(v) == nil && c.MaxAge() >= 0 {
				out.cookie = string(c.Value())
			}
		}
	})
	return out
}

func accountFixture(t *testing.T) (*Server, *fakeGateway, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Mojo-Units", "model.embed.tokens=3")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	f := newHowFixture(t, up.URL, 0)
	t.Setenv("MOJOJOJO_UPSTREAM_EMBED", up.URL)
	gw := &fakeGateway{}
	f.s.cfg.StripePublishableKey = "pk_test_fake"
	f.s.cfg.StripeCreditsPriceID = "price_fake"
	f.s.stripe = &stripeBilling{db: f.s.db, auth: f.s.auth, cfg: f.s.cfg, gateway: gw, customerLocks: map[string]*sync.Mutex{}}
	return f.s, gw, up
}

func TestAccountSignupLoginKeyAndSession(t *testing.T) {
	s, _, _ := accountFixture(t)
	r := acctCall(t, s, "embed", "GET", "/_acct/me", "", "", nil)
	if r.status != 200 || r.body["user"] != nil || r.body["stripe"] != true {
		t.Fatalf("anonymous me: %d %s", r.status, r.raw)
	}

	su := acctCall(t, s, "embed", "POST", "/_acct/signup", `{"email":"inline@example.com","password":"correct horse"}`, "", nil)
	if su.status != 200 || su.cookie == "" {
		t.Fatalf("signup: %d %s cookie=%q", su.status, su.raw, su.cookie)
	}
	user, _ := su.body["user"].(map[string]any)
	if user["email"] != "inline@example.com" {
		t.Fatalf("signup did not return the account: %s", su.raw)
	}
	if usage, _ := su.body["usage"].([]any); len(usage) != 1 {
		t.Fatalf("usage rows for embed: %s", su.raw)
	}

	if me := acctCall(t, s, "embed", "GET", "/_acct/me", "", su.cookie, nil); me.body["user"] == nil {
		t.Fatalf("cookie did not authenticate: %s", me.raw)
	}
	if dup := acctCall(t, s, "embed", "POST", "/_acct/signup", `{"email":"inline@example.com","password":"correct horse"}`, "", nil); dup.status != 400 || dup.cookie != "" {
		t.Fatalf("duplicate signup: %d %s", dup.status, dup.raw)
	}
	if bad := acctCall(t, s, "embed", "POST", "/_acct/login", `{"email":"inline@example.com","password":"wrong"}`, "", nil); bad.status != 401 || bad.cookie != "" {
		t.Fatalf("bad password: %d %s", bad.status, bad.raw)
	}
	li := acctCall(t, s, "embed", "POST", "/_acct/login", `{"email":"inline@example.com","password":"correct horse"}`, "", nil)
	if li.status != 200 || li.cookie == "" || li.cookie == su.cookie {
		t.Fatalf("login: %d %s", li.status, li.raw)
	}

	if anon := acctCall(t, s, "embed", "POST", "/_acct/key", `{}`, "", nil); anon.status != 401 {
		t.Fatalf("key without a session: %d", anon.status)
	}
	k := acctCall(t, s, "embed", "POST", "/_acct/key", `{}`, li.cookie, nil)
	key, _ := k.body["key"].(string)
	if k.status != 200 || !strings.HasPrefix(key, keyPrefix) {
		t.Fatalf("key: %d %s", k.status, k.raw)
	}

	if unpaid := acctCall(t, s, "embed", "POST", "/v1/embeddings", `{"input":"x"}`, li.cookie, nil); unpaid.status != 402 {
		t.Fatalf("metered call without paid credits: %d %s", unpaid.status, unpaid.raw)
	}
	if _, err := s.db.Exec(`UPDATE users SET paid_credits=100 WHERE id=$1`, userIDOf(t, s, "inline@example.com")); err != nil {
		t.Fatal(err)
	}
	// The playground path: the cookie alone pays for a metered /v1 call.
	call := acctCall(t, s, "embed", "POST", "/v1/embeddings", `{"input":"x"}`, li.cookie, nil)
	if call.status != 200 {
		t.Fatalf("cookie call: %d %s", call.status, call.raw)
	}
	if got := s.meters.drain()[meterKey{userIDOf(t, s, "inline@example.com"), "model.embed.tokens"}]; got != 3 {
		t.Errorf("cookie call not metered: %v", got)
	}
	// The key works the same way.
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI("/v1/embeddings")
	ctx.Request.Header.SetHost("embed." + s.cfg.BusinessDomain)
	ctx.Request.Header.Set("Authorization", "Bearer "+key)
	ctx.Request.SetBodyString(`{"input":"x"}`)
	s.route(ctx)
	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("key call: %d %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}

	if out := acctCall(t, s, "embed", "POST", "/_acct/logout", `{}`, li.cookie, nil); out.status != 200 {
		t.Fatalf("logout: %d", out.status)
	}
	if me := acctCall(t, s, "embed", "GET", "/_acct/me", "", li.cookie, nil); me.body["user"] != nil {
		t.Fatalf("session survived logout: %s", me.raw)
	}
}

func userIDOf(t *testing.T, s *Server, email string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM users WHERE email=$1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAccountRejectsCrossSiteAndNonJSON(t *testing.T) {
	s, _, _ := accountFixture(t)
	body := `{"email":"x@example.com","password":"correct horse"}`
	cases := map[string]map[string]string{
		"foreign origin":   {"Origin": "https://evil.example"},
		"sibling business": {"Origin": "https://rerank." + s.cfg.BusinessDomain},
		"form post":        {"Content-Type": "application/x-www-form-urlencoded"},
	}
	for name, hdr := range cases {
		r := acctCall(t, s, "embed", "POST", "/_acct/signup", body, "", hdr)
		if r.status != 403 || r.cookie != "" {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	ok := acctCall(t, s, "embed", "POST", "/_acct/signup", body, "", map[string]string{"Origin": "https://embed." + s.cfg.BusinessDomain})
	if ok.status != 200 {
		t.Errorf("same-origin signup refused: %d %s", ok.status, ok.raw)
	}
	if r := acctCall(t, s, "embed", "GET", "/_acct/nope", "", "", nil); r.status != 404 {
		t.Errorf("unknown account path: %d", r.status)
	}
}

func TestAccountSignupIsRateLimited(t *testing.T) {
	s, _, _ := accountFixture(t)
	var last acctResp
	for i := 0; i < 5; i++ {
		last = acctCall(t, s, "embed", "POST", "/_acct/signup", `{"email":"rl`+string(rune('a'+i))+`@example.com","password":"correct horse"}`, "", nil)
	}
	if last.status != 429 {
		t.Fatalf("5th signup from one address: %d %s", last.status, last.raw)
	}
}

func TestAccountInlineCheckout(t *testing.T) {
	s, gw, _ := accountFixture(t)
	if r := acctCall(t, s, "embed", "POST", "/_acct/checkout", `{"cents":500}`, "", nil); r.status != 401 {
		t.Fatalf("anonymous checkout: %d", r.status)
	}
	su := acctCall(t, s, "embed", "POST", "/_acct/signup", `{"email":"buyer@example.com","password":"correct horse"}`, "", nil)
	for _, cents := range []string{"0", "499", "50001"} {
		if r := acctCall(t, s, "embed", "POST", "/_acct/checkout", `{"cents":`+cents+`}`, su.cookie, nil); r.status != 400 {
			t.Errorf("cents=%s: %d %s", cents, r.status, r.raw)
		}
	}
	r := acctCall(t, s, "embed", "POST", "/_acct/checkout", `{"cents":2000}`, su.cookie, nil)
	if r.status != 200 || r.body["client_secret"] != "cs_secret_fake" || r.body["publishable_key"] != "pk_test_fake" {
		t.Fatalf("checkout: %d %s", r.status, r.raw)
	}
	if len(gw.sessions) != 1 {
		t.Fatalf("sessions created: %d", len(gw.sessions))
	}
	p := gw.sessions[0]
	if p.ReturnURL != nil || p.RedirectOnCompletion == nil || *p.RedirectOnCompletion != "never" {
		t.Errorf("inline checkout must not redirect: return=%v redirect=%v", p.ReturnURL, p.RedirectOnCompletion)
	}
	if *p.UIMode != "embedded_page" && *p.UIMode != "embedded" {
		t.Errorf("ui mode %q", *p.UIMode)
	}
	if *p.LineItems[0].Quantity != 2000 || p.Metadata["mojojojo_credits"] != "20000" {
		t.Errorf("quantity=%d credits=%q", *p.LineItems[0].Quantity, p.Metadata["mojojojo_credits"])
	}
	var credits int
	if err := s.db.QueryRow(`SELECT credits FROM stripe_checkouts WHERE session_id='cs_fake_b'`).Scan(&credits); err != nil || credits != 20000 {
		t.Errorf("checkout record: credits=%d err=%v", credits, err)
	}

	// The main-site flow still redirects back to /credits.
	main := &fasthttp.RequestCtx{}
	main.Request.Header.SetMethod("POST")
	main.Request.SetRequestURI("/v1/billing/checkout")
	main.Request.Header.SetHost("mojojojo.cc")
	main.Request.Header.SetCookie(sessionCookie, su.cookie)
	main.Request.SetBodyString(`{"offer":"credits","quantity":500}`)
	s.route(main)
	if main.Response.StatusCode() != 200 || len(gw.sessions) != 2 || gw.sessions[1].ReturnURL == nil {
		t.Errorf("main checkout regressed: %d %s", main.Response.StatusCode(), main.Response.Body())
	}
}

func TestAccountStripeDisabledReportsIt(t *testing.T) {
	s, _, _ := accountFixture(t)
	s.stripe = nil
	su := acctCall(t, s, "embed", "POST", "/_acct/signup", `{"email":"nostripe@example.com","password":"correct horse"}`, "", nil)
	if su.body["stripe"] != false {
		t.Errorf("stripe flag: %s", su.raw)
	}
	if r := acctCall(t, s, "embed", "POST", "/_acct/checkout", `{"cents":500}`, su.cookie, nil); r.status != 503 {
		t.Errorf("checkout without stripe: %d %s", r.status, r.raw)
	}
}

func TestBusinessPageServesAccountPanelAndScript(t *testing.T) {
	s, _, _ := accountFixture(t)
	page := acctCall(t, s, "embed", "GET", "/", "", "", nil)
	for _, want := range []string{`id="biz"`, `data-slug="embed"`, `/static/business.js`, `id="stripe-modal"`, "input_type", `data-stripe="1"`} {
		if !strings.Contains(page.raw, want) {
			t.Errorf("landing page lacks %q", want)
		}
	}
	js := acctCall(t, s, "embed", "GET", "/static/business.js", "", "", nil)
	if js.status != 200 || !strings.Contains(js.raw, "initEmbeddedCheckout") {
		t.Errorf("business.js: %d", js.status)
	}
}
