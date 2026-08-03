package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

func routeRequest(server *Server, method, path string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	server.route(ctx)
	return ctx
}

func TestCompletePublicPageAndAssetRouteSurface(t *testing.T) {
	server := authTestServer(t)
	pages := []string{
		"/", "/editor", "/playground", "/studio", "/ide", "/apps", "/apps/",
		"/tools", "/tools/", "/pricing", "/docs", "/keys", "/usage", "/credits",
		"/billing", "/cloud", "/data", "/data/", "/vector", "/vector/", "/examples",
		"/examples/", "/agent", "/research", "/research/", "/auth/login", "/auth/signup",
		"/robots.txt", "/sitemap.xml", "/mojojojo.js", "/mojojojo.min.js", "/hl.js",
		"/route.js", "/install.sh", "/install.ps1",
	}
	for _, path := range pages {
		ctx := routeRequest(server, http.MethodGet, path)
		if got := ctx.Response.StatusCode(); got != http.StatusOK {
			t.Errorf("GET %s = %d, body=%s", path, got, ctx.Response.Body())
			continue
		}
		if len(ctx.Response.Body()) == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
	}
	credits := routeRequest(server, http.MethodGet, "/credits")
	csp := string(credits.Response.Header.Peek("Content-Security-Policy"))
	for _, source := range []string{"https://js.stripe.com", "https://static.cloudflareinsights.com", "https://cloudflareinsights.com"} {
		if !strings.Contains(csp, source) {
			t.Errorf("credits CSP is missing %s: %s", source, csp)
		}
	}
	visual := routeRequest(server, http.MethodGet, "/visualbench")
	if visual.Response.StatusCode() != http.StatusFound ||
		!strings.HasPrefix(string(visual.Response.Header.Peek("Location")), "/static/visualbench/") {
		t.Fatalf("visualbench redirect = %d %q", visual.Response.StatusCode(), visual.Response.Header.Peek("Location"))
	}
	missing := routeRequest(server, http.MethodGet, "/definitely-not-a-route")
	if missing.Response.StatusCode() != http.StatusNotFound || len(missing.Response.Body()) == 0 {
		t.Fatalf("missing route = %d %s", missing.Response.StatusCode(), missing.Response.Body())
	}
}

func TestAnonymousAPIRouteStatusMatrix(t *testing.T) {
	server := authTestServer(t)
	server.cfg.AgentToken = "internal-agent-secret"
	server.agent = newAgentProxy("http://127.0.0.1:1", 2)
	for _, path := range []string{"/v1/pricing", "/v1/me", "/v1/credits", "/v1/showcase"} {
		ctx := routeRequest(server, http.MethodGet, path)
		if got := ctx.Response.StatusCode(); got != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", path, got, ctx.Response.Body())
		}
	}
	protected := []string{
		"/v1/usage", "/v1/keys", "/v1/storage", "/v1/projects",
		"/v1/data/namespaces", "/v1/vector/projects", "/v1/vector/reservations",
		"/v1/agent/sessions",
	}
	for _, path := range protected {
		ctx := routeRequest(server, http.MethodGet, path)
		if got := ctx.Response.StatusCode(); got != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401: %s", path, got, ctx.Response.Body())
		}
	}
	internal := routeRequest(server, http.MethodPost, "/internal/agent/settle")
	if internal.Response.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("internal settlement = %d, want 401", internal.Response.StatusCode())
	}
	options := routeRequest(server, http.MethodOptions, "/v1/vector/projects")
	if options.Response.StatusCode() != http.StatusNoContent {
		t.Fatalf("API preflight = %d, want 204", options.Response.StatusCode())
	}
}

func TestCreditPurchaseIntentSendsVisitorThroughSignup(t *testing.T) {
	server := authTestServer(t)
	ctx := routeRequest(server, http.MethodGet, "/credits?buy=credits")
	if ctx.Response.StatusCode() != http.StatusFound {
		t.Fatalf("GET credit purchase intent = %d, want 302", ctx.Response.StatusCode())
	}
	if got, want := string(ctx.Response.Header.Peek("Location")), "/auth/signup?next=%2Fcredits%3Fbuy%3Dcredits"; got != want {
		t.Fatalf("credit purchase redirect = %q, want %q", got, want)
	}
}
