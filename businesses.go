package main

// mojo businesses: each catalog entry is a standalone metered API served on
// <slug>.<domain> (landing page here, /v1/* proxied to the mojocloud daemon
// with the tenant resolved and usage metered at this gateway). The daemon is a
// separate process, so a business whose upstream is not configured says so
// instead of pretending to be live.

import (
	_ "embed"
	"encoding/json"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/valyala/fasthttp"
)

//go:embed businesses.json
var businessesJSON []byte

type business struct {
	Slug     string   `json:"slug"`
	Title    string   `json:"title"`
	Pitch    string   `json:"pitch"`
	Kind     string   `json:"kind"`
	Parts    []string `json:"parts"`
	Repo     string   `json:"repo"`
	Status   string   `json:"status"`
	Upstream string   `json:"upstream,omitempty"`

	// Model businesses (Mojo-optimised models served by modeld) document
	// themselves from the catalog: the model, endpoints, a curl example and the
	// honest limits and benchmark notes.
	Model     string    `json:"model,omitempty"`
	Endpoints []string  `json:"endpoints,omitempty"`
	Example   string    `json:"example,omitempty"`
	Notes     []string  `json:"notes,omitempty"`
	Tools     []bizTool `json:"tools,omitempty"`

	// Playground is the try-it box on the landing page: a sample JSON body for
	// one endpoint, optionally with a file the visitor picks (sent as base64).
	Playground *bizPlayground `json:"playground,omitempty"`
}

type bizPlayground struct {
	Path string         `json:"path"`
	Body map[string]any `json:"body"`
	File *struct {
		Field  string `json:"field"`
		Array  bool   `json:"array"`
		Accept string `json:"accept"`
		Label  string `json:"label"`
	} `json:"file,omitempty"`
}

var (
	bizOnce   sync.Once
	bizList   []business
	bizBySlug map[string]business
)

func loadBusinesses() ([]business, map[string]business) {
	bizOnce.Do(func() {
		_ = json.Unmarshal(businessesJSON, &bizList)
		bizBySlug = make(map[string]business, len(bizList))
		for _, b := range bizList {
			bizBySlug[b.Slug] = b
		}
	})
	return bizList, bizBySlug
}

func (s *Server) businessLive(b business) bool {
	return s.upstreamFor(b).base != "" && b.Status == "built"
}

func (s *Server) businessJSON(b business) map[string]any {
	meters := make([]map[string]any, 0)
	for _, m := range metersFor(b.Parts) {
		meters = append(meters, map[string]any{"id": m.ID, "label": m.Label, "unit": m.Unit,
			"usd_per_unit": m.USD(), "free_per_month": m.Free})
	}
	return map[string]any{"slug": b.Slug, "title": b.Title, "pitch": b.Pitch, "kind": b.Kind,
		"host": b.Slug + "." + s.cfg.BusinessDomain, "status": b.Status, "live": s.businessLive(b),
		"repo": b.Repo, "meters": meters, "model": b.Model, "endpoints": b.Endpoints, "notes": b.Notes}
}

// businessHost returns the catalog slug when the request is for a business
// subdomain of the platform domain, else "".
func (s *Server) businessHost(ctx *fasthttp.RequestCtx) string {
	host := strings.ToLower(string(ctx.Host()))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, d := range s.cfg.BusinessDomains {
		if sub, ok := strings.CutSuffix(host, "."+d); ok && sub != "" && !strings.Contains(sub, ".") {
			if _, ok := bizBySlugOnce()[sub]; ok {
				return sub
			}
		}
	}
	return ""
}

func bizBySlugOnce() map[string]business { _, m := loadBusinesses(); return m }

