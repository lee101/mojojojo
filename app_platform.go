package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	maxProjectFiles       = 128
	maxProjectSourceBytes = 2 << 20
	maxFunctionBody       = 1 << 20
	publicFunctionTimeout = 10_000
)

func validProjectPath(raw string) bool {
	clean := path.Clean(strings.ReplaceAll(raw, "\\", "/"))
	return raw != "" && len(raw) <= 180 && clean == raw && clean != "." && clean != ".." &&
		!strings.HasPrefix(clean, "../") && !strings.HasPrefix(clean, "/")
}

func validFunctionName(name string) bool {
	if name == "" || len(name) > 48 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func cleanTags(tags []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, min(len(tags), 8))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || len(tag) > 24 || seen[tag] {
			continue
		}
		valid := true
		for _, r := range tag {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				valid = false
			}
		}
		if valid {
			seen[tag] = true
			out = append(out, tag)
		}
		if len(out) == 8 {
			break
		}
	}
	return out
}

type projectFilesRequest struct {
	Files    map[string]string `json:"files"`
	Revision string            `json:"revision,omitempty"`
}

func projectRevision(updated time.Time) string {
	return updated.UTC().Format(time.RFC3339Nano)
}

func sourceContentType(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "text/plain; charset=utf-8"
}

type projectFileQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

func queryProjectFiles(q projectFileQueryer, query string, args ...any) (map[string]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := map[string]string{}
	for rows.Next() {
		var name, content string
		if err := rows.Scan(&name, &content); err != nil {
			return nil, err
		}
		files[name] = content
	}
	return files, rows.Err()
}

func sameProjectFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, content := range a {
		if other, ok := b[name]; !ok || other != content {
			return false
		}
	}
	return true
}

func (s *Server) projectVersionFiles(projectID, userID, versionID string) (map[string]string, time.Time, error) {
	var raw []byte
	var revision time.Time
	err := s.db.QueryRow(`SELECT files,revision FROM project_versions WHERE id=$1 AND project_id=$2 AND user_id=$3`,
		versionID, projectID, userID).Scan(&raw, &revision)
	if err != nil {
		return nil, time.Time{}, err
	}
	files := map[string]string{}
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, time.Time{}, err
	}
	return files, revision, nil
}

