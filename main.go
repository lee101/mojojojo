// mojojojo — per-millisecond Python execution on mojojojo.cc.
//
// The web tier owns three things and nothing else: mojojojo accounts and API
// keys, what a millisecond costs, and the public API shape. All
// actual execution happens in the Python runner on 127.0.0.1:4342, which has
// no idea who anybody is.
package main

import (
	"database/sql"
	"encoding/json"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/valyala/fasthttp"

	"mojojojo/scale"
)

var staticFS = fasthttp.FSHandler("static", 1)
var webFS = fasthttp.FSHandler("web", 1)
var examplesFS = fasthttp.FSHandler("examples", 2)

type Server struct {
	cfg                 Config
	db                  *sql.DB
	auth                *authStore
	bill                *billing
	stripe              *stripeBilling
	runner              *runnerClient
	vector              *vectorClient
	free                *freeTier
	uploads             *uploadStore
	tmpl                *templates
	email               *emailCampaign
	scaler              *scale.Manager
	vectorScaler        *scale.Manager
	agent               *agentProxy
	vectorSlots         chan struct{}
	reservedVectorSlots chan struct{}
	vectorClients       map[string]*vectorClient
	vectorLoad          *vectorLoad
	// Round-robin cursor over rented workers.
	rrCounter uint64
}

