package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/valyala/fasthttp"
)

func TestCallerPaidGPUManifest(t *testing.T) {
	files := map[string]string{
		"index.html":         "app",
		"functions/infer.py": "def handler(request): return request['body']",
		"mojojojo.json":      `{"runtime":"app","functions":{"infer":{"language":"python","entrypoint":"functions/infer.py","billing":"caller","gpu":true}}}`,
	}
	_, m, err := readBundle(zipFixture(t, files))
	if err != nil || m.Functions["infer"].Billing != "caller" || !m.Functions["infer"].GPU {
		t.Fatalf("caller-paid GPU manifest: %+v %v", m, err)
	}
	files["mojojojo.json"] = `{"runtime":"app","functions":{"infer":{"language":"python","entrypoint":"functions/infer.py","billing":"anonymous"}}}`
	if _, _, err := readBundle(zipFixture(t, files)); err == nil {
		t.Fatal("accepted unknown payer policy")
	}
}

func TestFoundryNumericExampleBundle(t *testing.T) {
	raw, err := os.ReadFile("examples/foundry-numeric/studio.json")
	if err != nil {
		t.Fatal(err)
	}
	var studio struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(raw, &studio); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, name := range studio.Files {
		body, err := os.ReadFile(filepath.Join("examples/foundry-numeric", name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(body)
	}
	_, manifest, err := readBundle(zipFixture(t, files))
	if err != nil || manifest.Functions["model"].Billing != "caller" {
		t.Fatalf("invalid Foundry bundle: %+v %v", manifest, err)
	}
}

func TestCallerPaidHostedGPUExecution(t *testing.T) {
	s := authTestServer(t)
	s.cfg.WelcomeCredits = 0
	s.cfg.MsPerCredit, s.cfg.MsPerCreditGPU = 1000, 100
	owner, err := s.auth.createUser("gpu-owner@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	caller, err := s.auth.createUser("gpu-caller@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE users SET paid_credits=100 WHERE id IN ($1,$2)`, owner.ID, caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.auth.createKey(caller.ID, "inference")
	if err != nil {
		t.Fatal(err)
	}
	s.bill, err = newBilling(s.db, s.auth, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	const source = "def handler(request): return {'body': {'prediction': 1}}"
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(source)))
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		fmt.Fprint(w, source)
	}))
	defer storage.Close()
	client, err := minio.New(strings.TrimPrefix(storage.URL, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4("fixture", "fixture", ""), Region: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	s.uploads = &uploadStore{client: client, bucket: "fixture"}
	_, err = s.db.Exec(`INSERT INTO projects(id,user_id,name,slug,visibility,active_deployment_id,created_at,updated_at)
		VALUES ('gpu-app',$1,'GPU app','gpu-app','public','gpu-deployment',$2,$2)`, owner.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO deployments(id,project_id,user_id,status,manifest,created_at)
		VALUES ('gpu-deployment','gpu-app',$1,'ready',$2,$3)`, owner.ID,
		`{"functions":{"infer":{"language":"python","entrypoint":"functions/infer.py","billing":"caller","gpu":true}}}`, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO assets(id,user_id,project_id,deployment_id,object_key,filename,content_type,size_bytes,public_url,created_at)
		VALUES ('gpu-source',$1,'gpu-app','gpu-deployment','source','functions/infer.py','text/plain',$2,'',$3)`, owner.ID, len(source), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if !req.GPU || req.TimeoutMs != publicFunctionTimeout || strings.Contains(req.Stdin, key.Secret) {
			t.Errorf("GPU request or secret boundary incorrect: gpu=%v timeout=%d", req.GPU, req.TimeoutMs)
		}
		runs++
		json.NewEncoder(w).Encode(RunResult{OK: true, BillableMs: 500,
			Stdout: `__MOJOJOJO_RESPONSE__{"body":{"prediction":1}}`})
	}))
	defer worker.Close()
	s.cfg.RunnerURL, s.cfg.EdgeURL = worker.URL, ""
	s.runner = newRunnerClient(worker.URL)
	call := func(method, token string) *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(method)
		ctx.Request.SetRequestURI("/p/gpu-app/api/infer")
		ctx.Request.Header.Set("Authorization", "Bearer "+token)
		ctx.Request.Header.SetContentType("application/json")
		ctx.Request.SetBodyString(`{"rows":[[1,2]]}`)
		s.handleHostedFunction(ctx, "gpu-app", "infer", false)
		return ctx
	}
	if ctx := call("POST", "invalid"); ctx.Response.StatusCode() != 401 || runs != 0 {
		t.Fatalf("invalid key executed compute: %d runs=%d", ctx.Response.StatusCode(), runs)
	}
	if ctx := call("GET", key.Secret); ctx.Response.StatusCode() != 405 || runs != 0 {
		t.Fatalf("GET executed caller-paid compute: %d runs=%d", ctx.Response.StatusCode(), runs)
	}
	ctx := call("POST", key.Secret)
	if ctx.Response.StatusCode() != 200 || runs != 1 || !strings.Contains(string(ctx.Response.Body()), "prediction") {
		t.Fatalf("inference failed: %d %s runs=%d", ctx.Response.StatusCode(), ctx.Response.Body(), runs)
	}
	if got := s.auth.userByID(caller.ID).Credits(); got != 95 {
		t.Fatalf("GPU caller balance = %d, want 95", got)
	}
	if got := s.auth.userByID(owner.ID).Credits(); got != 100 {
		t.Fatalf("charged the app owner: balance=%d", got)
	}
}

func TestHostedFunctionPayerDoesNotFallBackToOwner(t *testing.T) {
	s := authTestServer(t)
	owner, err := s.auth.createUser("model-owner@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	caller, err := s.auth.createUser("model-caller@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.auth.createKey(caller.ID, "model-client")
	if err != nil {
		t.Fatal(err)
	}
	fn := functionManifest{Billing: "caller"}
	for _, invalid := range []string{"", "invalid", "mjj_revoked"} {
		if got := s.hostedFunctionPayer(fn, false, owner.ID, invalid); got != nil {
			t.Fatal("invalid caller credentials fell back to the owner")
		}
	}
	if got := s.hostedFunctionPayer(fn, false, owner.ID, key.Secret); got == nil || got.ID != caller.ID {
		t.Fatal("valid API key did not select the caller")
	}
	if got := s.hostedFunctionPayer(fn, true, owner.ID, key.Secret); got == nil || got.ID != owner.ID {
		t.Fatal("preview selected a third-party payer")
	}
	for _, mode := range []string{"", "owner"} {
		if got := s.hostedFunctionPayer(functionManifest{Billing: mode}, false, owner.ID, key.Secret); got == nil || got.ID != owner.ID {
			t.Fatal("legacy owner billing changed")
		}
	}
}