func (s *Server) handleProjectFiles(ctx *fasthttp.RequestCtx, projectID string) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if !s.projectOwned(u.ID, projectID) {
		sendError(ctx, 404, "project not found")
		return
	}
	if ctx.IsGet() {
		var sourceRevision time.Time
		if err := s.db.QueryRow(`SELECT COALESCE(source_revision,updated_at) FROM projects WHERE id=$1 AND user_id=$2`, projectID, u.ID).Scan(&sourceRevision); err != nil {
			sendError(ctx, 404, "project not found")
			return
		}
		versionID := string(ctx.QueryArgs().Peek("version"))
		var files map[string]string
		var revision time.Time
		var err error
		if versionID != "" {
			files, revision, err = s.projectVersionFiles(projectID, u.ID, versionID)
		} else {
			files, err = queryProjectFiles(s.db, `SELECT path,content FROM project_files WHERE project_id=$1 ORDER BY path`, projectID)
			revision = sourceRevision
			var latest sql.NullString
			_ = s.db.QueryRow(`SELECT id FROM project_versions WHERE project_id=$1 ORDER BY created_at DESC LIMIT 1`, projectID).Scan(&latest)
			if latest.Valid {
				versionID = latest.String
			}
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				sendError(ctx, 404, "project version not found")
				return
			}
			sendError(ctx, 500, "could not load project files")
			return
		}
		sendJSON(ctx, map[string]any{"ok": true, "files": files, "revision": projectRevision(revision),
			"mainline_revision": projectRevision(sourceRevision), "version_id": versionID, "historical": string(ctx.QueryArgs().Peek("version")) != ""})
		return
	}
	if !ctx.IsPut() {
		sendError(ctx, 405, "method not allowed")
		return
	}
	var req projectFilesRequest
	if json.Unmarshal(ctx.PostBody(), &req) != nil || req.Files == nil {
		sendError(ctx, 400, "files object is required")
		return
	}
	if len(req.Files) == 0 || len(req.Files) > maxProjectFiles {
		sendError(ctx, 400, fmt.Sprintf("project must contain 1 to %d files", maxProjectFiles))
		return
	}
	total := 0
	for name, content := range req.Files {
		if !validProjectPath(name) {
			sendError(ctx, 400, "unsafe project path: "+name)
			return
		}
		total += len(content)
	}
	if total > maxProjectSourceBytes {
		sendError(ctx, 413, "project source is too large")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		sendError(ctx, 500, "could not save project")
		return
	}
	var current time.Time
	if err = tx.QueryRow(`SELECT COALESCE(source_revision,updated_at) FROM projects WHERE id=$1 AND user_id=$2 FOR UPDATE`, projectID, u.ID).Scan(&current); err != nil {
		_ = tx.Rollback()
		sendError(ctx, 404, "project not found")
		return
	}
	currentFiles, err := queryProjectFiles(tx, `SELECT path,content FROM project_files WHERE project_id=$1 ORDER BY path`, projectID)
	if err != nil {
		_ = tx.Rollback()
		sendError(ctx, 500, "could not compare project files")
		return
	}
	if sameProjectFiles(currentFiles, req.Files) {
		_ = tx.Rollback()
		var latest sql.NullString
		_ = s.db.QueryRow(`SELECT id FROM project_versions WHERE project_id=$1 ORDER BY created_at DESC LIMIT 1`, projectID).Scan(&latest)
		sendJSON(ctx, map[string]any{"ok": true, "changed": false, "file_count": len(req.Files), "size_bytes": total,
			"revision": projectRevision(current), "version_id": latest.String})
		return
	}
	if req.Revision != "" && req.Revision != projectRevision(current) {
		_ = tx.Rollback()
		ctx.SetStatusCode(409)
		sendJSON(ctx, map[string]any{"error": "this project changed in another tab; reload before overwriting it", "revision": projectRevision(current)})
		return
	}
	// PostgreSQL timestamptz stores microseconds. Returning a finer timestamp
	// would make this tab's very next save conflict with its own prior write.
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = tx.Exec(`DELETE FROM project_files WHERE project_id=$1`, projectID); err == nil {
		names := make([]string, 0, len(req.Files))
		for name := range req.Files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			_, err = tx.Exec(`INSERT INTO project_files(project_id,path,content,content_type,updated_at) VALUES($1,$2,$3,$4,$5)`,
				projectID, name, req.Files[name], sourceContentType(name), now)
			if err != nil {
				break
			}
		}
	}
	versionID := randomToken(12)
	versionJSON, _ := json.Marshal(req.Files)
	if err == nil {
		_, err = tx.Exec(`INSERT INTO project_versions(id,project_id,user_id,revision,files,file_count,size_bytes,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$4)`, versionID, projectID, u.ID, now, versionJSON, len(req.Files), total)
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE projects SET updated_at=$2,source_revision=$2 WHERE id=$1`, projectID, now)
	}
	if err != nil {
		_ = tx.Rollback()
		sendError(ctx, 500, "could not save project")
		return
	}
	if err = tx.Commit(); err != nil {
		sendError(ctx, 500, "could not save project")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "changed": true, "file_count": len(req.Files), "size_bytes": total,
		"revision": projectRevision(now), "version_id": versionID})
}

func (s *Server) handleProjectVersions(ctx *fasthttp.RequestCtx, projectID string) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if !s.projectOwned(u.ID, projectID) {
		sendError(ctx, 404, "project not found")
		return
	}
	rows, err := s.db.Query(`SELECT id,revision,file_count,size_bytes,created_at FROM project_versions
		WHERE project_id=$1 AND user_id=$2 ORDER BY created_at DESC LIMIT 100`, projectID, u.ID)
	if err != nil {
		sendError(ctx, 500, "could not list project versions")
		return
	}
	defer rows.Close()
	versions := make([]map[string]any, 0)
	for rows.Next() {
		var id string
		var revision, created time.Time
		var count int
		var size int64
		if rows.Scan(&id, &revision, &count, &size, &created) == nil {
			versions = append(versions, map[string]any{"id": id, "revision": projectRevision(revision),
				"file_count": count, "size_bytes": size, "created_at": created})
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "versions": versions})
}

func (s *Server) handleProjectShare(ctx *fasthttp.RequestCtx, projectID string) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if !s.projectOwned(u.ID, projectID) {
		sendError(ctx, 404, "project not found")
		return
	}
	var token sql.NullString
	if err := s.db.QueryRow(`SELECT share_token FROM projects WHERE id=$1 AND user_id=$2`, projectID, u.ID).Scan(&token); err != nil {
		sendError(ctx, 404, "project not found")
		return
	}
	if !token.Valid || token.String == "" {
		token.String, token.Valid = randomToken(24), true
		if _, err := s.db.Exec(`UPDATE projects SET share_token=$3 WHERE id=$1 AND user_id=$2`, projectID, u.ID, token.String); err != nil {
			sendError(ctx, 500, "could not create share link")
			return
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "share_token": token.String})
}

func (s *Server) handleSharedProjectFiles(ctx *fasthttp.RequestCtx, token string) {
	var projectID, userID string
	var sourceRevision time.Time
	if err := s.db.QueryRow(`SELECT id,user_id,COALESCE(source_revision,updated_at) FROM projects WHERE share_token=$1`, token).
		Scan(&projectID, &userID, &sourceRevision); err != nil {
		sendError(ctx, 404, "shared project not found")
		return
	}
	versionID := string(ctx.QueryArgs().Peek("version"))
	var files map[string]string
	var revision time.Time
	var err error
	if versionID != "" {
		files, revision, err = s.projectVersionFiles(projectID, userID, versionID)
	} else {
		files, err = queryProjectFiles(s.db, `SELECT path,content FROM project_files WHERE project_id=$1 ORDER BY path`, projectID)
		revision = sourceRevision
	}
	if err != nil {
		sendError(ctx, 404, "shared project version not found")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "project_id": projectID, "files": files,
		"revision": projectRevision(revision), "mainline_revision": projectRevision(sourceRevision),
		"version_id": versionID, "historical": versionID != "", "read_only": true})
}

// handleProjectDownload exports the editable draft, not the last deployment.
// It deliberately does not validate mojojojo.json: downloading is also the
// escape hatch when a half-finished project cannot be published yet.
func (s *Server) handleProjectDownload(ctx *fasthttp.RequestCtx, projectID string) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	var name string
	if err := s.db.QueryRow(`SELECT name FROM projects WHERE id=$1 AND user_id=$2`, projectID, u.ID).Scan(&name); err != nil {
		sendError(ctx, 404, "project not found")
		return
	}
	rows, err := s.db.Query(`SELECT path,content FROM project_files WHERE project_id=$1 ORDER BY path`, projectID)
	if err != nil {
		sendError(ctx, 500, "could not export project")
		return
	}
	defer rows.Close()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	count := 0
	for rows.Next() {
		var filename, content string
		if rows.Scan(&filename, &content) != nil {
			continue
		}
		writer, createErr := zw.Create(filename)
		if createErr != nil {
			err = createErr
			break
		}
		if _, writeErr := io.WriteString(writer, content); writeErr != nil {
			err = writeErr
			break
		}
		count++
	}
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	if err != nil || count == 0 {
		sendError(ctx, 500, "could not export project")
		return
	}
	filename := projectSlug(name)
	if filename == "" {
		filename = "mojojojo-project"
	}
	ctx.Response.Header.SetContentType("application/zip")
	ctx.Response.Header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename + ".zip"}))
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.SetBody(archive.Bytes())
}

func (s *Server) draftBundle(projectID string) ([]bundleFile, deploymentManifest, error) {
	rows, err := s.db.Query(`SELECT path, content FROM project_files WHERE project_id=$1 ORDER BY path`, projectID)
	if err != nil {
		return nil, deploymentManifest{}, err
	}
	defer rows.Close()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	count := 0
	for rows.Next() {
		var name, content string
		if rows.Scan(&name, &content) != nil {
			continue
		}
		w, e := zw.Create(name)
		if e != nil {
			return nil, deploymentManifest{}, e
		}
		if _, e = io.WriteString(w, content); e != nil {
			return nil, deploymentManifest{}, e
		}
		count++
	}
	if err := zw.Close(); err != nil {
		return nil, deploymentManifest{}, err
	}
	if count == 0 {
		return nil, deploymentManifest{}, errors.New("save project files before publishing")
	}
	return readBundle(archive.Bytes())
}

func (s *Server) storeAppDeployment(u *User, projectID string, files []bundleFile, manifest deploymentManifest) (map[string]any, error) {
	if s.uploads == nil {
		return nil, errors.New("object storage is not configured on this deployment")
	}
	total := int64(0)
	for _, f := range files {
		total += int64(len(f.Body))
	}
	if s.storageUsed(u.ID)+total > s.cfg.StorageMaxBytes {
		return nil, errors.New("account storage quota exceeded")
	}
	deploymentID := randomToken(12)
	manifestJSON, _ := json.Marshal(manifest)
	if _, err := s.db.Exec(`INSERT INTO deployments(id,project_id,user_id,status,manifest,entrypoint,created_at)
		VALUES($1,$2,$3,'uploading',$4,$5,$6)`, deploymentID, projectID, u.ID, string(manifestJSON), manifest.Entrypoint, time.Now().UTC()); err != nil {
		return nil, err
	}
	type stored struct {
		file     bundleFile
		key, url string
	}
	items := make([]stored, 0, len(files))
	cleanup := func() {
		for _, item := range items {
			_ = s.uploads.remove(item.key)
		}
	}
	for _, f := range files {
		key, err := s.uploads.siteKey(u.ID, projectID, deploymentID, f.Name)
		if err != nil {
			cleanup()
			return nil, err
		}
		objectURL, err := s.uploads.put(key, f.Body, f.ContentType)
		if err != nil {
			cleanup()
			return nil, err
		}
		items = append(items, stored{f, key, objectURL})
	}
	publicURL := strings.TrimRight(s.cfg.SiteBaseURL, "/") + "/p/" + projectID + "/"
	tx, err := s.db.Begin()
	if err != nil {
		cleanup()
		return nil, err
	}
	for _, item := range items {
		_, err = tx.Exec(`INSERT INTO assets(id,user_id,project_id,deployment_id,object_key,filename,content_type,size_bytes,public_url,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, randomToken(12), u.ID, projectID, deploymentID,
			item.key, item.file.Name, item.file.ContentType, len(item.file.Body), item.url, time.Now().UTC())
		if err != nil {
			break
		}
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE deployments SET status='ready',public_url=$2,file_count=$3,size_bytes=$4 WHERE id=$1`, deploymentID, publicURL, len(files), total)
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE projects SET active_deployment_id=$2,runtime=$3,visibility='public',updated_at=$4 WHERE id=$1 AND user_id=$5`, projectID, deploymentID, manifest.Runtime, time.Now().UTC(), u.ID)
	}
	if err != nil {
		_ = tx.Rollback()
		cleanup()
		_, _ = s.db.Exec(`UPDATE deployments SET status='failed' WHERE id=$1`, deploymentID)
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"id": deploymentID, "project_id": projectID, "status": "ready", "url": publicURL,
		"entrypoint": manifest.Entrypoint, "runtime": manifest.Runtime, "file_count": len(files), "size_bytes": total}, nil
}

