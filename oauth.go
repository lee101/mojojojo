package main

import (
	"crypto/subtle"
	"errors"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	authFormCookie = "mojojojo_auth_form"
	authFormTTL    = 2 * time.Hour
)

var errAuthForm = errors.New("this form expired; enter your password and try again")

// safeNext keeps post-auth redirects on mojojojo.cc.
func safeNext(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return "/"
	}
	return raw
}

// A new account should open into something useful when signup was not started
// from a specific product page. Explicit destinations are still preserved.
func signupNext(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "/editor"
	}
	return safeNext(raw)
}

func requestScheme(ctx *fasthttp.RequestCtx) string {
	if p := strings.TrimSpace(string(ctx.Request.Header.Peek("X-Forwarded-Proto"))); p != "" {
		if i := strings.IndexByte(p, ','); i > 0 {
			p = p[:i]
		}
		return strings.ToLower(strings.TrimSpace(p))
	}
	if ctx.IsTLS() {
		return "https"
	}
	return "http"
}

func redirectLocal(ctx *fasthttp.RequestCtx, path string) {
	ctx.Response.Header.Set("Location", safeNext(path))
	ctx.SetStatusCode(fasthttp.StatusFound)
}

type authAttempt struct {
	window time.Time
	count  int
}

type authLimiter struct {
	mu       sync.Mutex
	attempts map[string]authAttempt
}

var loginLimiter = &authLimiter{attempts: map[string]authAttempt{}}

func (l *authLimiter) allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	a := l.attempts[key]
	if a.window.IsZero() || now.Sub(a.window) > window {
		a = authAttempt{window: now}
	}
	a.count++
	l.attempts[key] = a
	return a.count <= max
}

func (s *Server) handleAuthLogin(ctx *fasthttp.RequestCtx) {
	if string(ctx.Method()) == "GET" {
		if s.auth.userFromSession(ctx) != nil {
			redirectLocal(ctx, safeNext(string(ctx.QueryArgs().Peek("next"))))
			return
		}
		s.renderAuth(ctx, "login", "", safeNext(string(ctx.QueryArgs().Peek("next"))))
		return
	}
	next := safeNext(string(ctx.FormValue("next")))
	if !s.validAuthForm(ctx) {
		s.renderAuth(ctx, "login", errAuthForm.Error(), next)
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		return
	}
	if !loginLimiter.allow("login:"+clientIP(ctx), 20, 10*time.Minute) {
		s.renderAuth(ctx, "login", "too many attempts; wait a few minutes", next)
		ctx.SetStatusCode(fasthttp.StatusTooManyRequests)
		return
	}
	u, err := s.auth.authenticate(string(ctx.FormValue("email")), string(ctx.FormValue("password")))
	if err != nil {
		s.renderAuth(ctx, "login", errInvalidCredentials.Error(), next)
		ctx.SetStatusCode(fasthttp.StatusUnauthorized)
		return
	}
	if err := s.finishAuth(ctx, u, next); err != nil {
		s.renderAuth(ctx, "login", "could not create a session", next)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
	}
}

func (s *Server) handleAuthSignup(ctx *fasthttp.RequestCtx) {
	if string(ctx.Method()) == "GET" {
		if s.auth.userFromSession(ctx) != nil {
			redirectLocal(ctx, signupNext(string(ctx.QueryArgs().Peek("next"))))
			return
		}
		s.renderAuth(ctx, "signup", "", signupNext(string(ctx.QueryArgs().Peek("next"))))
		return
	}
	next := signupNext(string(ctx.FormValue("next")))
	if !s.validAuthForm(ctx) {
		s.renderAuth(ctx, "signup", errAuthForm.Error(), next)
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		return
	}
	if !loginLimiter.allow("signup:"+clientIP(ctx), 3, time.Hour) {
		s.renderAuth(ctx, "signup", "too many attempts; wait a few minutes", next)
		ctx.SetStatusCode(fasthttp.StatusTooManyRequests)
		return
	}
	u, err := s.auth.createUser(string(ctx.FormValue("email")), string(ctx.FormValue("password")))
	if err != nil {
		s.renderAuth(ctx, "signup", err.Error(), next)
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		return
	}
	if s.email != nil && string(ctx.FormValue("product_updates")) == "on" {
		if err := s.email.setPreference(u.ID, true); err != nil {
			log.Printf("email opt-in for new user %s: %v", u.ID, err)
		}
	}
	if err := s.finishAuth(ctx, u, next); err != nil {
		s.renderAuth(ctx, "signup", "account created, but the session could not be created", next)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
	}
}

