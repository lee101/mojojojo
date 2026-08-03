package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	maxDeploymentFiles = 512
	maxDeploymentBytes = 128 << 20
)

type projectRequest struct {
	Name        string   `json:"name"`
	Slug        string   `json:"slug"`
	Runtime     string   `json:"runtime,omitempty"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

type functionManifest struct {
	Language     string   `json:"language"`
	Entrypoint   string   `json:"entrypoint"`
	Packages     []string `json:"packages,omitempty"`
	TimeoutMs    int      `json:"timeout_ms,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type computeManifest struct {
	Mode         string `json:"mode,omitempty"`
	Profile      string `json:"profile,omitempty"`
	HealthPath   string `json:"health_path,omitempty"`
	MinInstances int    `json:"min_instances,omitempty"`
}

type deploymentManifest struct {
	Name       string                      `json:"name,omitempty"`
	Entrypoint string                      `json:"entrypoint,omitempty"`
	Runtime    string                      `json:"runtime,omitempty"`
	Functions  map[string]functionManifest `json:"functions,omitempty"`
	Compute    computeManifest             `json:"compute,omitempty"`
}

type bundleFile struct {
	Name        string
	Body        []byte
	ContentType string
}

func projectSlug(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	dash := false
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Server) handleProjects(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if ctx.IsPost() {
		var req projectRequest
		if json.Unmarshal(ctx.PostBody(), &req) != nil {
			sendError(ctx, 400, "invalid JSON")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" || len(name) > 80 {
			sendError(ctx, 400, "project name must be between 1 and 80 characters")
			return
		}
		slug := projectSlug(req.Slug)
		if slug == "" {
			slug = projectSlug(name)
		}
		if slug == "" {
			sendError(ctx, 400, "project needs a URL-safe name")
			return
		}
		runtime := strings.TrimSpace(req.Runtime)
		if runtime == "" {
			runtime = "static"
		}
		if runtime != "static" && runtime != "mojo" && runtime != "app" {
			sendError(ctx, 400, "runtime must be static, app, or mojo")
			return
		}
		category := strings.TrimSpace(req.Category)
		if category == "" {
			category = "app"
		}
		if category != "app" && category != "tool" {
			sendError(ctx, 400, "category must be app or tool")
			return
		}
		description := strings.TrimSpace(req.Description)
		if len(description) > 240 {
			sendError(ctx, 400, "description must be at most 240 characters")
			return
		}
		tags := cleanTags(req.Tags)
		tagsJSON, _ := json.Marshal(tags)
		id, now := randomToken(12), time.Now().UTC()
		_, err := s.db.Exec(`INSERT INTO projects
			(id, user_id, name, slug, runtime, description, category, tags, created_at, updated_at, source_revision)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, $9)`, id, user.ID, name, slug,
			runtime, description, category, string(tagsJSON), now)
		if err != nil {
			sendError(ctx, 409, "a project with that slug already exists")
			return
		}
		ctx.SetStatusCode(201)
		sendJSON(ctx, map[string]any{"ok": true, "project": map[string]any{
			"id": id, "name": name, "slug": slug, "runtime": runtime,
			"description": description, "category": category, "tags": tags, "created_at": now}})
		return
	}

	rows, err := s.db.Query(`SELECT p.id, p.name, p.slug, p.runtime, p.active_deployment_id,
		p.created_at, p.updated_at, COALESCE(d.public_url, '')
		FROM projects p LEFT JOIN deployments d ON d.id = p.active_deployment_id
		WHERE p.user_id = $1 ORDER BY p.updated_at DESC`, user.ID)
	if err != nil {
		sendError(ctx, 500, "could not list projects")
		return
	}
	defer rows.Close()
	projects := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, slug, runtime, url string
		var active sql.NullString
		var created, updated time.Time
		if rows.Scan(&id, &name, &slug, &runtime, &active, &created, &updated, &url) == nil {
			projects = append(projects, map[string]any{"id": id, "name": name, "slug": slug,
				"runtime": runtime, "active_deployment_id": active.String, "url": url,
				"created_at": created, "updated_at": updated})
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "projects": projects})
}