func (s *Server) handleProjectPublish(ctx *fasthttp.RequestCtx, projectID string) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if !s.projectOwned(u.ID, projectID) {
		sendError(ctx, 404, "project not found")
		return
	}
	u, ok := s.paying(ctx, u)
	if !ok {
		return
	}
	files, manifest, err := s.draftBundle(projectID)
	if err != nil {
		sendError(ctx, 400, err.Error())
		return
	}
	deployment, err := s.storeAppDeployment(u, projectID, files, manifest)
	if err != nil {
		sendError(ctx, 500, "could not publish project: "+err.Error())
		return
	}
	ctx.SetStatusCode(201)
	sendJSON(ctx, map[string]any{"ok": true, "deployment": deployment})
}

func (s *Server) handleProjectSubmit(ctx *fasthttp.RequestCtx, projectID string) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	var category, deployment string
	if err := s.db.QueryRow(`SELECT category,COALESCE(active_deployment_id,'') FROM projects WHERE id=$1 AND user_id=$2`, projectID, u.ID).Scan(&category, &deployment); err != nil {
		sendError(ctx, 404, "project not found")
		return
	}
	if deployment == "" {
		sendError(ctx, 400, "publish the project before submitting it")
		return
	}
	if category != "app" && category != "tool" {
		category = "app"
	}
	id, now := randomToken(12), time.Now().UTC()
	status := "pending"
	if u.IsAdmin {
		status = "approved"
	}
	tx, err := s.db.Begin()
	if err == nil {
		_, err = tx.Exec(`DELETE FROM showcase_submissions WHERE project_id=$1 AND status IN ('pending','approved')`, projectID)
		if err == nil {
			_, err = tx.Exec(`INSERT INTO showcase_submissions(id,project_id,user_id,category,status,submitted_at,reviewed_at,reviewer_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, projectID, u.ID, category, status, now, nullableTime(status == "approved", now), nullableString(status == "approved", u.ID))
		}
		if err == nil {
			_, err = tx.Exec(`UPDATE projects SET showcase_status=$2,visibility='public',updated_at=$3 WHERE id=$1`, projectID, status, now)
		}
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		sendError(ctx, 500, "could not submit project")
		return
	}
	if err = tx.Commit(); err != nil {
		sendError(ctx, 500, "could not submit project")
		return
	}
	ctx.SetStatusCode(201)
	sendJSON(ctx, map[string]any{"ok": true, "submission": map[string]any{"id": id, "status": status, "category": category}})
}

func nullableTime(ok bool, value time.Time) any {
	if ok {
		return value
	}
	return nil
}
func nullableString(ok bool, value string) any {
	if ok {
		return value
	}
	return nil
}

func (s *Server) handleShowcase(ctx *fasthttp.RequestCtx) {
	category := string(ctx.QueryArgs().Peek("category"))
	if category != "app" && category != "tool" {
		category = ""
	}
	query := `SELECT p.id,p.name,p.slug,p.description,p.category,p.tags,p.featured,p.updated_at,u.handle,u.email,d.public_url,d.manifest
		FROM projects p JOIN users u ON u.id=p.user_id JOIN deployments d ON d.id=p.active_deployment_id
		WHERE p.showcase_status='approved' AND p.visibility='public'`
	args := []any{}
	if category != "" {
		query += ` AND p.category=$1`
		args = append(args, category)
	}
	query += ` ORDER BY p.featured DESC,p.updated_at DESC LIMIT 100`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		sendError(ctx, 500, "could not load showcase")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, slug, desc, cat, tagsJSON, handle, email, publicURL, manifestJSON string
		var featured bool
		var updated time.Time
		if rows.Scan(&id, &name, &slug, &desc, &cat, &tagsJSON, &featured, &updated, &handle, &email, &publicURL, &manifestJSON) == nil {
			var tags []string
			_ = json.Unmarshal([]byte(tagsJSON), &tags)
			if handle == "" {
				handle = strings.Split(email, "@")[0]
			}
			item := map[string]any{"id": id, "name": name, "slug": slug, "description": desc, "category": cat, "tags": tags, "featured": featured, "author": handle, "url": publicURL, "updated_at": updated}
			if price := minPriceCredits(manifestJSON); price > 0 {
				// The badge shows that an app charges and that real callers pay it,
				// never what the owner has earned.
				item["price_credits"] = price
				var payers int
				_ = s.db.QueryRow(`SELECT COUNT(DISTINCT payer_id) FROM creator_earnings WHERE project_id=$1 AND status='active' AND created_at > now() - interval '30 days'`, id).Scan(&payers)
				item["paying_callers_30d"] = payers
			}
			items = append(items, item)
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "items": items})
}

func (s *Server) handleShowcaseReview(ctx *fasthttp.RequestCtx, submissionID string) {
	u := s.auth.userFromSession(ctx)
	if u == nil || !u.IsAdmin {
		sendError(ctx, 403, "admin access required")
		return
	}
	var req struct{ Status, Note string }
	if json.Unmarshal(ctx.PostBody(), &req) != nil || (req.Status != "approved" && req.Status != "rejected") {
		sendError(ctx, 400, "status must be approved or rejected")
		return
	}
	if len(req.Note) > 500 {
		sendError(ctx, 400, "note is too long")
		return
	}
	tx, err := s.db.Begin()
	var projectID string
	if err == nil {
		err = tx.QueryRow(`UPDATE showcase_submissions SET status=$2,note=$3,reviewed_at=$4,reviewer_id=$5 WHERE id=$1 AND status='pending' RETURNING project_id`, submissionID, req.Status, req.Note, time.Now().UTC(), u.ID).Scan(&projectID)
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE projects SET showcase_status=$2,featured=FALSE,updated_at=$3 WHERE id=$1`, projectID, req.Status, time.Now().UTC())
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		if errors.Is(err, sql.ErrNoRows) {
			sendError(ctx, 404, "pending submission not found")
		} else {
			sendError(ctx, 500, "could not review submission")
		}
		return
	}
	if err = tx.Commit(); err != nil {
		sendError(ctx, 500, "could not review submission")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "status": req.Status})
}