func (s *Server) finishAuth(ctx *fasthttp.RequestCtx, u *User, next string) error {
	token, err := s.auth.createSession(u.ID, string(ctx.Request.Header.UserAgent()))
	if err != nil {
		return err
	}
	s.setCookie(ctx, sessionCookie, token, sessionTTL)
	s.clearCookie(ctx, authFormCookie)
	redirectLocal(ctx, next)
	return nil
}

func (s *Server) handleAuthLogout(ctx *fasthttp.RequestCtx) {
	if tok := sessionToken(ctx); tok != "" {
		_ = s.auth.deleteSession(tok)
	}
	s.clearCookie(ctx, sessionCookie)
	redirectLocal(ctx, "/")
}

func (s *Server) renderAuth(ctx *fasthttp.RequestCtx, mode, message, next string) {
	// Keep one stable double-submit token for the browser instead of rotating it
	// every time another login/signup tab opens. Rotating a singleton cookie made
	// the older tab fail with "form expired" even though it was only seconds old.
	csrf := authFormToken(ctx)
	if csrf == "" {
		csrf = randomToken(24)
	}
	s.setCookie(ctx, authFormCookie, csrf, authFormTTL)
	email := strings.TrimSpace(string(ctx.FormValue("email")))
	title := "Sign in"
	if mode == "signup" {
		title = "Create your mojojojo account"
	}
	s.render(ctx, "auth.html", map[string]any{
		"Meta": s.meta(title, ""), "Mode": mode, "Error": message,
		"Next": next, "CSRF": csrf,
		"Email":          email,
		"ProductUpdates": string(ctx.FormValue("product_updates")) == "on",
		"FocusPassword":  email != "" && strings.Contains(message, "password"),
		"PasswordMin":    passwordMinLength, "PasswordMax": passwordMaxLength,
	})
}

func authFormToken(ctx *fasthttp.RequestCtx) string {
	token := strings.TrimSpace(string(ctx.Request.Header.Cookie(authFormCookie)))
	// randomToken(24) is 32 unpadded base64url characters. Never reflect an
	// arbitrary request cookie into Set-Cookie when rendering a fresh form.
	if len(token) != 32 {
		return ""
	}
	for _, c := range token {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			return ""
		}
	}
	return token
}

func (s *Server) validAuthForm(ctx *fasthttp.RequestCtx) bool {
	cookie := authFormToken(ctx)
	form := strings.TrimSpace(string(ctx.FormValue("csrf")))
	if cookie != "" && len(cookie) == len(form) &&
		subtle.ConstantTimeCompare([]byte(cookie), []byte(form)) == 1 {
		return true
	}
	// Browsers attach Origin to a form POST. It is a safe fallback when the
	// browser expired or discarded the double-submit cookie: a hostile site
	// cannot forge this header, while scripts and older clients still need the
	// cookie/token match above.
	return form != "" && sameOrigin(string(ctx.Request.Header.Peek("Origin")), s.cfg.SiteBaseURL)
}

func sameOrigin(rawOrigin, rawSite string) bool {
	origin, err := url.Parse(strings.TrimSpace(rawOrigin))
	if err != nil || origin.Scheme == "" || origin.Host == "" || origin.Path != "" {
		return false
	}
	site, err := url.Parse(strings.TrimSpace(rawSite))
	if err != nil || site.Scheme == "" || site.Host == "" {
		return false
	}
	return strings.EqualFold(origin.Scheme, site.Scheme) && strings.EqualFold(origin.Host, site.Host)
}

func (s *Server) setCookie(ctx *fasthttp.RequestCtx, name, value string, ttl time.Duration) {
	var c fasthttp.Cookie
	c.SetKey(name)
	c.SetValue(value)
	c.SetPath("/")
	c.SetHTTPOnly(true)
	c.SetSameSite(fasthttp.CookieSameSiteLaxMode)
	c.SetSecure(requestScheme(ctx) == "https")
	c.SetMaxAge(int(ttl.Seconds()))
	ctx.Response.Header.SetCookie(&c)
}

func (s *Server) clearCookie(ctx *fasthttp.RequestCtx, name string) {
	var c fasthttp.Cookie
	c.SetKey(name)
	c.SetPath("/")
	c.SetHTTPOnly(true)
	c.SetSameSite(fasthttp.CookieSameSiteLaxMode)
	c.SetSecure(requestScheme(ctx) == "https")
	c.SetMaxAge(-1)
	ctx.Response.Header.SetCookie(&c)
}
