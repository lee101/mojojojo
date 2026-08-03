package main

// Managed vector search keeps identity, source documents, billing, and
// placement in the Go control plane. Workers only receive an opaque collection
// id and portable int8 snapshots; they never receive a user id or API key.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/valyala/fasthttp"
)

const (
	vectorDimension    = 512
	vectorBatchLimit   = 256
	vectorMetadataMax  = 8 << 10
	vectorQueryMax     = 32 << 10
	vectorSearchMaxK   = 100
	vectorHydrateBatch = 128
)

type vectorClient struct {
	base, token string
	http        *http.Client
	mu          sync.Mutex
	loaded      map[string]int64
}

type vectorDocument struct {
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type vectorSearchResult struct {
	OK      bool `json:"ok"`
	Matches []struct {
		ID       string         `json:"id"`
		Score    float64        `json:"score"`
		Metadata map[string]any `json:"metadata"`
	} `json:"matches"`
	VectorCount int     `json:"vector_count"`
	Backend     string  `json:"backend"`
	LatencyMs   float64 `json:"latency_ms"`
}

func newVectorClient(base, token string, client *http.Client) *vectorClient {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	return &vectorClient{base: strings.TrimRight(base, "/"), token: token,
		http: client, loaded: map[string]int64{}}
}

func (c *vectorClient) request(method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(responseBody, &failure)
		if failure.Error == "" {
			failure.Error = resp.Status
		}
		return errors.New(failure.Error)
	}
	if output != nil && len(responseBody) > 0 {
		return json.Unmarshal(responseBody, output)
	}
	return nil
}

func (c *vectorClient) health() (map[string]any, error) {
	var out map[string]any
	return out, c.request(http.MethodGet, "/health", nil, &out)
}

func (c *vectorClient) upsert(projectID string, documents []vectorDocument) error {
	return c.request(http.MethodPost, "/collections/"+projectID+"/upsert",
		map[string]any{"documents": documents}, nil)
}

func (c *vectorClient) search(projectID, query string, limit int) (vectorSearchResult, error) {
	var out vectorSearchResult
	err := c.request(http.MethodPost, "/collections/"+projectID+"/search",
		map[string]any{"query": query, "limit": limit}, &out)
	return out, err
}

func (c *vectorClient) delete(projectID string) error {
	err := c.request(http.MethodDelete, "/collections/"+projectID+"/index", nil, nil)
	c.mu.Lock()
	delete(c.loaded, projectID)
	c.mu.Unlock()
	return err
}

func (c *vectorClient) revision(projectID string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	revision, ok := c.loaded[projectID]
	return revision, ok
}

func (c *vectorClient) setRevision(projectID string, revision int64) {
	c.mu.Lock()
	c.loaded[projectID] = revision
	c.mu.Unlock()
}

type vectorProject struct {
	ID, UserID, Name, Slug, Tier, Status string
	Dimension                            int
	VectorCount, Revision                int64
	CreatedAt, UpdatedAt                 time.Time
}