func (s *Server) loadHostedAsset(projectID, name string) (key, contentType string, err error) {
	var manifestJSON string
	if err = s.db.QueryRow(`SELECT d.manifest::text FROM projects p JOIN deployments d ON d.id=p.active_deployment_id
		WHERE p.id=$1 AND p.visibility='public' AND d.status='ready'`, projectID).Scan(&manifestJSON); err != nil {
		return "", "", err
	}
	var manifest deploymentManifest
	if json.Unmarshal([]byte(manifestJSON), &manifest) != nil {
		return "", "", errors.New("invalid deployment manifest")
	}
	// The conventional functions directory is always private, including helper
	// modules which are not themselves named as entrypoints.
	if name == "mojojojo.json" || strings.HasPrefix(name, "functions/") {
		return "", "", sql.ErrNoRows
	}
	for _, fn := range manifest.Functions {
		if name == fn.Entrypoint {
			return "", "", sql.ErrNoRows
		}
	}
	err = s.db.QueryRow(`SELECT a.object_key,a.content_type FROM projects p JOIN deployments d ON d.id=p.active_deployment_id
		JOIN assets a ON a.deployment_id=d.id WHERE p.id=$1 AND p.visibility='public' AND d.status='ready' AND a.filename=$2`, projectID, name).Scan(&key, &contentType)
	return
}

