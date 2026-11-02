package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every page, rendered both ways. Two things are checked and neither is
// cosmetic: that the template executes at all (a missing key in one handler's
// data map is a 500 on a page nobody clicked during review), and that a
// stranger is always one click from an account and a customer always one click
// from paying. "Sign up" existing only on the pricing page is how a service
// ends up with visitors who cannot work out how to give it money.
func TestEveryPageOffersSignupAndTopUp(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "templates")); err != nil {
		t.Skipf("no templates directory: %v", err)
	}
	cfg := loadConfig()
	tmpl, err := loadTemplates(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, tmpl: tmpl}

	pages := []string{"home.html", "editor.html", "docs.html", "agent.html", "pricing.html",
		"keys.html", "usage.html", "credits.html", "cloud.html", "data.html", "examples.html",
		"vector.html", "research.html", "404.html", "auth.html", "email_preferences.html", "email_unsubscribe.html"}
	// Every declared post must have a template, and vice versa: a post in the
	// index with no file is a 404 from a link we published ourselves.
	for _, post := range posts {
		pages = append(pages, "research/"+post.Slug+".html")
	}
	user := &User{ID: "u1", Email: "a@b.c", Handle: "tester", FreeCredit: 1200}
	now := time.Now().UTC()

	for _, page := range pages {
		for _, signedIn := range []bool{false, true} {
			data := map[string]any{
				"Meta":    s.meta("t", ""),
				"Example": exampleProgram,
				"OwedMs":  1234.5, "TotalMs": 98765.4,
				"Granted": true, "Spent": 12, "Earned": 1000,
				"Runs": []usageRow{{At: now, Ms: 41.8, Credits: 0, Source: "editor"}},
				"Keys": []APIKey{{ID: "k1", Name: "k", Prefix: "mj_live_abc",
					Created: now, LastUsed: &now}},
				"Ledger": []LedgerEntry{{At: now, Delta: -2, Reason: "exec:cpu:2500ms",
					App: "mojojojo", Source: "spend", Balance: 1198}},
				"Posts": posts,
				"Post":  &posts[0],
				"Mode":  "signup", "Next": "/keys", "CSRF": "test-csrf",
				"Token": "signed-token",
			}
			if strings.HasPrefix(page, "research/") {
				slug := strings.TrimSuffix(strings.TrimPrefix(page, "research/"), ".html")
				post := postBySlug(slug)
				if post == nil {
					t.Fatalf("%s has no entry in the research index", page)
				}
				data["Post"] = post
			}
			if signedIn {
				data["User"] = user
			}
			var buf bytes.Buffer
			if err := tmpl.t.ExecuteTemplate(&buf, page, s.pageData(data)); err != nil {
				t.Fatalf("%s (signedIn=%v): %v", page, signedIn, err)
			}
			html := buf.String()
			if strings.Contains(html, "<no value>") {
				t.Errorf("%s (signedIn=%v): rendered a <no value>", page, signedIn)
			}
			if !strings.Contains(html, cfg.TopUpURL) {
				t.Errorf("%s (signedIn=%v): no way to buy credits", page, signedIn)
			}
			if !strings.Contains(html, `href="/credits"`) {
				t.Errorf("%s (signedIn=%v): no link to credit management", page, signedIn)
			}
			if !signedIn && !strings.Contains(html, cfg.SignupURL) {
				t.Errorf("%s: signed out with no way to sign up", page)
			}
		}
	}
}

func TestTopUpURLCarriesCheckoutIntent(t *testing.T) {
	cfg := loadConfig()
	if !strings.HasSuffix(cfg.TopUpURL, "/credits?buy=credits") {
		t.Fatalf("top-up URL %q does not carry the checkout intent", cfg.TopUpURL)
	}
}