func main() {
	cfg := loadConfig()

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("open mojojojo postgres: %v", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		log.Fatalf("connect mojojojo postgres: %v", err)
	}
	if err := migrateDatabase(db); err != nil {
		log.Fatalf("migrate mojojojo postgres: %v", err)
	}
	auth := newAuthStore(db)
	bill, err := newBilling(db, auth, cfg)
	if err != nil {
		log.Fatalf("billing: %v", err)
	}
	stripeBilling, err := newStripeBilling(db, auth, cfg)
	if err != nil {
		log.Fatalf("stripe billing: %v", err)
	}
	tmpl, err := loadTemplates(cfg)
	if err != nil {
		log.Fatalf("templates: %v", err)
	}
	emailCampaign, err := newEmailCampaign(cfg, db)
	if err != nil {
		log.Fatalf("email campaign: %v", err)
	}

	srv := &Server{
		cfg:                 cfg,
		db:                  db,
		auth:                auth,
		bill:                bill,
		stripe:              stripeBilling,
		runner:              newRunnerClient(cfg.RunnerURL),
		vector:              newVectorClient(cfg.VectorURL, cfg.VectorToken, &http.Client{Timeout: 2 * time.Minute}),
		vectorSlots:         make(chan struct{}, max(1, cfg.VectorConcurrency)),
		reservedVectorSlots: make(chan struct{}, max(1, cfg.ReservedVectorSlots)),
		vectorClients:       map[string]*vectorClient{},
		vectorLoad:          newVectorLoad(),
		free:                newFreeTier(cfg.FreeMsPerDay),
		tmpl:                tmpl,
		agent:               newAgentProxy(cfg.AgentURL, cfg.MaxAgentRuns),
		email:               emailCampaign,
	}

	if store, err := newUploadStore(cfg); err != nil {
		log.Printf("uploads: object storage unavailable (%v) — upload APIs will 503", err)
	} else if store != nil {
		srv.uploads = store
		log.Printf("uploads: %s/%suploads/ → %s", cfg.R2Bucket, cfg.R2Prefix, cfg.CDNBase)
	}

	srv.startScaling()
	srv.startVectorScaling()
	srv.startCapacityBilling()
	if err := srv.startEmailDrip(); err != nil {
		log.Fatalf("email campaign: %v", err)
	}

	if cfg.EdgeURL != "" {
		log.Printf("edge: %s (small Python runs go to the nearest PoP)", cfg.EdgeURL)
	}
	log.Printf("mojojojo on :%s — runner %s, %0.f ms/credit (cpu) %0.f ms/credit (gpu)",
		cfg.Port, cfg.RunnerURL, cfg.MsPerCredit, cfg.MsPerCreditGPU)
	server := &fasthttp.Server{
		Handler:            srv.route,
		Name:               "mojojojo",
		ReadTimeout:        2 * time.Minute,
		WriteTimeout:       16 * time.Minute, // a run may legitimately take 5
		MaxRequestBodySize: int(cfg.StorageMaxUpload + (2 << 20)),
	}
	if err := server.ListenAndServe(":" + cfg.Port); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func (s *Server) route(ctx *fasthttp.RequestCtx) {
	path := string(ctx.Path())
	method := string(ctx.Method())
	if s.cfg.Debug {
		log.Printf("%s %s", method, path)
	}

	// The JS client is meant to be embedded from anywhere, so the run API is
	// CORS-open. Auth is by API key on those calls; cookies are same-site.
	if strings.HasPrefix(path, "/v1/") {
		ctx.Response.Header.Set("Cache-Control", "no-store")
		ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
		ctx.Response.Header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, X-Filename, X-Project-ID")
		ctx.Response.Header.Set("Access-Control-Allow-Methods", "POST, PUT, GET, DELETE, OPTIONS")
		if method == "OPTIONS" {
			ctx.SetStatusCode(204)
			return
		}
	}

	switch {
	case path == "/health":
		s.handleHealth(ctx)
	case path == "/internal/agent/settle" && method == "POST":
		s.handleAgentSettlement(ctx)
	case path == "/v1/run" && method == "POST":
		s.handleRun(ctx)
	case strings.HasPrefix(path, "/v1/agent/"):
		s.handleAgentProxy(ctx)
	case path == "/v1/pricing":
		s.handlePricing(ctx)
	case path == "/v1/me":
		s.handleMe(ctx)
	case path == "/v1/usage":
		s.handleUsage(ctx)
	case path == "/v1/credits":
		s.handleCredits(ctx)
	case path == "/v1/billing/checkout" && method == "POST":
		s.handleStripeCheckout(ctx)
	case path == "/v1/billing/portal" && method == "POST":
		s.handleStripePortal(ctx)
	case path == "/v1/billing/session" && method == "GET":
		s.handleStripeSession(ctx)
	case path == "/v1/billing/webhook" && method == "POST":
		s.handleStripeWebhook(ctx)
	case path == "/v1/keys" && method == "GET":
		s.handleKeysList(ctx)
	case path == "/v1/keys" && method == "POST":
		s.handleKeyCreate(ctx)
	case path == "/v1/keys/revoke" && method == "POST":
		s.handleKeyRevoke(ctx)
	case path == "/v1/uploads" && method == "POST":
		s.handleUpload(ctx)
	case path == "/v1/uploads/sign" && method == "POST":
		s.handleUploadSign(ctx)
	case strings.HasPrefix(path, "/v1/uploads/") && method == "PUT":
		s.handleSignedUpload(ctx, strings.TrimPrefix(path, "/v1/uploads/"))
	case path == "/v1/storage" && method == "GET":
		s.handleStorage(ctx)
	case path == "/v1/data/namespaces" && (method == "GET" || method == "POST"):
		s.handleDataNamespaces(ctx)
	case strings.HasPrefix(path, "/v1/data/namespaces/"):
		s.handleDataNamespaceRoute(ctx, strings.TrimPrefix(path, "/v1/data/namespaces/"))
	case path == "/v1/vector/projects" && (method == "GET" || method == "POST"):
		s.handleVectorProjects(ctx)
	case path == "/v1/vector/reservations" && (method == "GET" || method == "POST"):
		s.handleCapacityReservations(ctx)
	case strings.HasPrefix(path, "/v1/vector/reservations/") && method == "DELETE":
		s.handleCapacityReservationCancel(ctx, strings.TrimPrefix(path, "/v1/vector/reservations/"))
	case strings.HasPrefix(path, "/v1/vector/projects/"):
		s.handleVectorProjectRoute(ctx, strings.TrimPrefix(path, "/v1/vector/projects/"))
	case strings.HasPrefix(path, "/v1/assets/") && method == "DELETE":
		s.handleAssetDelete(ctx, strings.TrimPrefix(path, "/v1/assets/"))
	case path == "/v1/projects" && (method == "GET" || method == "POST"):
		s.handleProjects(ctx)
	case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/files") && (method == "GET" || method == "PUT"):
		projectID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/projects/"), "/files")
		s.handleProjectFiles(ctx, projectID)
	case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/versions") && method == "GET":
		projectID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/projects/"), "/versions")
		s.handleProjectVersions(ctx, projectID)
	case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/share") && method == "POST":
		projectID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/projects/"), "/share")
		s.handleProjectShare(ctx, projectID)
	case strings.HasPrefix(path, "/v1/shared/projects/") && strings.HasSuffix(path, "/files") && method == "GET":
		token := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/shared/projects/"), "/files")
		s.handleSharedProjectFiles(ctx, token)
	case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/download") && method == "GET":
		projectID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/projects/"), "/download")
		s.handleProjectDownload(ctx, projectID)
	case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/publish") && method == "POST":
		projectID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/projects/"), "/publish")
		s.handleProjectPublish(ctx, projectID)
	case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/submit") && method == "POST":
		projectID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/projects/"), "/submit")
		s.handleProjectSubmit(ctx, projectID)
	case strings.HasPrefix(path, "/v1/projects/") && strings.Contains(path, "/preview/functions/") && method == "POST":
		rest := strings.TrimPrefix(path, "/v1/projects/")
		rest = strings.Replace(rest, "/preview/functions/", "/", 1)
		s.handleProjectPreviewFunction(ctx, rest)
	case strings.HasPrefix(path, "/v1/projects/") && !strings.Contains(strings.TrimPrefix(path, "/v1/projects/"), "/") && method == "DELETE":
		s.handleProjectDelete(ctx, strings.TrimPrefix(path, "/v1/projects/"))
	case path == "/v1/showcase" && method == "GET":
		s.handleShowcase(ctx)
	case strings.HasPrefix(path, "/v1/showcase/") && strings.HasSuffix(path, "/review") && method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/showcase/"), "/review")
		s.handleShowcaseReview(ctx, id)
	case path == "/v1/deployments" && method == "POST":
		s.handleDeploymentCreate(ctx)
	case strings.HasPrefix(path, "/v1/projects/") && strings.HasSuffix(path, "/deployments") && method == "GET":
		projectID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/projects/"), "/deployments")
		s.handleProjectDeployments(ctx, projectID)
	case path == "/v1/plan" && method == "POST":
		s.handlePlan(ctx)
	case path == "/v1/settle" && method == "POST":
		s.handleSettle(ctx)
	case path == "/v1/fleet":
		sendJSON(ctx, s.fleet())
	case path == "/v1/scale":
		s.handleScaleStatus(ctx)
	case path == "/worker.tar.gz":
		s.handleWorkerTarball(ctx)
	case path == "/vector-worker.tar.gz":
		s.handleVectorWorkerTarball(ctx)

	case path == "/mojojojo.js" || path == "/mojojojo.min.js":
		s.serveClientJS(ctx)
	case path == "/hl.js":
		s.serveJS(ctx, "web/hl.js")
	case path == "/route.js":
		// Imported by mojojojo.js: the placement decision has to run in the
		// caller's browser, because only it knows whether the program can run
		// there for free.
		s.serveJS(ctx, "web/route.js")
	case path == "/install.sh" && (method == "GET" || method == "HEAD"):
		handleAgentInstaller(ctx, "install.sh")
	case path == "/install.ps1" && (method == "GET" || method == "HEAD"):
		handleAgentInstaller(ctx, "install.ps1")
	case strings.HasPrefix(path, "/web/"):
		webFS(ctx)
	case strings.HasPrefix(path, "/static/"):
		staticFS(ctx)
	case strings.HasPrefix(path, "/examples/source/"):
		examplesFS(ctx)
	case strings.HasPrefix(path, "/p/"):
		s.handleHostedApp(ctx, strings.TrimPrefix(path, "/p/"))

	case path == "/auth/login" && (method == "GET" || method == "POST"):
		s.handleAuthLogin(ctx)
	case path == "/auth/signup" && (method == "GET" || method == "POST"):
		s.handleAuthSignup(ctx)
	case path == "/auth/logout" && (method == "GET" || method == "POST"):
		s.handleAuthLogout(ctx)
	case path == "/email/unsubscribe" && (method == "GET" || method == "POST"):
		s.handleEmailUnsubscribe(ctx)
	case path == "/email/preferences" && (method == "GET" || method == "POST"):
		s.handleEmailPreferences(ctx)

	case path == "/":
		s.handleHome(ctx)
	case path == "/docs":
		s.handleDocs(ctx)
	case path == "/agent":
		s.handleAgentPage(ctx)
	case path == "/editor" || path == "/playground":
		s.handleEditor(ctx)
	case path == "/studio" || path == "/ide":
		s.handleStudio(ctx)
	case path == "/apps" || path == "/apps/":
		s.handleShowcasePage(ctx, "app")
	case path == "/tools" || path == "/tools/":
		s.handleShowcasePage(ctx, "tool")
	case path == "/pricing":
		s.handlePricing2(ctx)
	case path == "/cloud":
		s.handleCloudPage(ctx)
	case path == "/data" || path == "/data/":
		s.handleDataPage(ctx)
	case path == "/vector" || path == "/vector/":
		s.handleVectorPage(ctx)
	case path == "/examples" || path == "/examples/":
		s.handleExamplesPage(ctx)
	case path == "/keys":
		s.handleKeysPage(ctx)
	case path == "/usage":
		s.handleUsagePage(ctx)
	case path == "/credits" || path == "/billing":
		s.handleCreditsPage(ctx)
	case path == "/research" || path == "/research/":
		s.handleResearchIndex(ctx)
	case strings.HasPrefix(path, "/research/"):
		s.handleResearchPost(ctx, strings.TrimPrefix(path, "/research/"))
	// The visual record of every page, produced by scripts/visualbench.mjs.
	// Served from static/ so it needs no handler of its own beyond the redirect
	// that makes /visualbench work without the trailing slash.
	case path == "/visualbench" || path == "/visualbench/":
		redirectLocal(ctx, "/static/visualbench/index.html?v=20260802")
	case path == "/robots.txt":
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.Response.Header.Set("Cache-Control", "public, max-age=3600")
		ctx.WriteString("User-agent: *\nAllow: /\nSitemap: " + s.cfg.SiteBaseURL + "/sitemap.xml\n")
	case path == "/sitemap.xml":
		s.handleSitemap(ctx)
	default:
		ctx.SetStatusCode(404)
		s.render(ctx, "404.html", map[string]any{"Meta": s.meta("Not found", "")})
	}
}

func (s *Server) handleHealth(ctx *fasthttp.RequestCtx) {
	health, err := s.runner.health()
	vectorHealth, vectorErr := s.vector.health()
	dbErr := s.db.Ping()
	out := map[string]any{"ok": err == nil && dbErr == nil, "service": "mojojojo", "database": "ok"}
	if dbErr != nil {
		out["database"] = dbErr.Error()
		ctx.SetStatusCode(503)
	}
	if err != nil {
		out["runner"] = err.Error()
		ctx.SetStatusCode(503)
	} else {
		out["runner"] = health
	}
	// Vector search scales independently. A vector outage is reported here but
	// does not turn a healthy code-execution API into a 503.
	if vectorErr != nil {
		out["vector"] = map[string]any{"ok": false, "error": vectorErr.Error()}
	} else {
		out["vector"] = vectorHealth
	}
	sendJSON(ctx, out)
}

func (s *Server) handlePricing(ctx *fasthttp.RequestCtx) {
	sendJSON(ctx, map[string]any{
		"unit":                 "millisecond",
		"ms_per_credit":        s.cfg.MsPerCredit,
		"ms_per_credit_gpu":    s.cfg.MsPerCreditGPU,
		"usd_per_credit":       0.001,
		"free_ms_per_day":      s.cfg.FreeMsPerDay,
		"free_timeout_ms":      s.cfg.FreeTimeoutMs,
		"max_timeout_ms":       s.cfg.MaxTimeoutMs,
		"browser_wasm":         "free — runs on the caller's machine, never billed",
		"not_billed":           []string{"queue time", "environment builds", "our compiler", "cold starts"},
		"credits_purchased_at": s.cfg.AccountURL,
		"storage": map[string]any{"included_bytes": s.cfg.StorageFreeBytes,
			"gb_month_usd": s.cfg.StorageGBMonthUSD, "delivery": "included",
			"max_upload_bytes": s.cfg.StorageMaxUpload},
		"vector_search": vectorPricing(s.cfg),
		"compute_modes": map[string]any{
			"scale_to_zero": map[string]any{"status": "available", "billing": "measured execution milliseconds", "idle_charge": false},
			"persistent": map[string]any{"status": "limited_preview", "billing": "hourly in advance", "idle_charge": true,
				"profiles": []map[string]any{
					{"id": "cpu-nano", "cpu": 0.25, "memory_mib": 512, "credits_per_hour": 200, "usd_per_hour": 0.20},
					{"id": "cpu-1", "cpu": 1, "memory_mib": 2048, "credits_per_hour": 750, "usd_per_hour": 0.75},
					{"id": "cpu-2", "cpu": 2, "memory_mib": 4096, "credits_per_hour": 1400, "usd_per_hour": 1.40},
					{"id": "gpu-24", "cpu": 4, "memory_mib": 16384, "gpu_memory_mib": 24576, "credits_per_hour": 3500, "usd_per_hour": 3.50},
				},
			},
		},
	})
}

func (s *Server) handleMe(ctx *fasthttp.RequestCtx) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendJSON(ctx, map[string]any{"signed_in": false, "login": s.cfg.LoginURL})
		return
	}
	sendJSON(ctx, map[string]any{
		"signed_in": true,
		"user":      map[string]any{"id": u.ID, "name": u.DisplayName(), "email": u.Email},
		"credits":   u.Credits(),
		"owed_ms":   round3(s.bill.owed(u.ID)),
		"total_ms":  round3(s.bill.totalMs(u.ID)),
		"via":       u.Via,
	})
}