func (s *Server) vectorProject(userID, projectID string) (*vectorProject, error) {
	var p vectorProject
	err := s.db.QueryRow(`SELECT id, user_id, name, slug, tier, status, dimension,
		vector_count, revision, created_at, updated_at FROM vector_projects
		WHERE id = $1 AND user_id = $2`, projectID, userID).Scan(
		&p.ID, &p.UserID, &p.Name, &p.Slug, &p.Tier, &p.Status, &p.Dimension,
		&p.VectorCount, &p.Revision, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func vectorProjectJSON(p *vectorProject) map[string]any {
	return map[string]any{"id": p.ID, "name": p.Name, "slug": p.Slug, "tier": p.Tier,
		"status": p.Status, "dimension": p.Dimension, "vector_count": p.VectorCount,
		"revision": p.Revision, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt}
}

func vectorPricing(cfg Config) map[string]any {
	return map[string]any{
		"model": "folded-static-int8-512", "dimension": vectorDimension,
		"cpu_exact_usd_per_1000_queries":  round3(cfg.VectorCPUQueryCredits),
		"gpu_ann_usd_per_1000_queries":    round3(cfg.VectorGPUQueryCredits),
		"usd_per_1000_documents_upserted": round3(cfg.VectorUpsertCredits),
		"max_documents_per_project":       cfg.VectorMaxDocuments,
		"note":                            "Performance search is selected automatically for eligible projects",
	}
}

func (s *Server) vectorUser(ctx *fasthttp.RequestCtx) *User {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		if bearerToken(ctx) != "" {
			sendError(ctx, 401, "invalid or revoked API key")
		} else {
			sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		}
	}
	return u
}

func (s *Server) handleVectorProjects(ctx *fasthttp.RequestCtx) {
	u := s.vectorUser(ctx)
	if u == nil {
		return
	}
	if ctx.IsPost() {
		var req struct{ Name, Slug, Tier string }
		if json.Unmarshal(ctx.PostBody(), &req) != nil {
			sendError(ctx, 400, "invalid JSON")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" || utf8.RuneCountInString(name) > 80 {
			sendError(ctx, 400, "project name must be between 1 and 80 characters")
			return
		}
		slug := projectSlug(req.Slug)
		if slug == "" {
			slug = projectSlug(name)
		}
		tier := strings.ToLower(strings.TrimSpace(req.Tier))
		if tier == "" {
			tier = "exact"
		}
		if tier != "exact" && tier != "performance" {
			sendError(ctx, 400, "tier must be exact or performance")
			return
		}
		id, now := "vec_"+randomToken(12), time.Now().UTC()
		_, err := s.db.Exec(`INSERT INTO vector_projects
			(id, user_id, name, slug, tier, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)`, id, u.ID, name, slug, tier, now)
		if err != nil {
			sendError(ctx, 409, "a vector project with that slug already exists")
			return
		}
		ctx.SetStatusCode(201)
		sendJSON(ctx, map[string]any{"ok": true, "project": vectorProjectJSON(&vectorProject{
			ID: id, UserID: u.ID, Name: name, Slug: slug, Tier: tier, Status: "ready",
			Dimension: vectorDimension, CreatedAt: now, UpdatedAt: now})})
		return
	}
	rows, err := s.db.Query(`SELECT id, user_id, name, slug, tier, status, dimension,
		vector_count, revision, created_at, updated_at FROM vector_projects
		WHERE user_id = $1 ORDER BY updated_at DESC`, u.ID)
	if err != nil {
		sendError(ctx, 500, "could not list vector projects")
		return
	}
	defer rows.Close()
	projects := make([]map[string]any, 0)
	for rows.Next() {
		var p vectorProject
		if rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Slug, &p.Tier, &p.Status,
			&p.Dimension, &p.VectorCount, &p.Revision, &p.CreatedAt, &p.UpdatedAt) == nil {
			projects = append(projects, vectorProjectJSON(&p))
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "projects": projects, "pricing": vectorPricing(s.cfg)})
}

func (s *Server) handleVectorProjectRoute(ctx *fasthttp.RequestCtx, tail string) {
	u := s.vectorUser(ctx)
	if u == nil {
		return
	}
	parts := strings.Split(strings.Trim(tail, "/"), "/")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		sendError(ctx, 404, "vector project not found")
		return
	}
	p, err := s.vectorProject(u.ID, parts[0])
	if err != nil {
		sendError(ctx, 404, "vector project not found")
		return
	}
	operation := ""
	if len(parts) == 2 {
		operation = parts[1]
	}
	switch {
	case operation == "" && ctx.IsDelete():
		s.handleVectorDelete(ctx, u, p)
	case operation == "upsert" && ctx.IsPost():
		s.handleVectorUpsert(ctx, u, p)
	case operation == "search" && ctx.IsPost():
		s.handleVectorSearch(ctx, u, p)
	case operation == "usage" && ctx.IsGet():
		s.handleVectorUsage(ctx, u, p)
	default:
		sendError(ctx, 404, "vector operation not found")
	}
}