func TestAgentPageLinksThePublicProject(t *testing.T) {
	cfg := loadConfig()
	tmpl, err := loadTemplates(cfg)
	if err != nil {
		t.Skipf("templates unavailable: %v", err)
	}
	s := &Server{cfg: cfg, tmpl: tmpl}
	var buf bytes.Buffer
	data := s.pageData(map[string]any{
		"Meta": s.metaAt("mjj", "agent", "/agent"),
	})
	if err := tmpl.t.ExecuteTemplate(&buf, "agent.html", data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{
		agentSourceURL,
		agentDocsURL,
		agentReleasesURL,
		agentIssuesURL,
		agentLicenseURL,
		`href="#install"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("agent page does not link %q", want)
		}
	}
	if data["Meta"].(Meta).Canonical != cfg.SiteBaseURL+"/agent" {
		t.Errorf("agent canonical = %q", data["Meta"].(Meta).Canonical)
	}
}

// The low-balance banner is the only thing standing between a user and a 402
// they did not see coming, so it appears exactly when it should.
func TestLowBalanceBannerAppearsOnlyWhenLow(t *testing.T) {
	cfg := loadConfig()
	tmpl, err := loadTemplates(cfg)
	if err != nil {
		t.Skipf("templates unavailable: %v", err)
	}
	s := &Server{cfg: cfg, tmpl: tmpl}

	for _, tc := range []struct {
		credits int
		want    bool
	}{{1, true}, {99, true}, {100, false}, {5000, false}} {
		var buf bytes.Buffer
		data := s.pageData(map[string]any{
			"Meta": s.meta("t", ""),
			"User": &User{ID: "u1", Email: "a@b.c", PaidCredit: tc.credits},
		})
		if err := tmpl.t.ExecuteTemplate(&buf, "404.html", data); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(buf.String(), "credits left</b>"); got != tc.want {
			t.Errorf("%d credits: banner=%v, want %v", tc.credits, got, tc.want)
		}
	}
	var buf bytes.Buffer
	data := s.pageData(map[string]any{
		"Meta": s.meta("t", ""),
		"User": &User{ID: "u1", Email: "a@b.c", FreeCredit: 5000},
	})
	if err := tmpl.t.ExecuteTemplate(&buf, "404.html", data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No paid credits") {
		t.Error("an account holding only bonus credits is not told to top up")
	}
}

// The editor opts out of the reading-width shell. Keep the opt-in explicit so
// another page cannot accidentally become full bleed when this layout evolves.
func TestEditorUsesFullBleedProductShell(t *testing.T) {
	cfg := loadConfig()
	tmpl, err := loadTemplates(cfg)
	if err != nil {
		t.Skipf("templates unavailable: %v", err)
	}
	s := &Server{cfg: cfg, tmpl: tmpl}
	var buf bytes.Buffer
	data := s.pageData(map[string]any{
		"Meta":      s.meta("editor", ""),
		"Example":   editorExample,
		"FullBleed": true,
	})
	if err := tmpl.t.ExecuteTemplate(&buf, "editor.html", data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{
		`<body class="full-bleed">`,
		`class="workspace"`,
		`role="separator"`,
		`id="line-numbers"`,
		`/static/editor.css`,
		`/static/editor.js`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("editor shell is missing %q", want)
		}
	}
}

// A post file with no index entry is unreachable, and an index entry with no
// file is a 404 from a link we published. Both directions, so neither can drift.
func TestResearchIndexAndFilesAgree(t *testing.T) {
	files, err := filepath.Glob("templates/research/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no research templates")
	}
	onDisk := map[string]bool{}
	for _, path := range files {
		onDisk[strings.TrimSuffix(filepath.Base(path), ".html")] = true
	}
	for _, post := range posts {
		if !onDisk[post.Slug] {
			t.Errorf("post %q is in the index with no templates/research/%s.html",
				post.Slug, post.Slug)
		}
		delete(onDisk, post.Slug)
		if post.Title == "" || post.Blurb == "" || post.Date == "" {
			t.Errorf("post %q is missing title, blurb or date", post.Slug)
		}
		if len(post.Tags) == 0 {
			t.Errorf("post %q has no tags, so it groups under nothing", post.Slug)
		}
	}
	for slug := range onDisk {
		t.Errorf("templates/research/%s.html is not in the index — unreachable", slug)
	}
}

// Measurement posts must cite something reproducible. Product/open-source
// notes have a different contract: they must use the non-benchmark footer and
// point to an actual product page, document, or repository from the article.
func TestResearchPostsCiteSomethingRunnable(t *testing.T) {
	cfg := loadConfig()
	tmpl, err := loadTemplates(cfg)
	if err != nil {
		t.Skipf("templates unavailable: %v", err)
	}
	s := &Server{cfg: cfg, tmpl: tmpl}
	for i := range posts {
		source, err := os.ReadFile(filepath.Join("templates", "research", posts[i].Slug+".html"))
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		data := s.pageData(map[string]any{
			"Meta": s.meta("t", ""), "Post": &posts[i], "Posts": posts,
		})
		name := "research/" + posts[i].Slug + ".html"
		if err := tmpl.t.ExecuteTemplate(&buf, name, data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		html := buf.String()
		technical := false
		for _, tag := range posts[i].Tags {
			technical = technical || tag == "containment" || tag == "speed"
		}
		if technical {
			if !strings.Contains(html, "data-lang=") {
				t.Errorf("%s has no highlighted code blocks", name)
			}
			if !strings.Contains(html, "bench/") && !strings.Contains(html, "tests/") &&
				!strings.Contains(html, "runner/") {
				t.Errorf("%s points at nothing a reader could run or read", name)
			}
			continue
		}
		if !bytes.Contains(source, []byte(`{{template "productpostfoot" .}}`)) {
			t.Errorf("%s uses the benchmark footer for a product note", name)
		}
		if !bytes.Contains(source, []byte(`href="`)) {
			t.Errorf("%s has no article-level source, document, or product link", name)
		}
	}
}
