package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func TestAppManifestPinsPythonAndMojoFunctions(t *testing.T) {
	body := zipFixture(t, map[string]string{
		"index.html":           "<!doctype html><title>tool</title>",
		"functions/convert.py": "def handler(request): return request['body']",
		"functions/hello.mojo": "def main(): print(\"hello\")",
		"mojojojo.json": `{"runtime":"app","entrypoint":"index.html","functions":{
			"convert":{"language":"python","entrypoint":"functions/convert.py"},
			"hello":{"language":"mojo","entrypoint":"functions/hello.mojo"}}}`,
	})
	_, manifest, err := readBundle(body)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Runtime != "app" || len(manifest.Functions) != 2 ||
		manifest.Functions["convert"].Language != "python" {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
}

func TestOneClickExamplesAreStudioLoadableAndDeployable(t *testing.T) {
	for _, slug := range []string{"qr-forge", "cron-lens", "phone-passport", "photo-twin"} {
		t.Run(slug, func(t *testing.T) {
			root := filepath.Join("examples", slug)
			raw, err := os.ReadFile(filepath.Join(root, "studio.json"))
			if err != nil {
				t.Fatal(err)
			}
			var studio struct {
				Files []string `json:"files"`
			}
			if json.Unmarshal(raw, &studio) != nil || len(studio.Files) < 4 {
				t.Fatalf("invalid Studio file list: %s", raw)
			}
			files := map[string]string{}
			for _, name := range studio.Files {
				if !validProjectPath(name) {
					t.Fatalf("unsafe Studio path %q", name)
				}
				body, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
				if readErr != nil {
					t.Fatalf("Studio file %q: %v", name, readErr)
				}
				files[name] = string(body)
			}
			_, manifest, err := readBundle(zipFixture(t, files))
			if err != nil {
				t.Fatalf("bundle is not deployable: %v", err)
			}
			if manifest.Runtime != "app" || len(manifest.Functions) != 1 {
				t.Fatalf("unexpected app manifest: %#v", manifest)
			}
			readme, err := os.ReadFile(filepath.Join(root, "README.md"))
			if err != nil || !bytes.Contains(readme, []byte("/studio?example="+slug)) {
				t.Fatalf("README has no one-click Studio link: %v", err)
			}
		})
	}
}

func TestAppManifestRejectsMissingOrUnknownFunctions(t *testing.T) {
	for name, manifest := range map[string]string{
		"missing":  `{"runtime":"app","functions":{"x":{"language":"python","entrypoint":"functions/x.py"}}}`,
		"language": `{"runtime":"app","functions":{"x":{"language":"javascript","entrypoint":"index.html"}}}`,
		"name":     `{"runtime":"app","functions":{"Not Safe":{"language":"python","entrypoint":"index.html"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := readBundle(zipFixture(t, map[string]string{
				"index.html": "ok", "mojojojo.json": manifest,
			})); err == nil {
				t.Fatal("invalid function manifest was accepted")
			}
		})
	}
}

func TestAppManifestMakesComputeModeExplicit(t *testing.T) {
	base := map[string]string{
		"index.html":    "ok",
		"mojojojo.json": `{"runtime":"app","compute":{"mode":"scale-to-zero"}}`,
	}
	_, manifest, err := readBundle(zipFixture(t, base))
	if err != nil || manifest.Compute.Mode != "scale-to-zero" {
		t.Fatalf("scale-to-zero manifest was not accepted: mode=%q err=%v", manifest.Compute.Mode, err)
	}
	base["mojojojo.json"] = `{"runtime":"app","compute":{"mode":"persistent","profile":"cpu-1"}}`
	if _, _, err = readBundle(zipFixture(t, base)); err == nil || !strings.Contains(err.Error(), "limited preview") {
		t.Fatalf("persistent deployment must fail clearly until the supervisor is live: %v", err)
	}
}

func TestPythonFunctionAdapterUsesRequestAndResponseEnvelope(t *testing.T) {
	code := pythonFunctionCode(`def handler(request):
    return {"status": 201, "headers": {"Content-Type": "text/plain"}, "body": request["body"]["value"]}`)
	cmd := exec.Command("python3", "-c", code)
	cmd.Stdin = strings.NewReader(`{"body":{"value":"worked"}}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("adapter failed: %v\n%s", err, out)
	}
	marker := bytes.TrimPrefix(bytes.TrimSpace(out), []byte("__MOJOJOJO_RESPONSE__"))
	var response hostedResponse
	if json.Unmarshal(marker, &response) != nil || response.Status != 201 || response.Body != "worked" {
		t.Fatalf("unexpected response: %s", out)
	}
}

func TestProjectWorkspaceIsAtomicAndOwnerScoped(t *testing.T) {
	s := authTestServer(t)
	u, _ := s.auth.createUser("studio@example.com", "correct horse battery staple")
	token, _ := s.auth.createSession(u.ID, "studio-test")
	request := func(method, uri, body string) *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(method)
		ctx.Request.SetRequestURI(uri)
		ctx.Request.Header.SetCookie(sessionCookie, token)
		ctx.Request.Header.SetContentType("application/json")
		ctx.Request.SetBodyString(body)
		s.route(ctx)
		return ctx
	}
	created := request("POST", "/v1/projects", `{"name":"Converter","runtime":"app","category":"tool"}`)
	var payload struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	_ = json.Unmarshal(created.Response.Body(), &payload)
	if payload.Project.ID == "" {
		t.Fatalf("create failed: %s", created.Response.Body())
	}
	files := `{"files":{"index.html":"<h1>ok</h1>","mojojojo.json":"{\"runtime\":\"app\"}"}}`
	saved := request("PUT", "/v1/projects/"+payload.Project.ID+"/files", files)
	if saved.Response.StatusCode() != 200 {
		t.Fatalf("save failed: %d %s", saved.Response.StatusCode(), saved.Response.Body())
	}
	var firstSave struct {
		Changed   bool   `json:"changed"`
		Revision  string `json:"revision"`
		VersionID string `json:"version_id"`
	}
	_ = json.Unmarshal(saved.Response.Body(), &firstSave)
	if !firstSave.Changed || firstSave.VersionID == "" {
		t.Fatalf("first content save did not create a version: %s", saved.Response.Body())
	}
	unchanged := request("PUT", "/v1/projects/"+payload.Project.ID+"/files", files)
	var noOp struct {
		Changed   bool   `json:"changed"`
		VersionID string `json:"version_id"`
	}
	_ = json.Unmarshal(unchanged.Response.Body(), &noOp)
	if unchanged.Response.StatusCode() != 200 || noOp.Changed || noOp.VersionID != firstSave.VersionID {
		t.Fatalf("identical save was not a no-op: %d %s", unchanged.Response.StatusCode(), unchanged.Response.Body())
	}
	var versionCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM project_versions WHERE project_id=$1`, payload.Project.ID).Scan(&versionCount)
	if versionCount != 1 {
		t.Fatalf("identical save created versions: got %d want 1", versionCount)
	}
	loaded := request("GET", "/v1/projects/"+payload.Project.ID+"/files", "")
	var workspace struct {
		Files    map[string]string `json:"files"`
		Revision string            `json:"revision"`
	}
	_ = json.Unmarshal(loaded.Response.Body(), &workspace)
	if workspace.Files["index.html"] != "<h1>ok</h1>" {
		t.Fatalf("workspace did not round trip: %s", loaded.Response.Body())
	}
	if workspace.Revision == "" {
		t.Fatalf("workspace has no revision: %s", loaded.Response.Body())
	}
	nextBody, _ := json.Marshal(map[string]any{"revision": workspace.Revision, "files": map[string]string{
		"index.html": "<h1>new</h1>", "mojojojo.json": `{"runtime":"app"}`,
	}})
	next := request("PUT", "/v1/projects/"+payload.Project.ID+"/files", string(nextBody))
	if next.Response.StatusCode() != 200 {
		t.Fatalf("revisioned save failed: %d %s", next.Response.StatusCode(), next.Response.Body())
	}
	var nextWorkspace struct {
		Revision string `json:"revision"`
	}
	_ = json.Unmarshal(next.Response.Body(), &nextWorkspace)
	freshBody, _ := json.Marshal(map[string]any{"revision": nextWorkspace.Revision, "files": map[string]string{
		"index.html": "<h1>newer</h1>", "mojojojo.json": `{"runtime":"app"}`,
	}})
	fresh := request("PUT", "/v1/projects/"+payload.Project.ID+"/files", string(freshBody))
	if fresh.Response.StatusCode() != 200 {
		t.Fatalf("returned revision could not be reused: %d %s", fresh.Response.StatusCode(), fresh.Response.Body())
	}
	stale := request("PUT", "/v1/projects/"+payload.Project.ID+"/files", string(nextBody))
	if stale.Response.StatusCode() != 409 || !bytes.Contains(stale.Response.Body(), []byte("another tab")) {
		t.Fatalf("stale save was not rejected: %d %s", stale.Response.StatusCode(), stale.Response.Body())
	}
	historical := request("GET", "/v1/projects/"+payload.Project.ID+"/files?version="+firstSave.VersionID, "")
	if historical.Response.StatusCode() != 200 || !bytes.Contains(historical.Response.Body(), []byte(`\u003ch1\u003eok\u003c/h1\u003e`)) {
		t.Fatalf("immutable version did not load: %d %s", historical.Response.StatusCode(), historical.Response.Body())
	}
	versions := request("GET", "/v1/projects/"+payload.Project.ID+"/versions", "")
	if versions.Response.StatusCode() != 200 || !bytes.Contains(versions.Response.Body(), []byte(firstSave.VersionID)) {
		t.Fatalf("version history did not list saved version: %d %s", versions.Response.StatusCode(), versions.Response.Body())
	}
	share := request("POST", "/v1/projects/"+payload.Project.ID+"/share", `{}`)
	var sharePayload struct {
		Token string `json:"share_token"`
	}
	_ = json.Unmarshal(share.Response.Body(), &sharePayload)
	if sharePayload.Token == "" {
		t.Fatalf("share link was not created: %d %s", share.Response.StatusCode(), share.Response.Body())
	}
	sharedVersion := request("GET", "/v1/shared/projects/"+sharePayload.Token+"/files?version="+firstSave.VersionID, "")
	if sharedVersion.Response.StatusCode() != 200 || !bytes.Contains(sharedVersion.Response.Body(), []byte(`\u003ch1\u003eok\u003c/h1\u003e`)) {
		t.Fatalf("shared immutable version did not load: %d %s", sharedVersion.Response.StatusCode(), sharedVersion.Response.Body())
	}
	sharedMain := request("GET", "/v1/shared/projects/"+sharePayload.Token+"/files", "")
	if sharedMain.Response.StatusCode() != 200 || !bytes.Contains(sharedMain.Response.Body(), []byte(`\u003ch1\u003enewer\u003c/h1\u003e`)) {
		t.Fatalf("shared mainline did not track latest files: %d %s", sharedMain.Response.StatusCode(), sharedMain.Response.Body())
	}
	download := request("GET", "/v1/projects/"+payload.Project.ID+"/download", "")
	if download.Response.StatusCode() != 200 || string(download.Response.Header.ContentType()) != "application/zip" {
		t.Fatalf("download failed: %d %s", download.Response.StatusCode(), download.Response.Body())
	}
	zr, err := zip.NewReader(bytes.NewReader(download.Response.Body()), int64(len(download.Response.Body())))
	if err != nil {
		t.Fatalf("download is not a ZIP: %v", err)
	}
	foundIndex := false
	for _, item := range zr.File {
		if item.Name == "index.html" {
			foundIndex = true
		}
	}
	if !foundIndex || !strings.Contains(string(download.Response.Header.Peek("Content-Disposition")), "converter.zip") {
		t.Fatalf("download did not preserve project: files=%v disposition=%s", foundIndex, download.Response.Header.Peek("Content-Disposition"))
	}
	other, _ := s.auth.createUser("other@example.com", "correct horse battery staple")
	otherToken, _ := s.auth.createSession(other.ID, "other")
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/v1/projects/" + payload.Project.ID + "/files")
	ctx.Request.Header.SetCookie(sessionCookie, otherToken)
	s.route(ctx)
	if ctx.Response.StatusCode() != 404 {
		t.Fatalf("foreign workspace leaked: %d %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}

func TestShowcaseOnlyReturnsApprovedPublicProjects(t *testing.T) {
	s := authTestServer(t)
	u, _ := s.auth.createUser("maker@example.com", "correct horse battery staple")
	now := time.Now().UTC()
	_, _ = s.db.Exec(`UPDATE users SET handle='maker' WHERE id=$1`, u.ID)
	_, _ = s.db.Exec(`INSERT INTO projects(id,user_id,name,slug,runtime,description,category,visibility,showcase_status,tags,created_at,updated_at,active_deployment_id)
		VALUES('approved',$1,'Good tool','good-tool','app','Useful','tool','public','approved','["data"]',$2,$2,'ready'),
		('pending',$1,'Hidden tool','hidden-tool','app','Not reviewed','tool','public','pending','[]',$2,$2,'waiting')`, u.ID, now)
	_, _ = s.db.Exec(`INSERT INTO deployments(id,project_id,user_id,status,public_url,created_at)
		VALUES('ready','approved',$1,'ready','https://example/p/approved/',$2),
		('waiting','pending',$1,'ready','https://example/p/pending/',$2)`, u.ID, now)
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/v1/showcase?category=tool")
	s.route(ctx)
	if ctx.Response.StatusCode() != 200 || !bytes.Contains(ctx.Response.Body(), []byte("Good tool")) || bytes.Contains(ctx.Response.Body(), []byte("Hidden tool")) {
		t.Fatalf("showcase boundary failed: %d %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}