func (s *Server) handleHostedApp(ctx *fasthttp.RequestCtx, rest string) {
	parts := strings.SplitN(strings.TrimPrefix(rest, "/"), "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		sendError(ctx, 404, "app not found")
		return
	}
	projectID := parts[0]
	name := ""
	if len(parts) == 2 {
		name = parts[1]
	}
	if strings.HasPrefix(name, "api/") {
		s.handleHostedFunction(ctx, projectID, strings.TrimPrefix(name, "api/"), false)
		return
	}
	if ctx.IsPost() {
		sendError(ctx, 405, "method not allowed")
		return
	}
	if name == "" {
		_ = s.db.QueryRow(`SELECT d.entrypoint FROM projects p JOIN deployments d ON d.id=p.active_deployment_id WHERE p.id=$1 AND p.visibility='public'`, projectID).Scan(&name)
	}
	if !validProjectPath(name) {
		sendError(ctx, 404, "file not found")
		return
	}
	key, contentType, err := s.loadHostedAsset(projectID, name)
	if err != nil || s.uploads == nil {
		sendError(ctx, 404, "file not found")
		return
	}
	body, err := s.uploads.get(key, maxDeploymentBytes)
	if err != nil {
		sendError(ctx, 502, "app file is temporarily unavailable")
		return
	}
	ctx.SetContentType(contentType)
	ctx.Response.Header.Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		ctx.Response.Header.Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-downloads; default-src 'self' data: blob:; img-src 'self' data: blob:; media-src 'self' data: blob:; connect-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		ctx.Response.Header.Set("Cache-Control", "no-cache")
	} else {
		ctx.Response.Header.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	ctx.SetBody(body)
}