func validateVectorDocuments(documents []vectorDocument, maxBytes int) error {
	if len(documents) == 0 || len(documents) > vectorBatchLimit {
		return fmt.Errorf("send between 1 and %d documents", vectorBatchLimit)
	}
	seen := map[string]bool{}
	for _, document := range documents {
		if strings.TrimSpace(document.ID) == "" || len(document.ID) > 128 || strings.ContainsAny(document.ID, "\x00\r\n") {
			return errors.New("each document id must be 1-128 characters without control characters")
		}
		if seen[document.ID] {
			return fmt.Errorf("duplicate document id %q", document.ID)
		}
		seen[document.ID] = true
		if strings.TrimSpace(document.Text) == "" || len(document.Text) > maxBytes {
			return fmt.Errorf("document %q text must be 1-%d bytes", document.ID, maxBytes)
		}
		metadata, err := json.Marshal(document.Metadata)
		if err != nil || len(metadata) > vectorMetadataMax {
			return fmt.Errorf("document %q metadata must be a JSON object no larger than %d bytes", document.ID, vectorMetadataMax)
		}
	}
	return nil
}

func (s *Server) handleVectorUpsert(ctx *fasthttp.RequestCtx, u *User, p *vectorProject) {
	if u.Credits() < 1 {
		s.bill.grantWelcome(u)
		u = s.auth.userByID(u.ID)
	}
	if u == nil || u.Credits() < 1 {
		sendError(ctx, 402, "out of credits — top up at "+s.cfg.AccountURL)
		return
	}
	var req struct {
		Documents []vectorDocument `json:"documents"`
	}
	if json.Unmarshal(ctx.PostBody(), &req) != nil {
		sendError(ctx, 400, "invalid JSON")
		return
	}
	if err := validateVectorDocuments(req.Documents, s.cfg.VectorMaxDocumentBytes); err != nil {
		sendError(ctx, 400, err.Error())
		return
	}
	count, revision, err := s.storeVectorDocuments(p, req.Documents)
	if err != nil {
		if errors.Is(err, errVectorProjectLimit) {
			sendError(ctx, 413, fmt.Sprintf("project limit is %d documents", s.cfg.VectorMaxDocuments))
		} else {
			sendError(ctx, 500, "could not store vector documents")
		}
		return
	}
	if err = s.syncVectorDocuments(p, req.Documents, revision); err != nil {
		_, _ = s.db.Exec(`UPDATE vector_projects SET status='degraded' WHERE id=$1`, p.ID)
		sendError(ctx, 502, "vector worker unavailable; documents are durable and will resync automatically")
		return
	}
	_, _ = s.db.Exec(`UPDATE vector_projects SET status='ready' WHERE id=$1`, p.ID)
	charge, chargeErr := s.bill.chargeVector(u, p.ID, "upsert", int64(len(req.Documents)),
		"folded-static-int8-512", 0, float64(len(req.Documents))*s.cfg.VectorUpsertCredits)
	if chargeErr != nil {
		sendError(ctx, 500, "documents indexed but usage metering failed")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "project_id": p.ID, "vector_count": count,
		"revision": revision, "upserted": len(req.Documents), "charge": charge})
}

var errVectorProjectLimit = errors.New("vector project document limit exceeded")

// storeVectorDocuments keeps Postgres authoritative. Both the vector API and
// managed data collections use this path, so a worker can always be rebuilt
// from the same tenant-scoped source rows.
func (s *Server) storeVectorDocuments(p *vectorProject, documents []vectorDocument) (int64, int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	for _, document := range documents {
		metadata, _ := json.Marshal(document.Metadata)
		if _, err = tx.Exec(`INSERT INTO vector_documents
			(project_id, external_id, content, metadata, updated_at) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT(project_id, external_id) DO UPDATE SET content=EXCLUDED.content,
			metadata=EXCLUDED.metadata, updated_at=EXCLUDED.updated_at`,
			p.ID, document.ID, document.Text, string(metadata), now); err != nil {
			return 0, 0, err
		}
	}
	var count int64
	if err = tx.QueryRow(`SELECT COUNT(*) FROM vector_documents WHERE project_id=$1`, p.ID).Scan(&count); err != nil {
		return 0, 0, err
	}
	if count > int64(s.cfg.VectorMaxDocuments) {
		return 0, 0, errVectorProjectLimit
	}
	var revision int64
	if err = tx.QueryRow(`UPDATE vector_projects SET vector_count=$1, revision=revision+1,
		status='syncing', updated_at=$2 WHERE id=$3 RETURNING revision`, count, now, p.ID).Scan(&revision); err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return count, revision, nil
}

