package main

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/crypto/bcrypt"
)

func TestSafeNextRefusesOffsiteTargets(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/keys", "/keys"}, {"/usage?tab=runs", "/usage?tab=runs"}, {"", "/"},
		{"//evil.com", "/"}, {"/\\evil.com", "/"}, {"https://evil.com", "/"},
		{"evil.com", "/"}, {"../etc", "/"},
	} {
		if got := safeNext(tc.in); got != tc.want {
			t.Errorf("safeNext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSignupDefaultsToEditorAndKeepsExplicitDestination(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "/editor"}, {"   ", "/editor"}, {"/keys", "/keys"}, {"https://evil.com", "/"},
	} {
		if got := signupNext(tc.in); got != tc.want {
			t.Errorf("signupNext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPasswordLengthAllowsSixThroughSeventyTwoCharacters(t *testing.T) {
	for _, tc := range []struct {
		password string
		wantErr  bool
	}{
		{"12345", true},
		{"123456", false},
		{strings.Repeat("a", passwordMaxLength), false},
		{strings.Repeat("a", passwordMaxLength+1), true},
	} {
		if got := validatePassword(tc.password); (got != nil) != tc.wantErr {
			t.Errorf("validatePassword(length %d) error = %v, wantErr %v", len(tc.password), got, tc.wantErr)
		}
	}
}

func TestRequestSchemeTrustsForwardedProto(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("X-Forwarded-Proto", "https, http")
	if got := requestScheme(ctx); got != "https" {
		t.Fatalf("scheme = %q", got)
	}
}

func TestAuthFormsReuseTokenAcrossTabs(t *testing.T) {
	s := authTestServer(t)
	token, _ := authGet(t, s, "/auth/signup")

	second := &fasthttp.RequestCtx{}
	second.Request.Header.SetMethod("GET")
	second.Request.SetRequestURI("/auth/login")
	second.Request.Header.SetHost("mojojojo.cc")
	second.Request.Header.Set("X-Forwarded-Proto", "https")
	second.Request.Header.SetCookie(authFormCookie, token)
	s.route(second)

	if got := responseCookie(t, second, authFormCookie); got != token {
		t.Fatalf("opening another auth tab rotated token: got %q, want %q", got, token)
	}
	if !strings.Contains(string(second.Response.Body()), `name="csrf" value="`+token+`"`) {
		t.Fatal("second auth tab did not reuse the browser token")
	}
}

func TestAuthFormSameOriginFallbackAndCrossSiteRejection(t *testing.T) {
	s := authTestServer(t)
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.Header.SetContentType("application/x-www-form-urlencoded")
	ctx.Request.SetBodyString("csrf=token-from-an-open-form")
	ctx.Request.Header.Set("Origin", "https://mojojojo.cc")
	if !s.validAuthForm(ctx) {
		t.Fatal("same-origin browser POST was rejected after its cookie expired")
	}

	ctx.Request.Header.Set("Origin", "https://evil.example")
	if s.validAuthForm(ctx) {
		t.Fatal("cross-site origin bypassed the auth form token")
	}
	ctx.Request.Header.Del("Origin")
	if s.validAuthForm(ctx) {
		t.Fatal("missing cookie and origin bypassed the auth form token")
	}
}

func TestConfigKeepsIdentityOnMojojojo(t *testing.T) {
	t.Setenv("SITE_BASE_URL", "https://mojojojo.cc")
	// Legacy variables may still exist on a host during rollout; they must not
	// pull identity or billing back across the product boundary.
	t.Setenv("APPNZ_LOGIN_URL", "https://app.nz/login")
	t.Setenv("APPNZ_DATABASE_PATH", "/tmp/appnz.db")
	cfg := loadConfig()
	if cfg.LoginURL != "https://mojojojo.cc/auth/login" || cfg.SignupURL != "https://mojojojo.cc/auth/signup" {
		t.Fatalf("identity escaped the local origin: login=%q signup=%q", cfg.LoginURL, cfg.SignupURL)
	}
	if !strings.HasPrefix(cfg.DatabaseURL, "postgres") {
		t.Fatalf("database is not PostgreSQL: %q", cfg.DatabaseURL)
	}
}

func authTestServer(t *testing.T) *Server {
	t.Helper()
	loginLimiter = &authLimiter{attempts: map[string]authAttempt{}}
	db := testPostgres(t)
	cfg := loadConfig()
	tmpl, err := loadTemplates(cfg)
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := newEmailCampaign(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: cfg, db: db, auth: newAuthStore(db), tmpl: tmpl, email: campaign}
}

func authGet(t *testing.T, s *Server, path string) (string, *fasthttp.RequestCtx) {
	t.Helper()
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.SetHost("mojojojo.cc")
	ctx.Request.Header.Set("X-Forwarded-Proto", "https")
	s.route(ctx)
	cookie := fasthttp.AcquireCookie()
	defer fasthttp.ReleaseCookie(cookie)
	cookie.SetKey(authFormCookie)
	if !ctx.Response.Header.Cookie(cookie) {
		t.Fatalf("auth form cookie missing: %s", ctx.Response.Header.Peek("Set-Cookie"))
	}
	return string(cookie.Value()), ctx
}

func authPost(s *Server, path string, values url.Values, csrf string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.SetHost("mojojojo.cc")
	ctx.Request.Header.Set("X-Forwarded-Proto", "https")
	ctx.Request.Header.SetContentType("application/x-www-form-urlencoded")
	ctx.Request.Header.SetCookie(authFormCookie, csrf)
	values.Set("csrf", csrf)
	ctx.Request.SetBodyString(values.Encode())
	s.route(ctx)
	return ctx
}

func responseCookie(t *testing.T, ctx *fasthttp.RequestCtx, name string) string {
	t.Helper()
	c := fasthttp.AcquireCookie()
	defer fasthttp.ReleaseCookie(c)
	c.SetKey(name)
	if !ctx.Response.Header.Cookie(c) {
		t.Fatalf("cookie %s missing: %s", name, ctx.Response.Header.Peek("Set-Cookie"))
	}
	return string(c.Value())
}

func TestSignupCreatesLocalAccountAndSession(t *testing.T) {
	s := authTestServer(t)
	csrf, page := authGet(t, s, "/auth/signup?next=/keys")
	body := string(page.Response.Body())
	if !strings.Contains(body, "independent mojojojo account") {
		t.Fatal("signup page does not explain the independent account boundary")
	}
	for _, want := range []string{`minlength="6"`, `maxlength="72"`, `name="next" value="/keys"`, "Step 1 of 1", "six-part product"} {
		if !strings.Contains(body, want) {
			t.Errorf("signup page is missing %q", want)
		}
	}
	ctx := authPost(s, "/auth/signup", url.Values{
		"email": {"Dev@Example.com"}, "password": {"correct horse battery staple"}, "next": {"/keys"}, "product_updates": {"on"},
	}, csrf)
	if ctx.Response.StatusCode() != fasthttp.StatusFound || string(ctx.Response.Header.Peek("Location")) != "/keys" {
		t.Fatalf("signup status=%d location=%q body=%s", ctx.Response.StatusCode(), ctx.Response.Header.Peek("Location"), ctx.Response.Body())
	}
	token := responseCookie(t, ctx, sessionCookie)
	var email, passwordHash string
	if err := s.db.QueryRow(`SELECT email, password_hash FROM users`).Scan(&email, &passwordHash); err != nil {
		t.Fatal(err)
	}
	if email != "dev@example.com" || passwordHash == "correct horse battery staple" {
		t.Fatalf("account was not normalized/hashed: %q %q", email, passwordHash)
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte("correct horse battery staple")) != nil {
		t.Fatal("stored password hash does not verify")
	}
	var optedIn bool
	if err := s.db.QueryRow(`SELECT product_updates FROM email_preferences`).Scan(&optedIn); err != nil || !optedIn {
		t.Fatalf("explicit product email opt-in not recorded: enabled=%v err=%v", optedIn, err)
	}
	request := &fasthttp.RequestCtx{}
	request.Request.Header.SetCookie(sessionCookie, token)
	if u := s.auth.userFromSession(request); u == nil || u.Email != email {
		t.Fatalf("new session did not resolve: %+v", u)
	}
}

func TestSignupErrorKeepsEmailButNeverPassword(t *testing.T) {
	s := authTestServer(t)
	csrf, _ := authGet(t, s, "/auth/signup")
	ctx := authPost(s, "/auth/signup", url.Values{
		"email": {"Dev@Example.com"}, "password": {"short"}, "next": {"/editor"},
	}, csrf)
	body := string(ctx.Response.Body())
	if ctx.Response.StatusCode() != fasthttp.StatusBadRequest || !strings.Contains(body, "at least 6 characters") {
		t.Fatalf("short password status=%d body=%s", ctx.Response.StatusCode(), body)
	}
	if !strings.Contains(body, `value="Dev@Example.com"`) {
		t.Fatal("signup error did not retain the submitted email")
	}
	if strings.Contains(body, `value="short"`) {
		t.Fatal("signup error reflected the submitted password")
	}
}

func TestLoginRejectsBadPasswordAndAcceptsGoodOne(t *testing.T) {
	s := authTestServer(t)
	if _, err := s.auth.createUser("dev@example.com", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	csrf, _ := authGet(t, s, "/auth/login")
	bad := authPost(s, "/auth/login", url.Values{"email": {"dev@example.com"}, "password": {"wrong password"}}, csrf)
	if bad.Response.StatusCode() != fasthttp.StatusUnauthorized || !strings.Contains(string(bad.Response.Body()), errInvalidCredentials.Error()) {
		t.Fatalf("bad login status=%d body=%s", bad.Response.StatusCode(), bad.Response.Body())
	}
	csrf, _ = authGet(t, s, "/auth/login")
	good := authPost(s, "/auth/login", url.Values{"email": {"dev@example.com"}, "password": {"correct horse battery staple"}}, csrf)
	if good.Response.StatusCode() != fasthttp.StatusFound || responseCookie(t, good, sessionCookie) == "" {
		t.Fatalf("good login failed: %d %s", good.Response.StatusCode(), good.Response.Body())
	}
}

func TestSignupRequiresCSRFAndUniqueEmail(t *testing.T) {
	s := authTestServer(t)
	values := url.Values{"email": {"dev@example.com"}, "password": {"correct horse battery staple"}}
	// Expired or invalid forms never reach password checking and therefore must
	// not burn the very small signup-attempt allowance.
	for range 5 {
		forged := &fasthttp.RequestCtx{}
		forged.Request.Header.SetMethod("POST")
		forged.Request.SetRequestURI("/auth/signup")
		forged.Request.Header.SetContentType("application/x-www-form-urlencoded")
		forged.Request.Header.SetCookie(authFormCookie, "cookie")
		forged.Request.SetBodyString("csrf=form&email=dev%40example.com&password=correct+horse+battery+staple")
		s.route(forged)
		if forged.Response.StatusCode() != fasthttp.StatusBadRequest ||
			!strings.Contains(string(forged.Response.Body()), "form expired") {
			t.Fatalf("forged signup status=%d body=%s", forged.Response.StatusCode(), forged.Response.Body())
		}
	}
	csrf, _ := authGet(t, s, "/auth/signup")
	first := authPost(s, "/auth/signup", values, csrf)
	if first.Response.StatusCode() != fasthttp.StatusFound {
		t.Fatalf("first signup: %d %s", first.Response.StatusCode(), first.Response.Body())
	}
	csrf, _ = authGet(t, s, "/auth/signup")
	second := authPost(s, "/auth/signup", values, csrf)
	if second.Response.StatusCode() != fasthttp.StatusBadRequest || !strings.Contains(string(second.Response.Body()), errEmailTaken.Error()) {
		t.Fatalf("duplicate signup: %d %s", second.Response.StatusCode(), second.Response.Body())
	}
}

func TestSessionExpiryIsEnforced(t *testing.T) {
	s := authTestServer(t)
	u, err := s.auth.createUser("dev@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.auth.createSession(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = $1`, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetCookie(sessionCookie, token)
	if got := s.auth.userFromSession(ctx); got != nil {
		t.Fatal("expired session still resolved")
	}
}