func (s *Server) handleBusinessesPage(ctx *fasthttp.RequestCtx) {
	list, _ := loadBusinesses()
	items := make([]map[string]any, 0, len(list))
	built := 0
	for _, b := range list {
		j := s.businessJSON(b)
		j["Slug"], j["Title"], j["Pitch"], j["Kind"], j["Host"], j["Status"] = b.Slug, b.Title, b.Pitch, b.Kind, j["host"], b.Status
		if b.Status == "built" {
			built++
		}
		items = append(items, j)
	}
	// Live businesses first: a visitor should meet what works before what is planned.
	sort.SliceStable(items, func(i, j int) bool {
		li, _ := items[i]["live"].(bool)
		lj, _ := items[j]["live"].(bool)
		return li && !lj
	})
	s.render(ctx, "businesses.html", map[string]any{
		"Meta": s.metaAt("mojo businesses — metered Mojo APIs on their own subdomains",
			"Standalone, pay-per-use APIs written in Mojo: key-value, object storage, queues, forms, links, webhooks and more, billed from one credit balance.",
			"/businesses"),
		"User": s.auth.userFromSession(ctx), "Items": items, "Built": built, "Total": len(list),
		"Domain": s.cfg.BusinessDomain,
	})
}

func (s *Server) handleBusinessesAPI(ctx *fasthttp.RequestCtx) {
	list, _ := loadBusinesses()
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, s.businessJSON(b))
	}
	sendJSON(ctx, map[string]any{"ok": true, "businesses": out})
}

func (s *Server) handleBusinessAPI(ctx *fasthttp.RequestCtx, slug string) {
	b, ok := bizBySlugOnce()[strings.Trim(slug, "/")]
	if !ok {
		sendError(ctx, 404, "unknown business")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "business": s.businessJSON(b)})
}

func (s *Server) handleBusinessHost(ctx *fasthttp.RequestCtx, slug string) {
	b := bizBySlugOnce()[slug]
	path := string(ctx.Path())
	if strings.HasPrefix(path, "/static/") {
		staticFS(ctx)
		return
	}
	if path == "/hl.js" {
		s.serveJS(ctx, "web/hl.js")
		return
	}
	if strings.HasPrefix(path, "/web/") {
		webFS(ctx)
		return
	}
	if s.businessAccount(ctx, b, path) || s.businessAgentDocs(ctx, b, path) {
		return
	}
	if path == "/health" {
		sendJSON(ctx, map[string]any{"ok": true, "business": slug, "live": s.businessLive(b)})
		return
	}
	if !strings.HasPrefix(path, "/v1/") {
		j := s.businessJSON(b)
		s.render(ctx, "business.html", map[string]any{
			"Meta": s.metaAt(b.Title+" — "+b.Pitch, b.Pitch, "/"),
			"User": s.auth.userFromSession(ctx), "B": b, "Host": j["host"], "Live": j["live"],
			"Meters": metersFor(b.Parts), "MeterRows": meterRows(b.Parts), "Account": s.cfg.SiteBaseURL,
			"StripePage": s.stripe != nil, "PlaygroundJSON": playgroundJSON(b),
		})
		return
	}
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
	ctx.Response.Header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key")
	ctx.Response.Header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	if string(ctx.Method()) == "OPTIONS" {
		ctx.SetStatusCode(204)
		return
	}
	if !s.businessLive(b) {
		sendError(ctx, 503, b.Title+" is not live yet — "+b.Status)
		return
	}
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendError(ctx, 401, "API key required: create one at "+s.cfg.SiteBaseURL+"/keys")
		return
	}
	meter := requestMeter(b.Parts, string(ctx.Method()), path)
	if meter != "" && (s.meterBlocked(u, meter) || (meter == "how.ask" && s.meterBlocked(u, "how.tokens"))) {
		sendError(ctx, 402, "free allowance used — add credits at "+s.cfg.TopUpURL)
		return
	}
	if up := s.upstreamFor(b); up.own {
		if err := s.proxyOwn(ctx, b, u, up, meter); err != nil {
			sendError(ctx, 502, "upstream unavailable")
		}
		return
	}
	n, err := s.proxyBusiness(ctx, b, u)
	if err != nil {
		sendError(ctx, 502, "upstream unavailable")
		return
	}
	s.meterAdd(u.ID, meter, 1)
	if hasMeter(b.Parts, "obj.egress") && n > 0 {
		s.meterAdd(u.ID, "obj.egress", float64(n)/(1<<30))
	}
}

// modelMeter is the one meter a model business bills, or "". Model businesses
// bill by the units the upstream reports (tokens, pairs, images, minutes, rows),
// never by request, so this is what the gateway gates on before forwarding.
func modelMeter(parts []string) string {
	for _, p := range parts {
		if strings.HasPrefix(p, "model-") {
			if ids := partMeters[p]; len(ids) > 0 {
				return ids[0]
			}
		}
	}
	return ""
}