func (s *Server) projectFunction(projectID, name string, draft bool, userID string) (functionManifest, string, string, error) {
	var manifestJSON, deploymentID, ownerID string
	if draft {
		if err := s.db.QueryRow(`SELECT user_id FROM projects WHERE id=$1 AND user_id=$2`, projectID, userID).Scan(&ownerID); err != nil {
			return functionManifest{}, "", "", err
		}
		files, manifest, err := s.draftBundle(projectID)
		if err != nil {
			return functionManifest{}, "", "", err
		}
		fn, ok := manifest.Functions[name]
		if !ok {
			return fn, "", "", sql.ErrNoRows
		}
		for _, f := range files {
			if f.Name == fn.Entrypoint {
				return fn, string(f.Body), ownerID, nil
			}
		}
		return fn, "", "", sql.ErrNoRows
	}
	err := s.db.QueryRow(`SELECT d.manifest::text,d.id,p.user_id FROM projects p JOIN deployments d ON d.id=p.active_deployment_id WHERE p.id=$1 AND p.visibility='public' AND d.status='ready'`, projectID).Scan(&manifestJSON, &deploymentID, &ownerID)
	if err != nil {
		return functionManifest{}, "", "", err
	}
	var manifest deploymentManifest
	if json.Unmarshal([]byte(manifestJSON), &manifest) != nil {
		return functionManifest{}, "", "", errors.New("invalid deployment manifest")
	}
	fn, ok := manifest.Functions[name]
	if !ok {
		return fn, "", "", sql.ErrNoRows
	}
	var key string
	if err = s.db.QueryRow(`SELECT object_key FROM assets WHERE deployment_id=$1 AND filename=$2`, deploymentID, fn.Entrypoint).Scan(&key); err != nil {
		return fn, "", "", err
	}
	if s.uploads == nil {
		return fn, "", "", errors.New("storage unavailable")
	}
	body, err := s.uploads.get(key, maxProjectSourceBytes)
	return fn, string(body), ownerID, err
}

