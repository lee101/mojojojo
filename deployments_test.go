package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

func zipFixture(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestReadBundleAcceptsManifestAndAssets(t *testing.T) {
	body := zipFixture(t, map[string]string{
		"mojojojo.json":     `{"runtime":"static","entrypoint":"public/index.html"}`,
		"public/index.html": "<!doctype html><title>ok</title>",
		"public/app.js":     "console.log('ok')",
	})
	files, manifest, err := readBundle(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || manifest.Entrypoint != "public/index.html" || manifest.Runtime != "static" {
		t.Fatalf("unexpected bundle: %d %#v", len(files), manifest)
	}
}

func TestReadBundleRejectsTraversalAndMissingEntrypoint(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"traversal": {"../index.html": "bad"},
		"missing":   {"mojojojo.json": `{"entrypoint":"index.html"}`, "other.txt": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := readBundle(zipFixture(t, files)); err == nil {
				t.Fatal("unsafe bundle was accepted")
			}
		})
	}
}

func TestReadBundleEnforcesFileCountBeforeExpansion(t *testing.T) {
	files := make(map[string]string, maxDeploymentFiles+1)
	for i := 0; i <= maxDeploymentFiles; i++ {
		files[fmt.Sprintf("f%03d.txt", i)] = "x"
	}
	if _, _, err := readBundle(zipFixture(t, files)); err == nil {
		t.Fatal("oversized file count was accepted")
	}
}

func TestSitemapIncludesCloudExamplesAndResearch(t *testing.T) {
	s := &Server{cfg: Config{SiteBaseURL: "https://mojojojo.cc"}}
	ctx := &fasthttp.RequestCtx{}
	s.handleSitemap(ctx)
	body := string(ctx.Response.Body())
	for _, path := range []string{"/cloud</loc>", "/examples</loc>", "/apps</loc>", "/tools</loc>", "/studio</loc>", "/agent</loc>", "/research/" + posts[0].Slug} {
		if !strings.Contains(body, path) {
			t.Errorf("sitemap missing %s", path)
		}
	}
}

func TestProjectAPIUsesTheLocalAccountBoundary(t *testing.T) {
	s := authTestServer(t)
	user, err := s.auth.createUser("cloud@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.auth.createSession(user.ID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
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
	created := request("POST", "/v1/projects", `{"name":"Fractal Explorer","runtime":"static"}`)
	if created.Response.StatusCode() != 201 {
		t.Fatalf("create project: %d %s", created.Response.StatusCode(), created.Response.Body())
	}
	var result struct {
		Project struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"project"`
	}
	if err := json.Unmarshal(created.Response.Body(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Project.ID == "" || result.Project.Slug != "fractal-explorer" {
		t.Fatalf("unexpected project: %#v", result.Project)
	}
	listed := request("GET", "/v1/projects", "")
	if listed.Response.StatusCode() != 200 || !bytes.Contains(listed.Response.Body(), []byte(result.Project.ID)) {
		t.Fatalf("list projects: %d %s", listed.Response.StatusCode(), listed.Response.Body())
	}
	storage := request("GET", "/v1/storage", "")
	if storage.Response.StatusCode() != 200 || !bytes.Contains(storage.Response.Body(), []byte(`"used_bytes":0`)) {
		t.Fatalf("storage status: %d %s", storage.Response.StatusCode(), storage.Response.Body())
	}
}
