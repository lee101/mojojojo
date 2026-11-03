package main

// Same-origin account API for business subdomains. A visitor can create an
// account, sign in, mint an API key, see their credits and free allowance, and
// buy credits through Stripe's embedded checkout without ever leaving the page.
// The session cookie is host-only (set on the business host that served the
// page), so these endpoints add no cross-origin surface: mutating calls must be
// JSON with a same-origin Origin, and the cookie is SameSite=Lax.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

type acctCreds struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) businessAccount(ctx *fasthttp.RequestCtx, b business, path string) bool {
	if !strings.HasPrefix(path, "/_acct/") {
		return false
	}
	ctx.Response.Header.Set("Cache-Control", "no-store")
	method := string(ctx.Method())
	if method != "GET" && !s.acctRequestOK(ctx) {
		sendError(ctx, 403, "requests must be JSON from this site")
		return true
	}
	switch {
	case path == "/_acct/me" && method == "GET":
		s.acctMe(ctx, b)
	case path == "/_acct/signup" && method == "POST":
		s.acctAuth(ctx, true)
		s.acctMeIfOK(ctx, b)
	case path == "/_acct/login" && method == "POST":
		s.acctAuth(ctx, false)
		s.acctMeIfOK(ctx, b)
	case path == "/_acct/logout" && method == "POST":
		if tok := sessionToken(ctx); tok != "" {
			_ = s.auth.deleteSession(tok)
		}
		s.clearCookie(ctx, sessionCookie)
		sendJSON(ctx, map[string]any{"ok": true})
	case path == "/_acct/key" && method == "POST":
		u := s.auth.userFromSession(ctx)
		if u == nil {
			sendError(ctx, 401, "sign in first")
			return true
		}
		key, err := s.auth.createKey(u.ID, b.Slug+" web")
		if err != nil {
			sendError(ctx, 500, "could not create a key")
			return true
		}
		sendJSON(ctx, map[string]any{"ok": true, "key": key.Secret, "note": "shown once - store it"})
	case path == "/_acct/checkout" && method == "POST":
		var in struct {
			Cents int64 `json:"cents"`
		}
		if json.Unmarshal(ctx.PostBody(), &in) != nil {
			sendError(ctx, 400, "invalid JSON body")
			return true
		}
		body, _ := json.Marshal(stripeCheckoutRequest{Offer: "credits", Quantity: in.Cents})
		ctx.Request.SetBody(body)
		s.startCheckout(ctx, true)
	default:
		sendError(ctx, 404, "not found")
	}
	return true
}

// acctRequestOK keeps state-changing calls to this site's own JSON fetches. A
// cross-site form cannot send application/json, and a cross-site fetch carries
// a foreign Origin that is refused here.
func (s *Server) acctRequestOK(ctx *fasthttp.RequestCtx) bool {
	if !strings.HasPrefix(strings.ToLower(string(ctx.Request.Header.ContentType())), "application/json") {
		return false
	}
	if origin := string(ctx.Request.Header.Peek("Origin")); origin != "" {
		return sameOrigin(origin, requestScheme(ctx)+"://"+string(ctx.Host()))
	}
	return true
}

// acctAuth signs up or signs in and sets the session cookie. It leaves the
// response unwritten on success so the caller can append the account state.
func (s *Server) acctAuth(ctx *fasthttp.RequestCtx, signup bool) {
	var in acctCreds
	if json.Unmarshal(ctx.PostBody(), &in) != nil {
		sendError(ctx, 400, "invalid JSON body")
		return
	}
	limit, kind := 20, "login:"
	if signup {
		limit, kind = 3, "signup:"
	}
	window := 10 * time.Minute
	if signup {
		window = time.Hour
	}
	if !loginLimiter.allow(kind+clientIP(ctx), limit, window) {
		sendError(ctx, 429, "too many attempts; wait a few minutes")
		return
	}
	var u *User
	var err error
	if signup {
		u, err = s.auth.createUser(in.Email, in.Password)
	} else {
		u, err = s.auth.authenticate(in.Email, in.Password)
	}
	if err != nil {
		status := 400
		if !signup {
			status = 401
		}
		sendError(ctx, status, err.Error())
		return
	}
	tok, err := s.auth.createSession(u.ID, string(ctx.Request.Header.UserAgent()))
	if err != nil {
		sendError(ctx, 500, "could not create a session")
		return
	}
	s.setCookie(ctx, sessionCookie, tok, sessionTTL)
	ctx.SetUserValue("acct.ok", true)
}

func (s *Server) acctMeIfOK(ctx *fasthttp.RequestCtx, b business) {
	if ok, _ := ctx.UserValue("acct.ok").(bool); ok {
		s.acctMe(ctx, b)
	}
}

func (s *Server) acctMe(ctx *fasthttp.RequestCtx, b business) {
	u := s.auth.userFromSession(ctx)
	if u == nil {
		// Signup and login set the cookie on this response; read it back from there.
		u = s.userFromResponseCookie(ctx)
	}
	out := map[string]any{"stripe": s.stripe != nil, "slug": b.Slug, "user": nil}
	if u != nil {
		month := meterMonth(time.Now())
		usage := []map[string]any{}
		for _, m := range metersFor(b.Parts) {
			var qty float64
			_ = s.db.QueryRow(`SELECT COALESCE(qty,0) FROM usage_meters WHERE user_id=$1 AND meter=$2 AND month=$3`,
				u.ID, m.ID, month).Scan(&qty)
			usage = append(usage, map[string]any{"meter": m.ID, "label": m.Label, "unit": m.Unit,
				"used": qty, "free": m.Free, "usd_per_unit": m.USD()})
		}
		out["user"] = map[string]any{"email": u.Email, "credits": u.Credits(), "usd": float64(u.Credits()) * usdPerCredit}
		out["usage"] = usage
		out["topup"] = map[string]any{"min_cents": stripeMinimumTopUpCents, "max_cents": stripeMaximumTopUpCents}
	}
	sendJSON(ctx, out)
}

// userFromResponseCookie finds the user for the session cookie this very
// response just set, so signup/login can answer with the account in one round trip.
func (s *Server) userFromResponseCookie(ctx *fasthttp.RequestCtx) *User {
	var tok string
	ctx.Response.Header.VisitAllCookie(func(k, v []byte) {
		if string(k) == sessionCookie {
			var c fasthttp.Cookie
			if c.ParseBytes(v) == nil {
				tok = string(c.Value())
			}
		}
	})
	if tok == "" {
		return nil
	}
	var userID string
	if s.db.QueryRow(`SELECT user_id FROM sessions WHERE id_hash = $1`, hashToken(tok)).Scan(&userID) != nil {
		return nil
	}
	return s.auth.userByID(userID)
}
