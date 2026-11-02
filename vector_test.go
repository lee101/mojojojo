package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func vectorWorkerFixture(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var upserts atomic.Int64
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{"ok":true,"search_backend":"mojo-int8-exact","gpu":false}`))
		case strings.HasSuffix(r.URL.Path, "/upsert"):
			upserts.Add(1)
			_, _ = w.Write([]byte(`{"ok":true,"vector_count":2}`))
		case strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"ok":true,"matches":[{"id":"mojo","score":0.91,"metadata":{"lang":"mojo"}}],"vector_count":2,"backend":"mojo-int8-exact","latency_ms":0.71}`))
		case strings.HasSuffix(r.URL.Path, "/index") && r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.Error(w, `{"error":"not found"}`, 404)
		}
	}))
	t.Cleanup(worker.Close)
	return worker, &upserts
}

func vectorGPUWorkerFixture(t *testing.T) *httptest.Server {
	t.Helper()
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"ok":true,"search_backend":"cuvs-cagra-gpu","gpu":true}`))
			return
		}
		http.Error(w, `{"error":"not found"}`, 404)
	}))
	t.Cleanup(worker.Close)
	return worker
}

func vectorTestServer(t *testing.T) (*Server, string, *atomic.Int64) {
	t.Helper()
	s := authTestServer(t)
	worker, upserts := vectorWorkerFixture(t)
	s.cfg.VectorURL = worker.URL
	s.cfg.VectorMaxDocuments = 1000
	s.cfg.VectorMaxDocumentBytes = 32768
	s.cfg.VectorCPUQueryCredits = 0.5
	s.cfg.VectorGPUQueryCredits = 1.5
	s.cfg.VectorUpsertCredits = 0.02
	s.vector = newVectorClient(worker.URL, "", worker.Client())
	bill, err := newBilling(s.db, s.auth, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.bill = bill
	u, err := s.auth.createUser("vector@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET paid_credits=1000 WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	token, err := s.auth.createSession(u.ID, "vector-test")
	if err != nil {
		t.Fatal(err)
	}
	return s, token, upserts
}

func vectorRequest(s *Server, token, method, path, body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.SetCookie(sessionCookie, token)
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBodyString(body)
	s.route(ctx)
	return ctx
}

func TestVectorProjectUpsertSearchUsageAndDelete(t *testing.T) {
	s, token, upserts := vectorTestServer(t)
	created := vectorRequest(s, token, "POST", "/v1/vector/projects", `{"name":"Mojo docs","tier":"performance"}`)
	if created.Response.StatusCode() != 201 {
		t.Fatalf("create: %d %s", created.Response.StatusCode(), created.Response.Body())
	}
	var creation struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := json.Unmarshal(created.Response.Body(), &creation); err != nil || creation.Project.ID == "" {
		t.Fatalf("decode project: %v %s", err, created.Response.Body())
	}
	projectID := creation.Project.ID
	upsert := vectorRequest(s, token, "POST", "/v1/vector/projects/"+projectID+"/upsert",
		`{"documents":[{"id":"mojo","text":"Mojo GPU kernels","metadata":{"lang":"mojo"}},{"id":"garden","text":"tomato plants"}]}`)
	if upsert.Response.StatusCode() != 200 || !bytes.Contains(upsert.Response.Body(), []byte(`"vector_count":2`)) {
		t.Fatalf("upsert: %d %s", upsert.Response.StatusCode(), upsert.Response.Body())
	}
	if upserts.Load() != 1 {
		t.Fatalf("worker received %d upserts, want one hydration batch", upserts.Load())
	}
	search := vectorRequest(s, token, "POST", "/v1/vector/projects/"+projectID+"/search",
		`{"query":"fast GPU code","limit":5}`)
	if search.Response.StatusCode() != 200 || !bytes.Contains(search.Response.Body(), []byte(`"backend":"mojo-int8-exact"`)) ||
		!bytes.Contains(search.Response.Body(), []byte(`"latency_ms":0.71`)) {
		t.Fatalf("search: %d %s", search.Response.StatusCode(), search.Response.Body())
	}
	usage := vectorRequest(s, token, "GET", "/v1/vector/projects/"+projectID+"/usage", "")
	if usage.Response.StatusCode() != 200 || !bytes.Contains(usage.Response.Body(), []byte(`"operation":"search"`)) {
		t.Fatalf("usage: %d %s", usage.Response.StatusCode(), usage.Response.Body())
	}
	deleted := vectorRequest(s, token, "DELETE", "/v1/vector/projects/"+projectID, "")
	if deleted.Response.StatusCode() != 200 {
		t.Fatalf("delete: %d %s", deleted.Response.StatusCode(), deleted.Response.Body())
	}
}

func TestVectorAPIRejectsOversizedBatchAndOtherTenant(t *testing.T) {
	s, token, _ := vectorTestServer(t)
	created := vectorRequest(s, token, "POST", "/v1/vector/projects", `{"name":"Private"}`)
	var creation struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	_ = json.Unmarshal(created.Response.Body(), &creation)
	other, err := s.auth.createUser("other-vector@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := s.auth.createSession(other.ID, "tenant-test")
	if err != nil {
		t.Fatal(err)
	}
	foreign := vectorRequest(s, otherToken, "POST", "/v1/vector/projects/"+creation.Project.ID+"/search", `{"query":"secret"}`)
	if foreign.Response.StatusCode() != 404 {
		t.Fatalf("cross-tenant search status=%d body=%s", foreign.Response.StatusCode(), foreign.Response.Body())
	}
	documents := make([]vectorDocument, vectorBatchLimit+1)
	for i := range documents {
		documents[i] = vectorDocument{ID: string(rune('a' + i%26)), Text: "x"}
	}
	body, _ := json.Marshal(map[string]any{"documents": documents})
	tooMany := vectorRequest(s, token, "POST", "/v1/vector/projects/"+creation.Project.ID+"/upsert", string(body))
	if tooMany.Response.StatusCode() != 400 {
		t.Fatalf("oversized batch status=%d body=%s", tooMany.Response.StatusCode(), tooMany.Response.Body())
	}
}

func TestVectorPricingMathUsesCreditDenomination(t *testing.T) {
	cfg := Config{VectorCPUQueryCredits: 0.5, VectorGPUQueryCredits: 1.5,
		VectorUpsertCredits: 0.02, VectorMaxDocuments: 250000}
	pricing := vectorPricing(cfg)
	if pricing["cpu_exact_usd_per_1000_queries"] != 0.5 ||
		pricing["gpu_ann_usd_per_1000_queries"] != 1.5 ||
		pricing["usd_per_1000_documents_upserted"] != 0.02 {
		t.Fatalf("unexpected pricing: %#v", pricing)
	}
}

func TestReservedVectorCapacityChargesRenewsRoutesAndCancels(t *testing.T) {
	s, token, _ := vectorTestServer(t)
	worker := vectorGPUWorkerFixture(t)
	s.vector = newVectorClient(worker.URL, "", worker.Client())
	s.cfg.ReservedVectorCredits = 100
	s.cfg.ReservedVectorSlots = 1
	var userID string
	if err := s.db.QueryRow(`SELECT user_id FROM sessions WHERE id_hash=$1`, hashToken(token)).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET paid_credits=5000 WHERE id=$1`, userID); err != nil {
		t.Fatal(err)
	}

	created := vectorRequest(s, token, "POST", "/v1/vector/reservations", `{"product":"vector-performance"}`)
	if created.Response.StatusCode() != 201 {
		t.Fatalf("reserve: %d %s", created.Response.StatusCode(), created.Response.Body())
	}
	var response struct {
		Reservation capacityReservation `json:"reservation"`
	}
	if err := json.Unmarshal(created.Response.Body(), &response); err != nil || response.Reservation.ID == "" {
		t.Fatalf("decode reservation: %v %s", err, created.Response.Body())
	}
	if !s.hasActiveVectorReservation(userID) {
		t.Fatal("new reservation is not active")
	}
	client, location, release := s.pickVectorBackend(&vectorProject{Tier: "performance"}, userID)
	release()
	if client != s.vector || location != "reserved" {
		t.Fatalf("reserved placement = %q, want reserved local GPU", location)
	}
	var balance int
	if err := s.db.QueryRow(`SELECT free_credits+plan_credits+paid_credits FROM users WHERE id=$1`, userID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 5900 {
		t.Fatalf("balance after first hour = %d, want 5900", balance)
	}

	now := time.Now().UTC()
	if _, err := s.db.Exec(`UPDATE capacity_reservations SET next_charge_at=$1 WHERE id=$2`, now.Add(-time.Second), response.Reservation.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCapacityReservations(now); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT free_credits+plan_credits+paid_credits FROM users WHERE id=$1`, userID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 5800 {
		t.Fatalf("balance after renewal = %d, want 5800", balance)
	}
	var charges int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM credit_ledger WHERE user_id=$1 AND reason LIKE 'capacity:%'`, userID).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if charges != 2 {
		t.Fatalf("capacity ledger entries = %d, want 2", charges)
	}

	cancelled := vectorRequest(s, token, "DELETE", "/v1/vector/reservations/"+response.Reservation.ID, "")
	if cancelled.Response.StatusCode() != 200 || !s.hasActiveVectorReservation(userID) {
		t.Fatalf("cancel: %d %s", cancelled.Response.StatusCode(), cancelled.Response.Body())
	}
	if _, err := s.db.Exec(`UPDATE capacity_reservations SET next_charge_at=$1 WHERE id=$2`, now.Add(-time.Second), response.Reservation.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCapacityReservations(now); err != nil {
		t.Fatal(err)
	}
	if s.hasActiveVectorReservation(userID) {
		t.Fatal("reservation stayed active after its cancelled paid period")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM credit_ledger WHERE user_id=$1 AND reason LIKE 'capacity:%'`, userID).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if charges != 2 {
		t.Fatalf("cancellation caused an extra renewal: %d ledger entries", charges)
	}
}

func TestReservedVectorCapacityRequiresAnAvailableGPU(t *testing.T) {
	s, token, _ := vectorTestServer(t)
	s.cfg.ReservedVectorCredits = 100
	created := vectorRequest(s, token, "POST", "/v1/vector/reservations", `{}`)
	if created.Response.StatusCode() != 503 {
		t.Fatalf("unavailable reserve: %d %s", created.Response.StatusCode(), created.Response.Body())
	}
}

func TestVectorWorkerBundleContainsPortableRuntime(t *testing.T) {
	s := &Server{}
	ctx := &fasthttp.RequestCtx{}
	s.handleVectorWorkerTarball(ctx)
	gz, err := gzip.NewReader(bytes.NewReader(ctx.Response.Body()))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		header, err := reader.Next()
		if err != nil {
			break
		}
		seen[header.Name] = true
	}
	for _, name := range []string{"vectorworker/server.py", "vectorworker/engine.py",
		"model/modelint8_512dim.safetensors", "model/tokenizer.json",
		"lib/libmojo_embed.so", "runtime/libKGENCompilerRTShared.so",
		"runtime/libAsyncRTMojoBindings.so"} {
		if !seen[name] {
			t.Errorf("vector worker bundle missing %s", name)
		}
	}
}
