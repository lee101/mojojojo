package main

// Tool search over the hermes-agent catalog (scripts/build_toolcatalog.py).
//
// The catalog is embedded at build time and indexed once on first use. BM25
// is enough for ~950 entries and keeps the index tiny: no tokenizer library,
// no vector store, no startup cost for routes that never search.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/valyala/fasthttp"
)

//go:embed data/toolcatalog.json
var toolCatalogJSON []byte

const (
	toolSearchDefaultLimit = 10
	toolSearchMaxLimit     = 25
	toolSearchMaxQueryLen  = 400
	toolSearchMaxTerms     = 16
	toolHitDescriptionMax  = 240
)

type toolEntry struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Description string         `json:"description"`
	Tags        []string       `json:"tags,omitempty"`
	Install     string         `json:"install,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
}

type toolCatalogFile struct {
	Source string      `json:"source"`
	Count  int         `json:"count"`
	Tools  []toolEntry `json:"tools"`
}

type toolSearchHit struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
}

type toolPosting struct {
	Doc int
	TF  int
}

type toolSearchIndex struct {
	entries  []toolEntry
	postings map[string][]toolPosting
	docLen   []float64
	avgLen   float64
}

var toolSearchStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "via": true, "with": true, "your": true,
}

// toolTokens lowercases alphanumeric runs and also splits camelCase, so
// SuperLocalMemory matches both "superlocalmemory" and "memory".
func toolTokens(s string) []string {
	out := make([]string, 0, 12)
	flush := func(run []rune) {
		if len(run) < 2 {
			return
		}
		out = append(out, strings.ToLower(string(run)))
		start := 0
		for i := 1; i < len(run); i++ {
			if unicode.IsUpper(run[i]) && !unicode.IsUpper(run[i-1]) {
				if i-start >= 2 {
					out = append(out, strings.ToLower(string(run[start:i])))
				}
				start = i
			}
		}
		if len(run)-start >= 2 && start > 0 {
			out = append(out, strings.ToLower(string(run[start:])))
		}
	}
	var run []rune
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			run = append(run, r)
			continue
		}
		flush(run)
		run = run[:0]
	}
	flush(run)
	return out
}

func toolQueryTokens(s string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	for _, token := range toolTokens(s) {
		if toolSearchStopwords[token] || seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

func normalizeToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func newToolSearchIndex(entries []toolEntry) *toolSearchIndex {
	idx := &toolSearchIndex{
		entries:  entries,
		postings: make(map[string][]toolPosting, 4096),
		docLen:   make([]float64, len(entries)),
	}
	for doc, entry := range entries {
		counts := make(map[string]int, 64)
		add := func(text string, weight int) {
			for _, token := range toolTokens(text) {
				counts[token] += weight
			}
		}
		// Name and tags are what a caller means; the description is what they
		// half-remember. Weighting them differently is most of the ranking.
		add(entry.Name, 3)
		add(strings.ReplaceAll(entry.ID, ":", " "), 2)
		add(strings.Join(entry.Tags, " "), 3)
		add(entry.Description, 1)
		total := 0
		for token, tf := range counts {
			idx.postings[token] = append(idx.postings[token], toolPosting{Doc: doc, TF: tf})
			total += tf
		}
		idx.docLen[doc] = float64(total)
	}
	if len(entries) > 0 {
		sum := 0.0
		for _, n := range idx.docLen {
			sum += n
		}
		idx.avgLen = sum / float64(len(entries))
	}
	return idx
}

func normalizeToolKind(kind string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "all":
		return "", true
	case "mcp", "plugin", "skill":
		return strings.ToLower(strings.TrimSpace(kind)), true
	}
	return "", false
}

// Search ranks entries with BM25. A query term that is the whole name gets a
// flat bonus because that is almost always the entry the caller wants.
func (idx *toolSearchIndex) Search(query, kind string, limit int) []toolSearchHit {
	kind, ok := normalizeToolKind(kind)
	if !ok {
		return nil
	}
	if limit <= 0 {
		limit = toolSearchDefaultLimit
	}
	if limit > toolSearchMaxLimit {
		limit = toolSearchMaxLimit
	}
	if utf8.RuneCountInString(query) > toolSearchMaxQueryLen {
		query = string([]rune(query)[:toolSearchMaxQueryLen])
	}
	terms := toolQueryTokens(query)
	if len(terms) > toolSearchMaxTerms {
		terms = terms[:toolSearchMaxTerms]
	}
	if len(terms) == 0 || len(idx.entries) == 0 {
		return nil
	}

	const k1, b = 1.2, 0.75
	scores := make([]float64, len(idx.entries))
	n := float64(len(idx.entries))
	for _, term := range terms {
		list := idx.postings[term]
		if len(list) == 0 {
			continue
		}
		df := float64(len(list))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, posting := range list {
			tf := float64(posting.TF)
			denom := tf + k1*(1-b+b*idx.docLen[posting.Doc]/idx.avgLen)
			if denom <= 0 {
				continue
			}
			scores[posting.Doc] += idf * (tf * (k1 + 1)) / denom
		}
	}

	queryNorm := normalizeToolName(query)
	for doc, entry := range idx.entries {
		if scores[doc] == 0 {
			continue
		}
		nameNorm := normalizeToolName(entry.Name)
		switch {
		case nameNorm == queryNorm:
			scores[doc] += 3
		case len(queryNorm) >= 3 && strings.Contains(nameNorm, queryNorm):
			scores[doc] += 1
		}
	}

	hits := make([]toolSearchHit, 0, limit)
	for doc, entry := range idx.entries {
		if scores[doc] <= 0 || (kind != "" && entry.Kind != kind) {
			continue
		}
		hits = append(hits, toolSearchHit{
			ID:          entry.ID,
			Name:        entry.Name,
			Kind:        entry.Kind,
			Description: toolShortDescription(entry.Description),
			Score:       math.Round(scores[doc]*1000) / 1000,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Name < hits[j].Name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func (idx *toolSearchIndex) Describe(id string) (toolEntry, bool) {
	id = strings.TrimSpace(id)
	for _, entry := range idx.entries {
		if entry.ID == id {
			return entry, true
		}
	}
	// A bare name is unambiguous in practice (ids are kind:name); accept it.
	for _, entry := range idx.entries {
		if strings.TrimPrefix(entry.ID, entry.Kind+":") == id {
			return entry, true
		}
	}
	return toolEntry{}, false
}

func toolShortDescription(text string) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= toolHitDescriptionMax {
		return text
	}
	runes := []rune(text)
	cut := string(runes[:toolHitDescriptionMax])
	if space := strings.LastIndexByte(cut, ' '); space > toolHitDescriptionMax/2 {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " ,;:.") + "…"
}

func loadToolSearchIndex(data []byte) (*toolSearchIndex, error) {
	var catalog toolCatalogFile
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("tool catalog: %w", err)
	}
	if len(catalog.Tools) == 0 {
		return nil, fmt.Errorf("tool catalog: no tools")
	}
	for i := range catalog.Tools {
		if catalog.Tools[i].ID == "" || catalog.Tools[i].Name == "" || catalog.Tools[i].Kind == "" {
			return nil, fmt.Errorf("tool catalog: entry %d is missing id, name or kind", i)
		}
	}
	return newToolSearchIndex(catalog.Tools), nil
}

var (
	toolSearchOnce    sync.Once
	toolSearchDefault *toolSearchIndex
	toolMCPOnce       sync.Once
	toolMCPDefault    *mcpServer
)

func defaultToolSearchIndex() *toolSearchIndex {
	toolSearchOnce.Do(func() {
		idx, err := loadToolSearchIndex(toolCatalogJSON)
		if err != nil {
			log.Printf("tool search disabled: %v", err)
			toolSearchDefault = newToolSearchIndex(nil)
			return
		}
		toolSearchDefault = idx
	})
	return toolSearchDefault
}

// newToolMCP builds the two MCP tools over an index. Tests use this with a
// fixture; production uses defaultToolMCP.
func newToolMCP(idx *toolSearchIndex) *mcpServer {
	s := newMCPServer("mojojojo-toolsearch", "1.0.0")
	_ = s.Register(mcpTool{
		Name:        "tool_search",
		Description: "Search the hermes tool catalog (MCP servers, plugins and skills). Returns ranked ids for tool_describe.",
		InputSchema: mcpObjectSchema(map[string]any{
			"query": mcpStringSchema("What the tool should do, e.g. \"github issues\" or \"postgres\"."),
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"mcp", "plugin", "skill"},
				"description": "Optional kind filter.",
			},
			"limit": mcpNumberSchema("Maximum results, 1-25 (default 10)."),
		}, []string{"query"}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			query, _ := args["query"].(string)
			if strings.TrimSpace(query) == "" {
				return nil, fmt.Errorf("query is required")
			}
			kind, _ := args["kind"].(string)
			if _, ok := normalizeToolKind(kind); !ok {
				return nil, fmt.Errorf("kind must be mcp, plugin or skill")
			}
			hits := idx.Search(query, kind, mcpArgumentLimit(args))
			return map[string]any{"query": query, "count": len(hits), "results": hits}, nil
		},
	})
	_ = s.Register(mcpTool{
		Name:        "tool_describe",
		Description: "Fetch the full catalog entry for a tool id returned by tool_search, including its install command.",
		InputSchema: mcpObjectSchema(map[string]any{
			"id": mcpStringSchema("Catalog id such as \"mcp:airtable\" or \"plugin:superlocalmemory\"."),
		}, []string{"id"}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, _ := args["id"].(string)
			if strings.TrimSpace(id) == "" {
				return nil, fmt.Errorf("id is required")
			}
			entry, ok := idx.Describe(id)
			if !ok {
				return nil, fmt.Errorf("unknown tool id: %s", id)
			}
			return entry, nil
		},
	})
	return s
}

func mcpArgumentLimit(args map[string]any) int {
	switch v := args["limit"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return toolSearchDefaultLimit
}

func defaultToolMCP() *mcpServer {
	toolMCPOnce.Do(func() {
		toolMCPDefault = newToolMCP(defaultToolSearchIndex())
	})
	return toolMCPDefault
}

// handleMCP is POST /mcp: one JSON-RPC message in, one response out.
//
// Search is read-only and costs the box almost nothing, but it is still an
// authenticated endpoint: any account that can present an API key (or a
// dashboard session, through the same userFromRequest check the rest of the
// web tier uses) may call it. Anonymous callers get 401 rather than a free
// catalog scrape.
func (s *Server) handleMCP(ctx *fasthttp.RequestCtx) {
	if s.auth.userFromRequest(ctx) == nil {
		sendError(ctx, 401, "a valid API key is required")
		return
	}
	body := ctx.PostBody()
	ctx.Response.Header.Set("Cache-Control", "no-store")
	if len(strings.TrimSpace(string(body))) == 0 {
		sendError(ctx, 400, "empty JSON-RPC body")
		return
	}
	resp := defaultToolMCP().Handle(ctx, body)
	if resp == nil {
		ctx.SetStatusCode(202)
		return
	}
	ctx.SetContentType("application/json; charset=utf-8")
	ctx.SetBody(resp)
}

// handleToolSearch is GET /api/tools/search?q=&kind=&limit=, the same index
// without JSON-RPC framing.
func (s *Server) handleToolSearch(ctx *fasthttp.RequestCtx) {
	if s.auth.userFromRequest(ctx) == nil {
		sendError(ctx, 401, "a valid API key is required")
		return
	}
	query := strings.TrimSpace(string(ctx.QueryArgs().Peek("q")))
	if query == "" {
		sendError(ctx, 400, "q is required")
		return
	}
	kind := string(ctx.QueryArgs().Peek("kind"))
	if _, ok := normalizeToolKind(kind); !ok {
		sendError(ctx, 400, "kind must be mcp, plugin or skill")
		return
	}
	limit := toolSearchDefaultLimit
	if raw := string(ctx.QueryArgs().Peek("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			sendError(ctx, 400, "limit must be a positive integer")
			return
		}
		limit = n
	}
	hits := defaultToolSearchIndex().Search(query, kind, limit)
	sendJSON(ctx, map[string]any{"ok": true, "query": query, "count": len(hits), "results": hits})
}
