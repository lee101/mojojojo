package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseUnitsOnlyHonoursOwnedMeters(t *testing.T) {
	own := metersFor([]string{"model-embed"})
	got := parseUnits("model.embed.tokens=2.5, model.rerank.pairs=9, model.embed.tokens=0.5, bogus, model.embed.tokens=-1, model.embed.tokens=NaN, model.embed.tokens=1e12", own)
	if len(got) != 1 || got["model.embed.tokens"] != 3 {
		t.Fatalf("units = %v", got)
	}
}

func TestModelCatalogIsConsistent(t *testing.T) {
	list, _ := loadBusinesses()
	n := 0
	for _, b := range list {
		if b.Kind != "model" {
			continue
		}
		n++
		if modelMeter(b.Parts) == "" {
			t.Errorf("%s has no model meter", b.Slug)
		}
		for _, m := range metersFor(b.Parts) {
			if m.Micro <= 0 || m.ID == "" {
				t.Errorf("%s: bad meter %+v", b.Slug, m)
			}
		}
		if b.Example == "" || len(b.Endpoints) == 0 || len(b.Notes) == 0 || b.Upstream == "" || b.Status != "built" {
			t.Errorf("%s: incomplete model entry", b.Slug)
		}
		if got := requestMeter(b.Parts, "POST", "/v1/anything"); got != modelMeter(b.Parts) {
			t.Errorf("%s: requestMeter = %q", b.Slug, got)
		}
	}
	if n != 5 {
		t.Fatalf("model businesses = %d, want 5", n)
	}
}

func modelCall(t *testing.T, f *howFixture, host, path, body string) (int, string, http.Header) {
	t.Helper()
	c, err := f.conn.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s.%s\r\nContent-Length: %d\r\nContent-Type: application/json\r\nX-Forwarded-Proto: https\r\nAuthorization: Bearer %s\r\n\r\n%s",
		path, host, f.s.cfg.BusinessDomain, len(body), f.key, body)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestModelBusinessMetersReportedUnits(t *testing.T) {
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Mojo-Units", "model.embed.tokens=1500,model.rerank.pairs=99")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	f := newHowFixture(t, up.URL, 100)
	t.Setenv("MOJOJOJO_UPSTREAM_EMBED", up.URL)

	status, body, hdr := modelCall(t, f, "embed", "/v1/embeddings", `{"input":"x"}`)
	if status != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("status %d %s", status, body)
	}
	if hdr.Get("X-Mojo-Units") != "" {
		t.Errorf("usage header leaked to client: %v", hdr)
	}
	if seen.Get("X-Mojo-Business") != "embed" || seen.Get("X-Mojo-Tenant") != f.user.ID || seen.Get("Authorization") != "" {
		t.Errorf("gateway headers: %v", seen)
	}
	d := f.s.meters.drain()
	if d[meterKey{f.user.ID, "model.embed.tokens"}] != 1500 {
		t.Errorf("embed meter: %v", d)
	}
	if d[meterKey{f.user.ID, "model.rerank.pairs"}] != 0 {
		t.Errorf("another business's meter was billed: %v", d)
	}
	if len(d) != 1 {
		t.Errorf("unexpected meters: %v", d)
	}
}

func TestModelBusinessUnreportedRequestsAreFree(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer up.Close()
	f := newHowFixture(t, up.URL, 100)
	t.Setenv("MOJOJOJO_UPSTREAM_RERANK", up.URL)
	if status, _, _ := modelCall(t, f, "rerank", "/v1/rerank", `{}`); status != 200 {
		t.Fatalf("status %d", status)
	}
	if d := f.s.meters.drain(); len(d) != 0 {
		t.Errorf("billed without a usage report: %v", d)
	}
}

func TestModelBusinessErrorsAreNotBilled(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Mojo-Units", "model.moderate.images=3")
		w.WriteHeader(400)
	}))
	defer up.Close()
	f := newHowFixture(t, up.URL, 100)
	t.Setenv("MOJOJOJO_UPSTREAM_MODERATE", up.URL)
	if status, _, _ := modelCall(t, f, "moderate", "/v1/moderations/image", `{}`); status != 400 {
		t.Fatalf("status %d", status)
	}
	if d := f.s.meters.drain(); len(d) != 0 {
		t.Errorf("billed a failed request: %v", d)
	}
}

func TestModelBusinessAgentSurface(t *testing.T) {
	var gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		w.Header().Set("X-Mojo-Units", "model.score.rows=2")
		w.Write([]byte(`{"predictions":[]}`))
	}))
	defer up.Close()
	f := newHowFixture(t, up.URL, 100)
	t.Setenv("MOJOJOJO_UPSTREAM_SCORE", up.URL)

	get := func(path string) (int, string) {
		c, _ := f.conn.Dial()
		defer c.Close()
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: score.%s\r\nX-Forwarded-Proto: https\r\n\r\n", path, f.s.cfg.BusinessDomain)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, body := get("/openapi.json"); st != 200 || !strings.Contains(body, "/v1/models/{name}/predict") || !strings.Contains(body, "bearerAuth") {
		t.Errorf("openapi %d %.200s", st, body)
	}
	if st, body := get("/llms.txt"); st != 200 || !strings.Contains(body, "Rows trained or scored") {
		t.Errorf("llms %d %.200s", st, body)
	}
	if st, body := get("/.well-known/mcp.json"); st != 200 || !strings.Contains(body, "/mcp") {
		t.Errorf("manifest %d %.200s", st, body)
	}

	st, body, _ := modelCall(t, f, "score", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if st != 200 || !strings.Contains(body, "train_model") || !strings.Contains(body, "predict") {
		t.Fatalf("tools/list %d %s", st, body)
	}
	st, body, _ = modelCall(t, f, "score", "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"predict","arguments":{"name":"churn","rows":[{"a":1}]}}}`)
	if st != 200 || strings.Contains(body, `"isError":true`) {
		t.Fatalf("tools/call %d %s", st, body)
	}
	if gotPath != "/v1/models/churn/predict" || strings.Contains(gotBody, "churn") || !strings.Contains(gotBody, "rows") {
		t.Errorf("upstream saw %s %s", gotPath, gotBody)
	}
	if d := f.s.meters.drain(); d[meterKey{f.user.ID, "model.score.rows"}] != 2 {
		t.Errorf("mcp call not metered: %v", d)
	}
	// without a key the call is a tool error, not a transport error
	f.key = ""
	_, body, _ = modelCall(t, f, "score", "/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_models","arguments":{}}}`)
	if !strings.Contains(body, "API key required") {
		t.Errorf("unauthenticated call: %s", body)
	}
}

func TestMeterRowsReadLikeAPriceList(t *testing.T) {
	rows := meterRows([]string{"model-embed"})
	if len(rows) != 1 || rows[0].Price != "$0.02 per million tokens" || rows[0].Free != "1,000,000 tokens" {
		t.Errorf("embed row: %+v", rows)
	}
	r := meterRows([]string{"model-rerank"})[0]
	if r.Price != "$0.01 per thousand pairs" {
		t.Errorf("rerank price: %q", r.Price)
	}
	img := meterRows([]string{"model-moderate"})[0]
	if img.Price != "$0.001 per image" || img.Free != "100 images" {
		t.Errorf("image row: %+v", img)
	}
	kv := meterRows([]string{"kv"})
	if len(kv) == 0 || !strings.Contains(kv[0].Price, "per million") {
		t.Errorf("request meters keep their per-million wording: %+v", kv)
	}
}
