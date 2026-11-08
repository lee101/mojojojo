package main

import (
	"strings"
	"testing"
)

func testToolEntries() []toolEntry {
	return []toolEntry{
		{
			ID:          "plugin:github-app",
			Name:        "GitHub App",
			Kind:        "plugin",
			Description: "Manage GitHub repositories, issues and pull requests from the agent.",
			Tags:        []string{"github", "git", "reviews", "plugin"},
			Install:     "hermes plugins install github-app",
		},
		{
			ID:          "skill:gitlab",
			Name:        "gitlab",
			Kind:        "skill",
			Description: "GitLab merge requests and pipelines.",
			Tags:        []string{"gitlab", "git", "ci", "skill"},
			Install:     "hermes skills install gitlab",
		},
		{
			ID:          "mcp:postgres",
			Name:        "postgres",
			Kind:        "mcp",
			Description: "Query a PostgreSQL database, inspect schemas and run SQL.",
			Tags:        []string{"database", "sql", "postgres", "mcp"},
			Install:     "hermes mcp install postgres",
		},
		{
			ID:          "plugin:slack",
			Name:        "Slack",
			Kind:        "plugin",
			Description: "Send and read Slack messages in channels.",
			Tags:        []string{"chat", "messaging", "plugin"},
			Install:     "hermes plugins install slack",
		},
	}
}

func testToolIndex() *toolSearchIndex {
	return newToolSearchIndex(testToolEntries())
}

func TestToolSearchRanksGithubFirst(t *testing.T) {
	idx := testToolIndex()
	hits := idx.Search("github", "", 10)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].ID != "plugin:github-app" {
		t.Fatalf("first hit = %q (%v), want plugin:github-app", hits[0].ID, hits)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("score = %v", hits[0].Score)
	}
}

func TestToolSearchDescriptionAndCompoundQuery(t *testing.T) {
	idx := testToolIndex()
	hits := idx.Search("pull requests", "", 5)
	if len(hits) == 0 || hits[0].ID != "plugin:github-app" {
		t.Fatalf("hits = %v", hits)
	}
	hits = idx.Search("postgres sql", "", 5)
	if len(hits) == 0 || hits[0].ID != "mcp:postgres" {
		t.Fatalf("hits = %v", hits)
	}
	if hits := idx.Search("xyzzy-nothing", "", 5); len(hits) != 0 {
		t.Fatalf("expected no hits, got %v", hits)
	}
}

func TestToolSearchKindFilter(t *testing.T) {
	idx := testToolIndex()
	hits := idx.Search("git", "mcp", 10)
	if len(hits) != 0 {
		t.Fatalf("kind=mcp returned %v", hits)
	}
	hits = idx.Search("git", "skill", 10)
	if len(hits) == 0 || hits[0].Kind != "skill" {
		t.Fatalf("kind=skill returned %v", hits)
	}
	if _, ok := normalizeToolKind("bogus"); ok {
		t.Fatal("bogus kind accepted")
	}
}

func TestToolSearchLimitIsCapped(t *testing.T) {
	idx := testToolIndex()
	if hits := idx.Search("git", "", 100); len(hits) > toolSearchMaxLimit {
		t.Fatalf("got %d hits, cap is %d", len(hits), toolSearchMaxLimit)
	}
	if hits := idx.Search("git", "", -3); len(hits) > toolSearchDefaultLimit {
		t.Fatalf("negative limit got %d hits", len(hits))
	}
}

func TestToolSearchDescribe(t *testing.T) {
	idx := testToolIndex()
	entry, ok := idx.Describe("mcp:postgres")
	if !ok || entry.Install != "hermes mcp install postgres" {
		t.Fatalf("describe = %+v, %v", entry, ok)
	}
	if entry, ok := idx.Describe("postgres"); !ok || entry.ID != "mcp:postgres" {
		t.Fatalf("bare name describe = %+v, %v", entry, ok)
	}
	if _, ok := idx.Describe("mcp:nope"); ok {
		t.Fatal("unknown id resolved")
	}
}

func TestToolSearchEmbeddedCatalog(t *testing.T) {
	idx := defaultToolSearchIndex()
	if len(idx.entries) < 900 {
		t.Fatalf("embedded catalog has %d entries", len(idx.entries))
	}
	ids := make(map[string]bool, len(idx.entries))
	for _, entry := range idx.entries {
		if ids[entry.ID] {
			t.Fatalf("duplicate id %q", entry.ID)
		}
		ids[entry.ID] = true
	}
	hits := idx.Search("github", "", 10)
	if len(hits) == 0 {
		t.Fatal("github search returned nothing")
	}
	if !strings.Contains(hits[0].ID, "github") {
		t.Fatalf("top github hit = %+v", hits[0])
	}
	mcpHits := idx.Search("postgres", "mcp", 10)
	if len(mcpHits) == 0 || mcpHits[0].Kind != "mcp" {
		t.Fatalf("mcp postgres hits = %+v", mcpHits)
	}
	if desc := mcpHits[0].Description; len(desc) > toolHitDescriptionMax*4 {
		t.Fatalf("hit description too long: %d", len(desc))
	}
}

func TestToolSearchLoadRejectsBadCatalog(t *testing.T) {
	if _, err := loadToolSearchIndex([]byte(`{"tools":[]}`)); err == nil {
		t.Fatal("empty catalog accepted")
	}
	if _, err := loadToolSearchIndex([]byte(`{`)); err == nil {
		t.Fatal("invalid json accepted")
	}
	idx, err := loadToolSearchIndex([]byte(`{"tools":[{"id":"skill:yaml","name":"yaml","kind":"skill"}]}`))
	if err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	if hits := idx.Search("yaml", "", 5); len(hits) != 1 {
		t.Fatalf("hits = %v", hits)
	}
}