func pythonFunctionCode(source string) string {
	return source + `

# mojojojo serverless adapter
if __name__ == "__main__":
 import sys as _sys, json as _json, base64 as _base64
 _request = _json.loads(_sys.stdin.read() or "{}")
 _value = handler(_request)
 if isinstance(_value, bytes): _value = {"body": _base64.b64encode(_value).decode(), "is_base64": True}
 elif not isinstance(_value, dict) or not any(k in _value for k in ("body", "status", "headers", "broker")): _value = {"body": _value}
 print("__MOJOJOJO_RESPONSE__" + _json.dumps(_value, default=str))
`
}

func (s *Server) reserveFunctionCall(projectID string) error {
	var calls, limit int
	if err := s.db.QueryRow(`INSERT INTO app_function_usage(project_id,day,calls) VALUES($1,CURRENT_DATE,1)
		ON CONFLICT(project_id,day) DO UPDATE SET calls=app_function_usage.calls+1 RETURNING calls`, projectID).Scan(&calls); err != nil {
		return err
	}
	if err := s.db.QueryRow(`SELECT daily_call_limit FROM projects WHERE id=$1`, projectID).Scan(&limit); err != nil {
		return err
	}
	if calls > limit {
		return errors.New("this app has reached its daily function limit")
	}
	return nil
}

func requestEnvelope(ctx *fasthttp.RequestCtx) map[string]any {
	body := ctx.PostBody()
	var value any
	if strings.Contains(strings.ToLower(string(ctx.Request.Header.ContentType())), "application/json") && json.Unmarshal(body, &value) == nil {
	} else {
		value = string(body)
	}
	query := map[string]string{}
	ctx.QueryArgs().VisitAll(func(k, v []byte) { query[string(k)] = string(v) })
	headers := map[string]string{}
	for _, name := range []string{"Content-Type", "Accept", "X-Requested-With"} {
		if v := string(ctx.Request.Header.Peek(name)); v != "" {
			headers[strings.ToLower(name)] = v
		}
	}
	return map[string]any{"method": string(ctx.Method()), "path": string(ctx.Path()), "query": query, "headers": headers, "body": value}
}

type hostedResponse struct {
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers"`
	Body     any               `json:"body"`
	IsBase64 bool              `json:"is_base64"`
	Broker   *brokerCommand    `json:"broker,omitempty"`
}

