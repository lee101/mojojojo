package main

// Managed Data is a deliberately small document database over the existing
// Postgres control plane. It has no idle process to keep warm, is tenant scoped
// in every query, and can mirror selected record text into a managed vector
// project without making the vector worker the source of truth.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/valyala/fasthttp"
)

const (
	dataRecordMaxBytes   = 64 << 10
	dataDatabaseMaxBytes = 100 << 20
	dataRecordLimit      = 10_000
	dataNamespaceLimit   = 10
	dataListLimit        = 200
)

var (
	errDataRecordLimit  = errors.New("database record limit exceeded")
	errDataStorageLimit = errors.New("database storage limit exceeded")
)

type dataNamespace struct {
	ID, UserID, Name, Slug, VectorProjectID string
	Records                                 int64
	CreatedAt, UpdatedAt                    time.Time
}

func dataNamespaceJSON(namespace *dataNamespace) map[string]any {
	return map[string]any{
		"id": namespace.ID, "name": namespace.Name, "slug": namespace.Slug,
		"vector_project_id": namespace.VectorProjectID, "records": namespace.Records,
		"created_at": namespace.CreatedAt, "updated_at": namespace.UpdatedAt,
	}
}

func (s *Server) dataUser(ctx *fasthttp.RequestCtx) *User {
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

func (s *Server) handleDataNamespaces(ctx *fasthttp.RequestCtx) {
	u := s.dataUser(ctx)
	if u == nil {
		return
	}
	if ctx.IsPost() {
		var req struct {
			Name            string `json:"name"`
			Slug            string `json:"slug"`
			VectorProjectID string `json:"vector_project_id"`
		}
		if json.Unmarshal(ctx.PostBody(), &req) != nil {
			sendError(ctx, 400, "invalid JSON")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" || utf8.RuneCountInString(name) > 80 {
			sendError(ctx, 400, "database name must be between 1 and 80 characters")
			return
		}
		slug := projectSlug(req.Slug)
		if slug == "" {
			slug = projectSlug(name)
		}
		if slug == "" {
			sendError(ctx, 400, "database needs a URL-safe name")
			return
		}
		vectorProjectID := strings.TrimSpace(req.VectorProjectID)
		if vectorProjectID != "" {
			if _, err := s.vectorProject(u.ID, vectorProjectID); err != nil {
				sendError(ctx, 400, "linked vector project not found")
				return
			}
		}
		var databases int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM data_namespaces WHERE user_id=$1`, u.ID).Scan(&databases); err != nil {
			sendError(ctx, 500, "could not check database allowance")
			return
		}
		if tier := dataTierFor(u); databases >= tier.Databases {
			sendError(ctx, 413, fmt.Sprintf("your %s tier allows %d databases", tier.Name, tier.Databases))
			return
		}
		id, now := "db_"+randomToken(12), time.Now().UTC()
		_, err := s.db.Exec(`INSERT INTO data_namespaces
			(id,user_id,name,slug,vector_project_id,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$6)`, id, u.ID, name, slug,
			nullableString(vectorProjectID != "", vectorProjectID), now)
		if err != nil {
			sendError(ctx, 409, "a database with that name already exists")
			return
		}
		ctx.SetStatusCode(201)
		sendJSON(ctx, map[string]any{"ok": true, "database": dataNamespaceJSON(&dataNamespace{
			ID: id, UserID: u.ID, Name: name, Slug: slug, VectorProjectID: vectorProjectID,
			CreatedAt: now, UpdatedAt: now,
		})})
		return
	}
	if !ctx.IsGet() {
		sendError(ctx, 405, "method not allowed")
		return
	}
	rows, err := s.db.Query(`SELECT n.id,n.user_id,n.name,n.slug,
		COALESCE(n.vector_project_id,''),COUNT(r.record_key),n.created_at,n.updated_at
		FROM data_namespaces n LEFT JOIN data_records r ON r.namespace_id=n.id
		WHERE n.user_id=$1 GROUP BY n.id ORDER BY n.updated_at DESC`, u.ID)
	if err != nil {
		sendError(ctx, 500, "could not list databases")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var namespace dataNamespace
		if rows.Scan(&namespace.ID, &namespace.UserID, &namespace.Name, &namespace.Slug,
			&namespace.VectorProjectID, &namespace.Records, &namespace.CreatedAt, &namespace.UpdatedAt) == nil {
			items = append(items, dataNamespaceJSON(&namespace))
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "databases": items,
		"limits": map[string]any{"databases": dataTierFor(u).Databases, "records_per_database": dataTierFor(u).RecordsPerDatabase,
			"record_bytes": dataRecordMaxBytes, "bytes_per_database": dataTierFor(u).BytesPerDatabase, "tier": dataTierFor(u).Name}})
}

func (s *Server) dataNamespace(userID, namespaceID string) (*dataNamespace, error) {
	var namespace dataNamespace
	err := s.db.QueryRow(`SELECT id,user_id,name,slug,COALESCE(vector_project_id,''),created_at,updated_at
		FROM data_namespaces WHERE id=$1 AND user_id=$2`, namespaceID, userID).Scan(
		&namespace.ID, &namespace.UserID, &namespace.Name, &namespace.Slug,
		&namespace.VectorProjectID, &namespace.CreatedAt, &namespace.UpdatedAt)
	return &namespace, err
}

func (s *Server) handleDataNamespaceRoute(ctx *fasthttp.RequestCtx, tail string) {
	u := s.dataUser(ctx)
	if u == nil {
		return
	}
	parts := strings.Split(strings.Trim(tail, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "records" {
		sendError(ctx, 404, "database operation not found")
		return
	}
	namespace, err := s.dataNamespace(u.ID, parts[0])
	if err != nil {
		sendError(ctx, 404, "database not found")
		return
	}
	meter := "db.write"
	if ctx.IsGet() {
		meter = "db.read"
	}
	if s.meters != nil && s.meterBlocked(u, meter) {
		sendError(ctx, 402, "free allowance used — add credits at "+s.cfg.TopUpURL)
		return
	}
	switch {
	case ctx.IsGet():
		s.meterAdd(u.ID, meter, 1)
		s.handleDataRecordsList(ctx, namespace)
	case ctx.IsPost() || ctx.IsPut():
		s.meterAdd(u.ID, meter, 1)
		s.handleDataRecordUpsert(ctx, u, namespace)
	case ctx.IsDelete():
		s.meterAdd(u.ID, meter, 1)
		s.handleDataRecordDelete(ctx, namespace)
	default:
		sendError(ctx, 405, "method not allowed")
	}
}

func (s *Server) handleDataRecordsList(ctx *fasthttp.RequestCtx, namespace *dataNamespace) {
	rows, err := s.db.Query(`SELECT record_key,value::text,search_text,created_at,updated_at
		FROM data_records WHERE namespace_id=$1 ORDER BY updated_at DESC LIMIT $2`, namespace.ID, dataListLimit)
	if err != nil {
		sendError(ctx, 500, "could not list records")
		return
	}
	defer rows.Close()
	records := make([]map[string]any, 0)
	for rows.Next() {
		var key, raw, searchText string
		var created, updated time.Time
		if rows.Scan(&key, &raw, &searchText, &created, &updated) == nil {
			records = append(records, map[string]any{"key": key, "value": json.RawMessage(raw),
				"search_text": searchText, "created_at": created, "updated_at": updated})
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "database": dataNamespaceJSON(namespace), "records": records})
}

type dataRecordRequest struct {
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value"`
	SearchText string          `json:"search_text"`
}

func validateDataRecord(req dataRecordRequest, maxSearchBytes int) error {
	if strings.TrimSpace(req.Key) == "" || len(req.Key) > 128 || strings.ContainsAny(req.Key, "\x00\r\n") {
		return errors.New("key must be 1-128 characters without control characters")
	}
	if len(req.Value) == 0 || len(req.Value) > dataRecordMaxBytes || !json.Valid(req.Value) {
		return fmt.Errorf("value must be valid JSON no larger than %d bytes", dataRecordMaxBytes)
	}
	if len(req.SearchText) > maxSearchBytes {
		return fmt.Errorf("search text must be no larger than %d bytes", maxSearchBytes)
	}
	return nil
}

func (s *Server) handleDataRecordUpsert(ctx *fasthttp.RequestCtx, u *User, namespace *dataNamespace) {
	var req dataRecordRequest
	if json.Unmarshal(ctx.PostBody(), &req) != nil {
		sendError(ctx, 400, "invalid JSON")
		return
	}
	if err := validateDataRecord(req, s.cfg.VectorMaxDocumentBytes); err != nil {
		sendError(ctx, 400, err.Error())
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		sendError(ctx, 500, "could not save record")
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	_, err = tx.Exec(`INSERT INTO data_records(namespace_id,record_key,value,search_text,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(namespace_id,record_key) DO UPDATE SET
		value=EXCLUDED.value,search_text=EXCLUDED.search_text,updated_at=EXCLUDED.updated_at`,
		namespace.ID, req.Key, string(req.Value), strings.TrimSpace(req.SearchText), now)
	var count, usedBytes int64
	if err == nil {
		err = tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(pg_column_size(value)+octet_length(search_text)+octet_length(record_key)),0)
			FROM data_records WHERE namespace_id=$1`, namespace.ID).Scan(&count, &usedBytes)
	}
	if err == nil && count > dataTierFor(u).RecordsPerDatabase {
		err = errDataRecordLimit
	}
	if err == nil && usedBytes > dataTierFor(u).BytesPerDatabase {
		err = errDataStorageLimit
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE data_namespaces SET updated_at=$2 WHERE id=$1`, namespace.ID, now)
	}
	if err != nil {
		status := 500
		if errors.Is(err, errDataRecordLimit) || errors.Is(err, errDataStorageLimit) {
			status = 413
		}
		sendError(ctx, status, err.Error())
		return
	}
	if err = tx.Commit(); err != nil {
		sendError(ctx, 500, "could not save record")
		return
	}
	vector := s.syncDataRecord(u, namespace, req.Key, strings.TrimSpace(req.SearchText))
	sendJSON(ctx, map[string]any{"ok": true, "record": map[string]any{
		"key": req.Key, "value": req.Value, "search_text": strings.TrimSpace(req.SearchText), "updated_at": now,
	}, "records": count, "used_bytes": usedBytes, "vector": vector})
}

func dataVectorDocumentID(namespaceID, key string) string {
	return "data:" + namespaceID + ":" + hashToken(key)[:18]
}

func (s *Server) syncDataRecord(u *User, namespace *dataNamespace, key, searchText string) map[string]any {
	if namespace.VectorProjectID == "" {
		return map[string]any{"status": "not_linked"}
	}
	p, err := s.vectorProject(u.ID, namespace.VectorProjectID)
	if err != nil {
		return map[string]any{"status": "degraded", "error": "linked vector project is unavailable"}
	}
	documentID := dataVectorDocumentID(namespace.ID, key)
	if searchText == "" {
		changed, revision, removeErr := s.removeVectorDocument(p, documentID)
		if removeErr != nil {
			return map[string]any{"status": "degraded", "error": removeErr.Error()}
		}
		if changed {
			if removeErr = s.resetVectorProject(p, revision); removeErr != nil {
				return map[string]any{"status": "degraded", "error": "record saved; vector index will resync"}
			}
		}
		return map[string]any{"status": "not_indexed"}
	}
	if !u.Funded() {
		changed, revision, removeErr := s.removeVectorDocument(p, documentID)
		if removeErr == nil && changed {
			removeErr = s.resetVectorProject(p, revision)
		}
		if removeErr != nil {
			return map[string]any{"status": "degraded", "error": "record saved; stale vector data could not be removed yet"}
		}
		return map[string]any{"status": "needs_credits", "error": "record saved; add credits to index it"}
	}
	document := vectorDocument{ID: documentID, Text: searchText, Metadata: map[string]any{
		"source": "managed-data", "database_id": namespace.ID, "record_key": key,
	}}
	count, revision, err := s.storeVectorDocuments(p, []vectorDocument{document})
	if err == nil {
		err = s.syncVectorDocuments(p, []vectorDocument{document}, revision)
	}
	if err != nil {
		_, _ = s.db.Exec(`UPDATE vector_projects SET status='degraded' WHERE id=$1`, p.ID)
		return map[string]any{"status": "degraded", "error": "record saved; vector index will resync"}
	}
	_, _ = s.db.Exec(`UPDATE vector_projects SET status='ready' WHERE id=$1`, p.ID)
	var charge any
	if s.bill != nil {
		charged, chargeErr := s.bill.chargeVector(u, p.ID, "upsert", 1,
			"folded-static-int8-512", 0, s.cfg.VectorUpsertCredits)
		if chargeErr == nil {
			charge = charged
		}
	}
	return map[string]any{"status": "ready", "document_id": documentID,
		"vector_count": count, "revision": revision, "charge": charge}
}

func (s *Server) removeVectorDocument(p *vectorProject, documentID string) (bool, int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM vector_documents WHERE project_id=$1 AND external_id=$2`, p.ID, documentID)
	if err != nil {
		return false, 0, err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return false, p.Revision, tx.Commit()
	}
	var count, revision int64
	if err = tx.QueryRow(`SELECT COUNT(*) FROM vector_documents WHERE project_id=$1`, p.ID).Scan(&count); err == nil {
		err = tx.QueryRow(`UPDATE vector_projects SET vector_count=$1,revision=revision+1,
			status='syncing',updated_at=$2 WHERE id=$3 RETURNING revision`, count, time.Now().UTC(), p.ID).Scan(&revision)
	}
	if err == nil {
		err = tx.Commit()
	}
	return true, revision, err
}

func (s *Server) resetVectorProject(p *vectorProject, revision int64) error {
	if s.vector == nil {
		return errors.New("vector worker unavailable")
	}
	release := s.acquireVector(scaleKindForProject(p))
	defer release()
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
	if err := s.hydrateVectorProject(s.vector, p.ID, revision); err != nil {
		return err
	}
	_, _ = s.db.Exec(`UPDATE vector_projects SET status='ready' WHERE id=$1`, p.ID)
	return nil
}

func (s *Server) handleDataRecordDelete(ctx *fasthttp.RequestCtx, namespace *dataNamespace) {
	var req struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(ctx.PostBody(), &req) != nil || strings.TrimSpace(req.Key) == "" {
		sendError(ctx, 400, "record key is required")
		return
	}
	result, err := s.db.Exec(`DELETE FROM data_records WHERE namespace_id=$1 AND record_key=$2`, namespace.ID, req.Key)
	if err != nil {
		sendError(ctx, 500, "could not delete record")
		return
	}
	deleted, _ := result.RowsAffected()
	if deleted == 0 {
		sendError(ctx, 404, "record not found")
		return
	}
	vectorStatus := map[string]any{"status": "not_linked"}
	if namespace.VectorProjectID != "" {
		if p, projectErr := s.vectorProject(namespace.UserID, namespace.VectorProjectID); projectErr == nil {
			changed, revision, removeErr := s.removeVectorDocument(p, dataVectorDocumentID(namespace.ID, req.Key))
			if removeErr != nil {
				vectorStatus = map[string]any{"status": "degraded", "error": "record deleted; vector index will resync"}
			} else if changed {
				if removeErr = s.resetVectorProject(p, revision); removeErr != nil {
					vectorStatus = map[string]any{"status": "degraded", "error": "record deleted; vector index will resync"}
				} else {
					vectorStatus = map[string]any{"status": "ready"}
				}
			}
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "deleted": req.Key, "vector": vectorStatus})
}