func readBundle(body []byte) ([]bundleFile, deploymentManifest, error) {
	if len(body) == 0 {
		return nil, deploymentManifest{}, errors.New("empty deployment bundle")
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, deploymentManifest{}, errors.New("deployment must be a ZIP archive")
	}
	if len(zr.File) > maxDeploymentFiles {
		return nil, deploymentManifest{}, fmt.Errorf("deployment contains more than %d files", maxDeploymentFiles)
	}
	files := make([]bundleFile, 0, len(zr.File))
	seen := make(map[string]bool)
	var total int64
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		name := strings.ReplaceAll(zf.Name, "\\", "/")
		clean := path.Clean(name)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") ||
			strings.HasPrefix(name, "/") || clean != name || zf.Mode()&os.ModeSymlink != 0 {
			return nil, deploymentManifest{}, fmt.Errorf("unsafe path in deployment: %s", name)
		}
		if seen[clean] {
			return nil, deploymentManifest{}, fmt.Errorf("duplicate path in deployment: %s", clean)
		}
		seen[clean] = true
		total += int64(zf.UncompressedSize64)
		if total > maxDeploymentBytes {
			return nil, deploymentManifest{}, fmt.Errorf("expanded deployment is larger than %d bytes", maxDeploymentBytes)
		}
		r, err := zf.Open()
		if err != nil {
			return nil, deploymentManifest{}, errors.New("could not read deployment archive")
		}
		data, readErr := io.ReadAll(io.LimitReader(r, maxDeploymentBytes-total+int64(zf.UncompressedSize64)+1))
		_ = r.Close()
		if readErr != nil || uint64(len(data)) != zf.UncompressedSize64 {
			return nil, deploymentManifest{}, errors.New("could not read deployment archive")
		}
		contentType := mime.TypeByExtension(path.Ext(clean))
		if contentType == "" {
			contentType = http.DetectContentType(data)
		}
		files = append(files, bundleFile{Name: clean, Body: data, ContentType: contentType})
	}
	if len(files) == 0 {
		return nil, deploymentManifest{}, errors.New("deployment contains no files")
	}
	manifest := deploymentManifest{Entrypoint: "index.html", Runtime: "static"}
	for _, f := range files {
		if f.Name == "mojojojo.json" {
			if err := json.Unmarshal(f.Body, &manifest); err != nil {
				return nil, deploymentManifest{}, errors.New("mojojojo.json is invalid")
			}
			break
		}
	}
	if manifest.Entrypoint == "" {
		manifest.Entrypoint = "index.html"
	}
	manifest.Entrypoint = path.Clean(strings.ReplaceAll(manifest.Entrypoint, "\\", "/"))
	if !seen[manifest.Entrypoint] {
		return nil, deploymentManifest{}, fmt.Errorf("entrypoint %q is not in the bundle", manifest.Entrypoint)
	}
	if manifest.Runtime == "" {
		manifest.Runtime = "static"
	}
	if manifest.Runtime != "static" && manifest.Runtime != "mojo" && manifest.Runtime != "app" {
		return nil, deploymentManifest{}, errors.New("manifest runtime must be static, app, or mojo")
	}
	if manifest.Compute.Mode == "" {
		manifest.Compute.Mode = "scale-to-zero"
	}
	if manifest.Compute.Mode != "scale-to-zero" && manifest.Compute.Mode != "persistent" {
		return nil, deploymentManifest{}, errors.New("compute mode must be scale-to-zero or persistent")
	}
	if manifest.Compute.Mode == "persistent" {
		return nil, deploymentManifest{}, errors.New("persistent services are in limited preview; request reserved capacity before deploying this manifest")
	}
	if manifest.Compute.Profile != "" || manifest.Compute.HealthPath != "" || manifest.Compute.MinInstances != 0 {
		return nil, deploymentManifest{}, errors.New("compute profile, health_path, and min_instances require persistent mode")
	}
	for name, fn := range manifest.Functions {
		if !validFunctionName(name) || (fn.Language != "python" && fn.Language != "mojo") {
			return nil, deploymentManifest{}, fmt.Errorf("invalid function %q", name)
		}
		fn.Entrypoint = path.Clean(strings.ReplaceAll(fn.Entrypoint, "\\", "/"))
		if !seen[fn.Entrypoint] {
			return nil, deploymentManifest{}, fmt.Errorf("function %q entrypoint is not in the bundle", name)
		}
		if len(fn.Packages) > 8 {
			return nil, deploymentManifest{}, fmt.Errorf("function %q has too many packages", name)
		}
		if len(fn.Capabilities) > 4 {
			return nil, deploymentManifest{}, fmt.Errorf("function %q has too many capabilities", name)
		}
		for _, capability := range fn.Capabilities {
			if capability != "room-broker" {
				return nil, deploymentManifest{}, fmt.Errorf("function %q requests unknown capability %q", name, capability)
			}
		}
		manifest.Functions[name] = fn
	}
	return files, manifest, nil
}

