package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

type creatorFx struct {
	s     *Server
	owner *User
	n     int
}

func newCreatorFx(t *testing.T) *creatorFx {
	t.Helper()
	s := authTestServer(t)
	s.cfg.WelcomeCredits = 0
	var err error
	owner, err := s.auth.createUser("creator@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	s.bill, err = newBilling(s.db, s.auth, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO projects(id,user_id,name,slug,visibility,created_at,updated_at) VALUES ('app1',$1,'App','app1','public',$2,$2)`, owner.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return &creatorFx{s: s, owner: owner}
}

func (f *creatorFx) caller(t *testing.T, free, plan, paid int) *User {
	t.Helper()
	f.n++
	u, err := f.s.auth.createUser("caller"+string(rune('a'+f.n))+"@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE users SET free_credits=$2, plan_credits=$3, paid_credits=$4 WHERE id=$1`, u.ID, free, plan, paid); err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *creatorFx) sum(t *testing.T) creatorSummary {
	t.Helper()
	sum, err := f.s.creatorEarningsFor(f.s.db, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestPriceCreditsManifestRules(t *testing.T) {
	mk := func(fn string) map[string]string {
		return map[string]string{"index.html": "x", "functions/a.py": "def handler(r): return {}",
			"mojojojo.json": `{"runtime":"app","functions":{"a":{"language":"python","entrypoint":"functions/a.py",` + fn + `}}}`}
	}
	if _, m, err := readBundle(zipFixture(t, mk(`"billing":"caller","price_credits":25`))); err != nil || m.Functions["a"].PriceCredits != 25 {
		t.Fatalf("valid price: %+v %v", m, err)
	}
	for name, fn := range map[string]string{
		"owner-paid":  `"price_credits":25`,
		"negative":    `"billing":"caller","price_credits":-1`,
		"too large":   `"billing":"caller","price_credits":100001`,
		"owner bills": `"billing":"owner","price_credits":5`,
	} {
		if _, _, err := readBundle(zipFixture(t, mk(fn))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCreatorFeeSplitsAndOnlyPaidCreditsEarn(t *testing.T) {
	f := newCreatorFx(t)
	// 30 free + 70 paid: the fee takes free first, so 70 of 100 earn 80%
	c := f.caller(t, 30, 0, 100)
	if err := f.s.chargeCreatorFee(c, f.owner.ID, "app1", "gen", 100); err != nil {
		t.Fatal(err)
	}
	if got := f.sum(t); got.Pending != 56 || got.Lifetime != 56 || got.Available != 0 {
		t.Fatalf("mixed: %+v", got)
	}
	var left int
	f.s.db.QueryRow(`SELECT free_credits+plan_credits+paid_credits FROM users WHERE id=$1`, c.ID).Scan(&left)
	if left != 30 {
		t.Fatalf("caller has %d left, want 30", left)
	}
	// free-only caller earns the owner nothing but still pays
	free := f.caller(t, 500, 0, 0)
	if err := f.s.chargeCreatorFee(free, f.owner.ID, "app1", "gen", 500); err != nil {
		t.Fatal(err)
	}
	// owner calling their own function pays and earns nothing
	own := f.caller(t, 0, 0, 100)
	if err := f.s.chargeCreatorFee(own, own.ID, "app1", "gen", 100); err != nil {
		t.Fatal(err)
	}
	if got := f.sum(t); got.Lifetime != 56 {
		t.Fatalf("free and self calls earned: %+v", got)
	}
}

func TestCreatorFeeIsAllOrNothing(t *testing.T) {
	f := newCreatorFx(t)
	poor := f.caller(t, 0, 0, 40)
	if err := f.s.chargeCreatorFee(poor, f.owner.ID, "app1", "gen", 100); err != errCreatorFeeUnaffordable {
		t.Fatalf("unaffordable fee: %v", err)
	}
	var left int
	f.s.db.QueryRow(`SELECT paid_credits FROM users WHERE id=$1`, poor.ID).Scan(&left)
	if left != 40 {
		t.Fatalf("a refused fee still took credits: %d left", left)
	}
	if got := f.sum(t); got.Lifetime != 0 {
		t.Fatalf("refused fee booked an earning: %+v", got)
	}
}

func TestCreatorEarningsVestThenConvert(t *testing.T) {
	t.Setenv("CREATOR_HOLD_DAYS", "0")
	f := newCreatorFx(t)
	if err := f.s.chargeCreatorFee(f.caller(t, 0, 0, 1000), f.owner.ID, "app1", "gen", 1000); err != nil {
		t.Fatal(err)
	}
	key, _ := f.s.auth.createKey(f.owner.ID, "k")
	call := func(method, path, body string) (int, map[string]any) {
		ctx := newRequestCtxM(method, path, body)
		ctx.Request.Header.Set("Authorization", "Bearer "+key.Secret)
		f.s.route(ctx)
		var out map[string]any
		_ = json.Unmarshal(ctx.Response.Body(), &out)
		return ctx.Response.StatusCode(), out
	}
	if code, out := call("GET", "/v1/earnings", ""); code != 200 || out["earnings"].(map[string]any)["available_credits"].(float64) != 800 {
		t.Fatalf("summary: %d %v", code, out)
	}
	if code, _ := call("POST", "/v1/earnings/convert", `{"credits":801}`); code != 409 {
		t.Fatalf("over-convert: %d", code)
	}
	if code, _ := call("POST", "/v1/earnings/convert", `{"credits":-1}`); code != 400 {
		t.Fatalf("negative: %d", code)
	}
	code, out := call("POST", "/v1/earnings/convert", `{"credits":300}`)
	if code != 200 || out["balance_after"].(float64) != 300 {
		t.Fatalf("partial: %d %v", code, out)
	}
	code, out = call("POST", "/v1/earnings/convert", "")
	if code != 200 || out["converted_credits"].(float64) != 500 || out["balance_after"].(float64) != 800 {
		t.Fatalf("rest: %d %v", code, out)
	}
	if code, _ := call("POST", "/v1/earnings/convert", ""); code != 409 {
		t.Fatalf("nothing left: %d", code)
	}
	if got := f.sum(t); got.Available != 0 || got.Converted != 800 {
		t.Fatalf("final: %+v", got)
	}
}

func TestCreatorEarningsRequireAKeyAndHoldByDefault(t *testing.T) {
	f := newCreatorFx(t)
	if err := f.s.chargeCreatorFee(f.caller(t, 0, 0, 100), f.owner.ID, "app1", "gen", 100); err != nil {
		t.Fatal(err)
	}
	ctx := newRequestCtxM("GET", "/v1/earnings", "")
	f.s.route(ctx)
	if ctx.Response.StatusCode() != 401 {
		t.Fatalf("anonymous: %d", ctx.Response.StatusCode())
	}
	if got := f.sum(t); got.Pending != 80 || got.Available != 0 {
		t.Fatalf("default hold: %+v", got)
	}
	f.s.db.Exec(`UPDATE creator_earnings SET available_at = now() - interval '1 minute'`)
	if got := f.sum(t); got.Available != 80 {
		t.Fatalf("after vesting: %+v", got)
	}
	if strings.Contains(string(newRequestCtxM("GET", "/v1/earnings", "").Response.Body()), "creator") {
		t.Fatal("unexpected body")
	}
}

func newRequestCtxM(method, path, body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.SetHost("mojojojo.cc")
	ctx.Request.Header.Set("X-Forwarded-Proto", "https")
	if body != "" {
		ctx.Request.SetBodyString(body)
		ctx.Request.Header.SetContentType("application/json")
	}
	return ctx
}

func TestShowcaseBadgesPricedAppsWithoutRevealingEarnings(t *testing.T) {
	f := newCreatorFx(t)
	if _, err := f.s.db.Exec(`UPDATE projects SET showcase_status='approved', category='app', active_deployment_id='d1' WHERE id='app1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO deployments(id,project_id,user_id,status,manifest,public_url,created_at) VALUES ('d1','app1',$1,'ready',$2,'https://app1.example',now())`,
		f.owner.ID, `{"functions":{"a":{"language":"python","entrypoint":"a.py","billing":"caller","price_credits":40},"b":{"language":"python","entrypoint":"b.py","billing":"caller","price_credits":25},"c":{"language":"python","entrypoint":"c.py"}}}`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.s.chargeCreatorFee(f.caller(t, 0, 0, 100), f.owner.ID, "app1", "b", 25); err != nil {
			t.Fatal(err)
		}
	}
	ctx := newRequestCtxM("GET", "/v1/showcase?category=app", "")
	f.s.route(ctx)
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(ctx.Response.Body(), &out); err != nil || len(out.Items) != 1 {
		t.Fatalf("showcase: %v %s", err, ctx.Response.Body())
	}
	it := out.Items[0]
	if it["price_credits"].(float64) != 25 || it["paying_callers_30d"].(float64) != 2 {
		t.Fatalf("badge fields: %v", it)
	}
	if strings.Contains(string(ctx.Response.Body()), "share") || strings.Contains(string(ctx.Response.Body()), "earn") {
		t.Fatalf("showcase leaks earnings: %s", ctx.Response.Body())
	}
	if minPriceCredits(`{"functions":{"x":{"language":"python","entrypoint":"x.py","billing":"owner","price_credits":9}}}`) != 0 || minPriceCredits("not json") != 0 {
		t.Fatal("owner-paid or invalid manifests must not be priced")
	}
}
