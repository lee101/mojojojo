package main

import (
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

func TestAccrueFreeAllowanceThenExactPricing(t *testing.T) {
	def := meterDef{ID: "x", Micro: 2000, Free: 100}
	c, owed, billed := accrue(def, 0, 100, 0)
	if c != 0 || owed != 0 || billed != 0 {
		t.Fatalf("free usage billed: %d %v %v", c, owed, billed)
	}
	c, owed, billed = accrue(def, 90, 20, 0)
	if billed != 10*2000 || c != 0 || owed != 20000 {
		t.Fatalf("straddle: %d %v %v", c, owed, billed)
	}
	c, owed, _ = accrue(def, 200, 1000, 999_500)
	if c != 2 || math.Abs(owed-999500) > 1e-6 {
		t.Fatalf("carry: %d %v", c, owed)
	}
}

func TestCatalogAndMeters(t *testing.T) {
	list, by := loadBusinesses()
	if len(list) < 40 || by["kv"].Slug != "kv" {
		t.Fatalf("catalog: %d", len(list))
	}
	if got := requestMeter(by["kv"].Parts, "GET", "/v1/x"); got != "db.read" {
		t.Errorf("kv GET = %q", got)
	}
	if got := requestMeter(by["kv"].Parts, "POST", "/v1/x"); got != "db.write" {
		t.Errorf("kv POST = %q", got)
	}
	if got := requestMeter(by["embed-box"].Parts, "POST", "/v1/search"); got != "search.query" {
		t.Errorf("embed-box = %q", got)
	}
	if len(metersFor(by["backup-buddy"].Parts)) == 0 {
		t.Error("backup-buddy has no meters")
	}
}

func TestBusinessSubdomainRouting(t *testing.T) {
	server := authTestServer(t)
	get := func(host, path string) *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI(path)
		ctx.Request.Header.SetHost(host)
		server.route(ctx)
		return ctx
	}
	ctx := get("paste-vault."+server.cfg.BusinessDomain, "/")
	if ctx.Response.StatusCode() != http.StatusOK || !strings.Contains(string(ctx.Response.Body()), "Paste Vault") {
		t.Fatalf("landing: %d", ctx.Response.StatusCode())
	}
	if c := get("paste-vault."+server.cfg.BusinessDomain, "/v1/x").Response.StatusCode(); c != 503 {
		t.Errorf("not-live API = %d, want 503", c)
	}
	if c := get("nosuch."+server.cfg.BusinessDomain, "/").Response.StatusCode(); c != 200 {
		t.Errorf("unknown subdomain should fall through to main site, got %d", c)
	}
}

func TestMeterSettlementMovesWholeCredits(t *testing.T) {
	b, _ := testBilling(t, 10)
	s := &Server{db: b.db, bill: b}
	s.meters = newMeterBuffer()
	def := meterDefs["db.write"]
	// 50000 free, then 600 billable writes at 2000 micro = 1_200_000 = 1 credit + 200000 owed.
	if err := s.settleMeter("u1", def, "2026-10", 50600); err != nil {
		t.Fatal(err)
	}
	var free int
	var owed float64
	_ = b.db.QueryRow(`SELECT free_credits FROM users WHERE id='u1'`).Scan(&free)
	_ = b.db.QueryRow(`SELECT owed_micro FROM usage_meters WHERE user_id='u1'`).Scan(&owed)
	if free != 9 || math.Abs(owed-200000) > 1e-6 {
		t.Fatalf("free=%d owed=%v", free, owed)
	}
}