func (s *Server) handleUsage(ctx *fasthttp.RequestCtx) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	rows, err := s.bill.recent(u.ID, 100)
	if err != nil {
		sendError(ctx, 500, err.Error())
		return
	}
	sendJSON(ctx, map[string]any{
		"credits": u.Credits(), "owed_ms": round3(s.bill.owed(u.ID)),
		"total_ms": round3(s.bill.totalMs(u.ID)), "runs": rows,
	})
}

// handleCredits is the machine view of the same page: what is left, what it
// buys, where it went, and the URLs for getting more. The SDK polls this to
// decide whether a long job can afford to start.
func (s *Server) handleCredits(ctx *fasthttp.RequestCtx) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendJSON(ctx, map[string]any{
			"signed_in": false, "login": s.cfg.LoginURL, "signup": s.cfg.SignupURL,
			"top_up": s.cfg.TopUpURL, "welcome_credits": s.cfg.WelcomeCredits,
		})
		return
	}
	ledger, err := s.auth.ledger(u.ID, 50)
	if err != nil {
		sendError(ctx, 500, err.Error())
		return
	}
	credits := u.Credits()
	spent, earned := s.auth.ledgerTotals(u.ID)
	sendJSON(ctx, map[string]any{
		"signed_in": true,
		"credits":   credits,
		"funded":    u.Funded(),
		"balance": map[string]any{
			"free": u.FreeCredit, "plan": u.PlanCredit, "paid": u.PaidCredit,
			"usd": round3(float64(credits) * usdPerCredit),
		},
		"buys": map[string]any{
			"cpu_ms": float64(credits) * s.cfg.MsPerCredit,
			"gpu_ms": float64(credits) * s.cfg.MsPerCreditGPU,
		},
		"owed_ms":         round3(s.bill.owed(u.ID)),
		"total_ms":        round3(s.bill.totalMs(u.ID)),
		"ms_per_credit":   s.cfg.MsPerCredit,
		"welcome_granted": s.bill.granted(u.ID),
		"welcome_credits": s.cfg.WelcomeCredits,
		"spent_credits":   spent,
		"granted_credits": earned,
		"ledger":          ledger,
		"top_up":          s.cfg.TopUpURL,
		"account":         s.cfg.AccountURL,
	})
}