func (s *Server) syncVectorDocuments(p *vectorProject, documents []vectorDocument, revision int64) error {
	// If this process already knows the worker has the previous revision, only
	// send the changed batch. Otherwise rebuild from Postgres, the source of
	// truth. This is what makes a newly rented node safe to route to.
	release := s.acquireVector(scaleKindForProject(p))
	defer release()
	loaded, ok := s.vector.revision(p.ID)
	if ok && loaded == revision-1 {
		err := s.vector.upsert(p.ID, documents)
		if err == nil {
			s.vector.setRevision(p.ID, revision)
		}
		return err
	} else {
		return s.hydrateVectorProject(s.vector, p.ID, revision)
	}
}

func (s *Server) hydrateVectorProject(client *vectorClient, projectID string, revision int64) error {
	// Hydration is replacement, not merge. A project can lose documents (for
	// example when a linked managed-data record is deleted), so merely
	// upserting the surviving source rows would leave stale matches behind.
	if err := client.delete(projectID); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT external_id, content, metadata FROM vector_documents
		WHERE project_id=$1 ORDER BY external_id`, projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	batch := make([]vectorDocument, 0, vectorHydrateBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := client.upsert(projectID, batch)
		batch = batch[:0]
		return err
	}
	for rows.Next() {
		var document vectorDocument
		var raw []byte
		if err := rows.Scan(&document.ID, &document.Text, &raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &document.Metadata); err != nil {
			return err
		}
		batch = append(batch, document)
		if len(batch) == vectorHydrateBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	client.setRevision(projectID, revision)
	return nil
}

func (s *Server) handleVectorSearch(ctx *fasthttp.RequestCtx, u *User, p *vectorProject) {
	if u.Credits() < 1 {
		s.bill.grantWelcome(u)
		u = s.auth.userByID(u.ID)
	}
	if u == nil || u.Credits() < 1 {
		sendError(ctx, 402, "out of credits — top up at "+s.cfg.AccountURL)
		return
	}
	var req struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if json.Unmarshal(ctx.PostBody(), &req) != nil {
		sendError(ctx, 400, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Query) == "" || len(req.Query) > vectorQueryMax {
		sendError(ctx, 400, fmt.Sprintf("query must be 1-%d bytes", vectorQueryMax))
		return
	}
	if req.Limit == 0 {
		req.Limit = 10
	}
	if req.Limit < 1 || req.Limit > vectorSearchMaxK {
		sendError(ctx, 400, fmt.Sprintf("limit must be between 1 and %d", vectorSearchMaxK))
		return
	}
	client, ranOn, release := s.pickVectorBackend(p, u.ID)
	defer release()
	if revision, ok := client.revision(p.ID); !ok || revision != p.Revision {
		if err := s.hydrateVectorProject(client, p.ID, p.Revision); err != nil {
			sendError(ctx, 502, "vector worker unavailable")
			return
		}
	}
	result, err := client.search(p.ID, req.Query, req.Limit)
	if err != nil {
		sendError(ctx, 502, "vector worker unavailable")
		return
	}
	cost := s.cfg.VectorCPUQueryCredits
	if strings.Contains(result.Backend, "gpu") {
		cost = s.cfg.VectorGPUQueryCredits
	}
	charge, err := s.bill.chargeVector(u, p.ID, "search", 1, result.Backend, result.LatencyMs, cost)
	if err != nil {
		sendError(ctx, 500, "search completed but usage metering failed")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "project_id": p.ID, "matches": result.Matches,
		"vector_count": result.VectorCount, "backend": result.Backend,
		"latency_ms": result.LatencyMs, "ran_on": ranOn, "charge": charge})
}

func (s *Server) handleVectorDelete(ctx *fasthttp.RequestCtx, u *User, p *vectorProject) {
	if err := s.vector.delete(p.ID); err != nil {
		sendError(ctx, 502, "vector worker unavailable; project was not deleted")
		return
	}
	// Best-effort erasure of overflow replicas. The local durable source is
	// authoritative, so an unreachable rented node must not make account data
	// undeletable; its whole disk is destroyed when the scaler returns it.
	if s.vectorLoad != nil {
		s.vectorLoad.mu.Lock()
		clients := make([]*vectorClient, 0, len(s.vectorClients))
		for _, client := range s.vectorClients {
			clients = append(clients, client)
		}
		s.vectorLoad.mu.Unlock()
		for _, client := range clients {
			_ = client.delete(p.ID)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM vector_projects WHERE id=$1 AND user_id=$2`, p.ID, u.ID); err != nil {
		sendError(ctx, 500, "could not delete vector project")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true})
}