func (s *Server) handleHostedFunction(ctx *fasthttp.RequestCtx, projectID, name string, draft bool) {
	ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
	ctx.Response.Header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key")
	ctx.Response.Header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if ctx.IsOptions() {
		ctx.SetStatusCode(204)
		return
	}
	if !validFunctionName(name) {
		sendError(ctx, 404, "function not found")
		return
	}
	if len(ctx.PostBody()) > maxFunctionBody {
		sendError(ctx, 413, "function request is too large")
		return
	}
	requester := ""
	if draft {
		u := s.auth.userFromRequest(ctx)
		if u == nil {
			sendError(ctx, 401, "sign in to preview functions")
			return
		}
		requester = u.ID
	}
	fn, source, ownerID, err := s.projectFunction(projectID, name, draft, requester)
	if err != nil {
		sendError(ctx, 404, "function not found")
		return
	}
	if fn.Billing == "caller" && !draft && !ctx.IsPost() {
		sendError(ctx, 405, "caller-paid functions require POST")
		return
	}
	payer := s.hostedFunctionPayer(fn, draft, ownerID, bearerToken(ctx))
	if payer == nil {
		if fn.Billing == "caller" && !draft {
			sendError(ctx, 401, "a valid mojojojo API key is required for caller-paid compute")
		} else {
			sendError(ctx, 404, "app owner not found")
		}
		return
	}
	if !draft {
		if err := s.reserveFunctionCall(projectID); err != nil {
			sendError(ctx, 429, err.Error())
			return
		}
	}
	s.bill.grantWelcome(payer)
	if !payer.Funded() {
		sendError(ctx, 402, "the compute payer has no paid credits")
		return
	}
	// A priced function charges its creator fee up front, so a caller cannot
	// run it for free by running out of credits halfway.
	if !draft && fn.PriceCredits > 0 {
		if refusal := s.chargeCreatorFee(payer, ownerID, projectID, name, fn.PriceCredits); refusal != nil {
			s.creatorFeeRefused(ctx, refusal, fn.PriceCredits)
			return
		}
	}
	timeout := fn.TimeoutMs
	if timeout <= 0 || timeout > publicFunctionTimeout {
		timeout = publicFunctionTimeout
	}
	stdin, _ := json.Marshal(requestEnvelope(ctx))
	req := RunRequest{Code: source, Stdin: string(stdin), Language: fn.Language, Packages: fn.Packages, TimeoutMs: timeout, GPU: fn.GPU, Source: "app:" + projectID}
	if fn.Language == "python" {
		req.Code = pythonFunctionCode(source)
	}
	result, where, err := s.execute(req)
	if err != nil {
		s.runFailed(ctx, result, err)
		return
	}
	native := result.Accel.NativeCalls > 0
	charge, chargeErr := s.bill.charge(payer, result.BillableMs, req.GPU, native, req.Source)
	if chargeErr != nil {
		log.Printf("app function charge failed project=%s: %v", projectID, chargeErr)
	} else {
		result.Charge = &charge
	}
	_, _ = s.db.Exec(`UPDATE app_function_usage SET billable_ms=billable_ms+$2 WHERE project_id=$1 AND day=CURRENT_DATE`, projectID, result.BillableMs)
	s.remember(requestSource(req), req, result)
	_ = where
	response := hostedResponse{Status: 200, Headers: map[string]string{}, Body: result.Stdout}
	text := result.Stdout
	if at := strings.LastIndex(text, "__MOJOJOJO_RESPONSE__"); at >= 0 {
		text = text[at+len("__MOJOJOJO_RESPONSE__"):]
		text = strings.TrimSpace(strings.Split(text, "\n")[0])
	}
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &response) != nil {
		response.Body = result.Stdout
	}
	if response.Broker != nil {
		response.Headers = map[string]string{"Content-Type": "application/json; charset=utf-8"}
		if !hasCapability(fn, "room-broker") {
			response.Status = 400
			response.Body = map[string]any{"ok": false, "error": "function does not declare the room-broker capability"}
		} else if brokerResult, brokerErr := s.executeBroker(projectID, response.Broker); brokerErr != nil {
			response.Status = 400
			response.Body = map[string]any{"ok": false, "error": brokerErr.Error()}
		} else {
			response.Status = 200
			response.Body = brokerResult
		}
	}
	if response.Status < 200 || response.Status > 599 {
		response.Status = 200
	}
	for key, value := range response.Headers {
		canonical := http.CanonicalHeaderKey(key)
		if canonical == "Content-Type" || canonical == "Content-Disposition" || canonical == "Cache-Control" {
			ctx.Response.Header.Set(canonical, value)
		}
	}
	ctx.SetStatusCode(response.Status)
	if response.IsBase64 {
		encoded, _ := response.Body.(string)
		decoded, e := base64.StdEncoding.DecodeString(encoded)
		if e != nil {
			sendError(ctx, 502, "function returned invalid binary data")
			return
		}
		if len(ctx.Response.Header.ContentType()) == 0 {
			ctx.SetContentType("application/octet-stream")
		}
		ctx.SetBody(decoded)
		return
	}
	if len(ctx.Response.Header.ContentType()) == 0 {
		if _, ok := response.Body.(string); ok {
			ctx.SetContentType("text/plain; charset=utf-8")
		} else {
			ctx.SetContentType("application/json; charset=utf-8")
		}
	}
	if body, ok := response.Body.(string); ok {
		ctx.SetBodyString(body)
	} else {
		encoded, _ := json.Marshal(response.Body)
		ctx.SetBody(encoded)
	}
}

func (s *Server) handleProjectPreviewFunction(ctx *fasthttp.RequestCtx, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		sendError(ctx, 404, "function not found")
		return
	}
	s.handleHostedFunction(ctx, parts[0], parts[1], true)
}