func (s *Server) handleDeploymentCreate(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if s.uploads == nil {
		sendError(ctx, 503, "object storage is not configured on this deployment")
		return
	}
	projectID := string(ctx.Request.Header.Peek("X-Project-ID"))
	body := ctx.PostBody()
	if form, err := ctx.MultipartForm(); err == nil && form != nil {
		if values := form.Value["project_id"]; len(values) > 0 {
			projectID = values[0]
		}
		bundles := form.File["bundle"]
		if len(bundles) == 0 {
			bundles = form.File["file"]
		}
		if len(bundles) == 0 {
			sendError(ctx, 400, "no bundle in the form")
			return
		}
		if bundles[0].Size > s.cfg.StorageMaxUpload {
			sendError(ctx, 413, "deployment archive is too large")
			return
		}
		r, err := bundles[0].Open()
		if err != nil {
			sendError(ctx, 400, "could not read deployment archive")
			return
		}
		body, err = io.ReadAll(io.LimitReader(r, s.cfg.StorageMaxUpload+1))
		_ = r.Close()
		if err != nil {
			sendError(ctx, 400, "could not read deployment archive")
			return
		}
	}
	if !s.projectOwned(user.ID, projectID) || projectID == "" {
		sendError(ctx, 404, "project not found")
		return
	}
	if int64(len(body)) > s.cfg.StorageMaxUpload {
		sendError(ctx, 413, "deployment archive is too large")
		return
	}
	files, manifest, err := readBundle(body)
	if err != nil {
		sendError(ctx, 400, err.Error())
		return
	}
	var total int64
	for _, f := range files {
		total += int64(len(f.Body))
	}
	if s.storageUsed(user.ID)+total > s.cfg.StorageMaxBytes {
		sendError(ctx, 413, "account storage quota exceeded")
		return
	}
	deploymentID := randomToken(12)
	manifestJSON, _ := json.Marshal(manifest)
	_, err = s.db.Exec(`INSERT INTO deployments
		(id, project_id, user_id, status, manifest, entrypoint, created_at)
		VALUES ($1, $2, $3, 'uploading', $4, $5, $6)`, deploymentID, projectID, user.ID,
		string(manifestJSON), manifest.Entrypoint, time.Now().UTC())
	if err != nil {
		sendError(ctx, 500, "could not start deployment")
		return
	}
	type stored struct {
		file     bundleFile
		key, url string
	}
	storedFiles := make([]stored, 0, len(files))
	cleanupStored := func() {
		for _, stored := range storedFiles {
			_ = s.uploads.remove(stored.key)
		}
	}
	for _, f := range files {
		key, keyErr := s.uploads.siteKey(user.ID, projectID, deploymentID, f.Name)
		if keyErr != nil {
			err = keyErr
			break
		}
		url, putErr := s.uploads.put(key, f.Body, f.ContentType)
		if putErr != nil {
			err = putErr
			break
		}
		storedFiles = append(storedFiles, stored{file: f, key: key, url: url})
	}
	if err != nil {
		cleanupStored()
		_, _ = s.db.Exec(`UPDATE deployments SET status = 'failed' WHERE id = $1`, deploymentID)
		sendError(ctx, 502, "object storage temporarily unavailable")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		cleanupStored()
		_, _ = s.db.Exec(`UPDATE deployments SET status = 'failed' WHERE id = $1`, deploymentID)
		sendError(ctx, 500, "could not finish deployment")
		return
	}
	entryURL := ""
	for _, stored := range storedFiles {
		if stored.file.Name == manifest.Entrypoint {
			entryURL = stored.url
		}
		_, err = tx.Exec(`INSERT INTO assets
			(id, user_id, project_id, deployment_id, object_key, filename, content_type, size_bytes, public_url, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, randomToken(12), user.ID,
			projectID, deploymentID, stored.key, stored.file.Name, stored.file.ContentType,
			len(stored.file.Body), stored.url, time.Now().UTC())
		if err != nil {
			break
		}
	}
	if manifest.Runtime == "app" || manifest.Runtime == "static" {
		entryURL = strings.TrimRight(s.cfg.SiteBaseURL, "/") + "/p/" + projectID + "/"
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE deployments SET status = 'ready', public_url = $2,
			file_count = $3, size_bytes = $4 WHERE id = $1`, deploymentID, entryURL, len(files), total)
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE projects SET active_deployment_id = $2, runtime = $3,
			visibility = CASE WHEN $3 IN ('app','static') THEN 'public' ELSE visibility END,
			updated_at = $4 WHERE id = $1 AND user_id = $5`, projectID, deploymentID,
			manifest.Runtime, time.Now().UTC(), user.ID)
	}
	if err != nil {
		_ = tx.Rollback()
		cleanupStored()
		_, _ = s.db.Exec(`UPDATE deployments SET status = 'failed' WHERE id = $1`, deploymentID)
		sendError(ctx, 500, "could not finish deployment")
		return
	}
	if err := tx.Commit(); err != nil {
		sendError(ctx, 500, "could not finish deployment")
		return
	}
	ctx.SetStatusCode(201)
	sendJSON(ctx, map[string]any{"ok": true, "deployment": map[string]any{
		"id": deploymentID, "project_id": projectID, "status": "ready", "url": entryURL,
		"entrypoint": manifest.Entrypoint, "runtime": manifest.Runtime,
		"file_count": len(files), "size_bytes": total}})
}

func (s *Server) handleProjectDeployments(ctx *fasthttp.RequestCtx, projectID string) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if !s.projectOwned(user.ID, projectID) {
		sendError(ctx, 404, "project not found")
		return
	}
	rows, err := s.db.Query(`SELECT id, status, entrypoint, public_url, file_count, size_bytes, created_at
		FROM deployments WHERE project_id = $1 AND user_id = $2 ORDER BY created_at DESC LIMIT 50`, projectID, user.ID)
	if err != nil {
		sendError(ctx, 500, "could not list deployments")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, status, entrypoint, url string
		var count int
		var size int64
		var created time.Time
		if rows.Scan(&id, &status, &entrypoint, &url, &count, &size, &created) == nil {
			items = append(items, map[string]any{"id": id, "status": status, "entrypoint": entrypoint,
				"url": url, "file_count": count, "size_bytes": size, "created_at": created})
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "deployments": items})
}

func (s *Server) handleProjectDelete(ctx *fasthttp.RequestCtx, projectID string) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if s.uploads == nil {
		sendError(ctx, 503, "object storage is not configured on this deployment")
		return
	}
	if !s.projectOwned(user.ID, projectID) {
		sendError(ctx, 404, "project not found")
		return
	}
	rows, err := s.db.Query(`SELECT object_key FROM assets WHERE project_id = $1 AND user_id = $2`,
		projectID, user.ID)
	if err != nil {
		sendError(ctx, 500, "could not read project assets")
		return
	}
	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if rows.Scan(&key) == nil {
			keys = append(keys, key)
		}
	}
	_ = rows.Close()
	for _, key := range keys {
		if err := s.uploads.remove(key); err != nil {
			sendError(ctx, 502, "could not remove every project asset; retry deletion")
			return
		}
	}
	result, err := s.db.Exec(`DELETE FROM projects WHERE id = $1 AND user_id = $2`, projectID, user.ID)
	if err != nil {
		sendError(ctx, 500, "could not delete project")
		return
	}
	deleted, _ := result.RowsAffected()
	if deleted == 0 {
		sendError(ctx, 404, "project not found")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "deleted": projectID,
		"assets_removed": len(keys)})
}