func (s *Server) handleVectorUsage(ctx *fasthttp.RequestCtx, u *User, p *vectorProject) {
	rows, err := s.db.Query(`SELECT created_at, operation, units, backend, latency_ms,
		accrued_credits, charged_credits FROM vector_usage
		WHERE user_id=$1 AND project_id=$2 ORDER BY created_at DESC LIMIT 100`, u.ID, p.ID)
	if err != nil {
		sendError(ctx, 500, "could not load vector usage")
		return
	}
	defer rows.Close()
	usage := make([]map[string]any, 0)
	for rows.Next() {
		var at time.Time
		var operation, backend string
		var units, charged int64
		var latency, accrued float64
		if rows.Scan(&at, &operation, &units, &backend, &latency, &accrued, &charged) == nil {
			usage = append(usage, map[string]any{"at": at, "operation": operation, "units": units,
				"backend": backend, "latency_ms": round3(latency), "accrued_credits": round3(accrued),
				"charged_credits": charged})
		}
	}
	var owed, total float64
	_ = s.db.QueryRow(`SELECT owed_credits,total_credits FROM vector_accrual WHERE user_id=$1`, u.ID).Scan(&owed, &total)
	sendJSON(ctx, map[string]any{"ok": true, "project": vectorProjectJSON(p), "usage": usage,
		"owed_credits": round3(owed), "total_credits": round3(total), "pricing": vectorPricing(s.cfg)})
}

type vectorCharge struct {
	AccruedCredits float64 `json:"accrued_credits"`
	ChargedCredits int     `json:"charged_credits"`
	OwedCredits    float64 `json:"owed_credits"`
	Balance        int     `json:"balance"`
}

func (b *billing) chargeVector(u *User, projectID, operation string, units int64,
	backend string, latencyMs, rawCredits float64) (vectorCharge, error) {
	lock := b.lockFor(u.ID)
	lock.Lock()
	defer lock.Unlock()
	if rawCredits < 0 {
		rawCredits = 0
	}
	var owed float64
	err := b.db.QueryRow(`SELECT owed_credits FROM vector_accrual WHERE user_id=$1`, u.ID).Scan(&owed)
	if err != nil && err != sql.ErrNoRows {
		return vectorCharge{}, err
	}
	owed += rawCredits
	credits := int(math.Floor(owed))
	if credits > 0 {
		owed -= float64(credits)
	}
	balance := u.Credits()
	if credits > 0 {
		spent, nextBalance, err := b.spendReason(u.ID, credits,
			fmt.Sprintf("vector:%s:%d", operation, units))
		if err != nil {
			return vectorCharge{}, err
		}
		if spent < credits {
			owed += float64(credits - spent)
		}
		credits, balance = spent, nextBalance
	}
	now := time.Now().UTC()
	if _, err := b.db.Exec(`INSERT INTO vector_accrual
		(user_id,owed_credits,total_credits,updated_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT(user_id) DO UPDATE SET owed_credits=EXCLUDED.owed_credits,
		total_credits=vector_accrual.total_credits+EXCLUDED.total_credits,
		updated_at=EXCLUDED.updated_at`, u.ID, owed, rawCredits, now); err != nil {
		return vectorCharge{}, err
	}
	if _, err := b.db.Exec(`INSERT INTO vector_usage
		(id,user_id,project_id,operation,units,backend,latency_ms,accrued_credits,charged_credits,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, randomToken(12), u.ID, projectID,
		operation, units, backend, latencyMs, rawCredits, credits, now); err != nil {
		return vectorCharge{}, err
	}
	return vectorCharge{AccruedCredits: round3(rawCredits), ChargedCredits: credits,
		OwedCredits: round3(owed), Balance: balance}, nil
}