func (s *Server) handleSitemap(ctx *fasthttp.RequestCtx) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	// Product and discovery pages come first. The example entrypoints below are
	// complete public apps, not source files; listing them lets search engines
	// discover the useful thing a visitor can open directly.
	for _, page := range []struct{ path, priority string }{
		{"/", "1.0"}, {"/studio", "0.9"}, {"/cloud", "0.9"}, {"/agent", "0.9"},
		{"/apps", "0.9"}, {"/tools", "0.9"}, {"/examples", "0.9"},
		{"/data", "0.8"}, {"/vector", "0.8"}, {"/editor", "0.8"},
		{"/pricing", "0.8"}, {"/docs", "0.8"},
	} {
		b.WriteString("<url><loc>" + html.EscapeString(s.cfg.SiteBaseURL+page.path) +
			"</loc><lastmod>2026-08-04</lastmod><priority>" + page.priority + "</priority></url>\n")
	}
	for _, slug := range []string{
		"qr-forge", "cron-lens", "phone-passport", "photo-twin", "json-to-csv",
		"png-to-webp", "mojo-serverless", "mojojojo-style-transfer",
		"mojojojo-fractal", "multi-mojo-libs",
	} {
		b.WriteString("<url><loc>" + html.EscapeString(s.cfg.SiteBaseURL+"/examples/source/"+slug+"/index.html") +
			"</loc><lastmod>2026-08-04</lastmod><priority>0.7</priority></url>\n")
	}
	// Long-form project notes deliberately come last, after the product and the
	// apps they explain.
	b.WriteString("<url><loc>" + html.EscapeString(s.cfg.SiteBaseURL+"/research") +
		"</loc><lastmod>2026-08-04</lastmod><priority>0.6</priority></url>\n")
	for _, post := range posts {
		b.WriteString("<url><loc>" + html.EscapeString(s.cfg.SiteBaseURL+"/research/"+post.Slug) +
			"</loc><lastmod>" + post.Date + "</lastmod><priority>0.5</priority></url>\n")
	}
	b.WriteString("</urlset>\n")
	ctx.SetContentType("text/xml; charset=utf-8")
	ctx.Response.Header.Set("Cache-Control", "public, max-age=3600")
	ctx.WriteString(b.String())
}

func sendJSON(ctx *fasthttp.RequestCtx, v any) {
	ctx.SetContentType("application/json; charset=utf-8")
	if err := json.NewEncoder(ctx).Encode(v); err != nil {
		log.Printf("encode: %v", err)
	}
}

func sendError(ctx *fasthttp.RequestCtx, status int, msg string) {
	ctx.SetStatusCode(status)
	sendJSON(ctx, map[string]any{"ok": false, "error": msg})
}