func hasMeter(parts []string, id string) bool {
	for _, m := range metersFor(parts) {
		if m.ID == id {
			return true
		}
	}
	return false
}

// requestMeter maps one request to the meter it consumes.
func requestMeter(parts []string, method, path string) string {
	if id := modelMeter(parts); id != "" {
		return id
	}
	if hasMeter(parts, "how.ask") {
		switch {
		case method == "POST" && path == "/v1/ask":
			return "how.ask"
		case method == "GET" && strings.HasPrefix(path, "/v1/search"):
			return "how.search"
		}
		return ""
	}
	read := method == "GET" || method == "HEAD"
	pick := func(ids ...string) string {
		for _, id := range ids {
			if hasMeter(parts, id) {
				return id
			}
		}
		return ""
	}
	switch {
	case strings.Contains(path, "/search") && hasMeter(parts, "search.query"):
		return "search.query"
	case read:
		return pick("db.read", "obj.get")
	default:
		return pick("db.write", "obj.put", "queue.job")
	}
}

var cloudClient = &fasthttp.Client{MaxConnsPerHost: 256, ReadTimeout: 60e9, WriteTimeout: 60e9}

// proxyBusiness forwards to the mojocloud daemon. Client-supplied X-Mojo-*
// headers are dropped; the gateway alone names the tenant and the business.
func (s *Server) proxyBusiness(ctx *fasthttp.RequestCtx, b business, u *User) (int, error) {
	base, err := url.Parse(s.cfg.CloudURL)
	if err != nil {
		return 0, err
	}
	req, resp := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	ctx.Request.CopyTo(req)
	req.Header.DisableNormalizing()
	stripClientMojoHeaders(req)
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.SetRequestURI(base.Scheme + "://" + base.Host + string(ctx.RequestURI()))
	req.Header.SetHost(base.Host)
	req.Header.Set("X-Mojo-Business", b.Slug)
	req.Header.Set("X-Mojo-Tenant", u.ID)
	req.Header.Set("X-Mojo-Token", s.cfg.CloudToken)
	if err := cloudClient.Do(req, resp); err != nil {
		return 0, err
	}
	ctx.SetStatusCode(resp.StatusCode())
	ctx.SetContentTypeBytes(resp.Header.ContentType())
	ctx.SetBody(resp.Body())
	return len(resp.Body()), nil
}

func playgroundJSON(b business) string {
	if b.Playground == nil {
		return "null"
	}
	out, _ := json.Marshal(b.Playground)
	return string(out)
}

type meterRow struct{ Label, Price, Free string }

// meterRows words each meter the way a buyer reads a price list: embeddings per
// million tokens, other "1k" units per thousand, everything else per unit.
func meterRows(parts []string) []meterRow {
	money := func(v float64) string {
		if v >= 0.01 {
			return "$" + strconv.FormatFloat(v, 'f', 2, 64)
		}
		return "$" + strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 7, 64), "0"), ".")
	}
	num := func(v float64) string {
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 2, 64), "0"), ".")
	}
	var out []meterRow
	for _, m := range metersFor(parts) {
		row := meterRow{Label: m.Label}
		if noun, ok := strings.CutPrefix(m.Unit, "1k "); ok {
			if noun == "tokens" {
				row.Price = money(m.USD()*1000) + " per million tokens"
				row.Free = commas(m.Free*1000) + " tokens"
			} else {
				row.Price = money(m.USD()) + " per thousand " + noun
				row.Free = commas(m.Free*1000) + " " + noun
			}
		} else if m.Unit == "request" || m.Unit == "query" || m.Unit == "job" || m.Unit == "run" {
			row.Price = money(m.USD()*1e6) + " per million " + m.Unit + "s"
			row.Free = commas(m.Free) + " " + m.Unit + "s"
		} else {
			row.Price = money(m.USD()) + " per " + m.Unit
			row.Free = num(m.Free) + " " + m.Unit + "s"
			if m.Free == 1 {
				row.Free = "1 " + m.Unit
			}
		}
		out = append(out, row)
	}
	return out
}

func commas(v float64) string {
	s := strconv.FormatFloat(math.Round(v), 'f', 0, 64)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
