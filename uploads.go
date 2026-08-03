package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/valyala/fasthttp"
)

// Storage is deliberately presented as a mojojojo primitive. Provider
// credentials, endpoints, and object keys never cross the public API.

const uploadTicketTTL = 15 * time.Minute

type uploadStore struct {
	client    *minio.Client
	bucket    string
	prefix    string
	publicURL string
}

type uploadTicket struct {
	UserID      string `json:"u"`
	AssetID     string `json:"a"`
	ObjectKey   string `json:"k"`
	Filename    string `json:"n"`
	ContentType string `json:"c"`
	ProjectID   string `json:"p,omitempty"`
	MaxBytes    int64  `json:"m"`
	Expires     int64  `json:"e"`
}

type signUploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ProjectID   string `json:"project_id,omitempty"`
	SizeBytes   int64  `json:"size_bytes"`
}

func newUploadStore(cfg Config) (*uploadStore, error) {
	if cfg.R2Endpoint == "" || cfg.R2AccessKey == "" || cfg.R2SecretKey == "" {
		return nil, nil
	}
	endpoint := strings.TrimPrefix(strings.TrimPrefix(cfg.R2Endpoint, "https://"), "http://")
	client, err := minio.New(strings.TrimRight(endpoint, "/"), &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.R2AccessKey, cfg.R2SecretKey, ""),
		Secure: true, Region: "auto",
	})
	if err != nil {
		return nil, err
	}
	return &uploadStore{client: client, bucket: cfg.R2Bucket, prefix: cfg.R2Prefix,
		publicURL: strings.TrimRight(cfg.CDNBase, "/")}, nil
}

var errUploadPath = errors.New("invalid storage path")

func sanitizeSegment(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}

func safeFilename(filename string) string {
	name := sanitizeSegment(path.Base(strings.ReplaceAll(filename, "\\", "/")), 120)
	name = strings.TrimLeft(name, ".")
	if name == "" {
		return "file"
	}
	return name
}

func (u *uploadStore) keyFor(userID, filename string) (string, error) {
	user := sanitizeSegment(userID, 64)
	if user == "" {
		return "", errUploadPath
	}
	key := u.prefix + "uploads/" + user + "/" + safeFilename(filename)
	if !strings.HasPrefix(key, u.prefix+"uploads/"+user+"/") || strings.Contains(key, "..") {
		return "", errUploadPath
	}
	return key, nil
}

func (u *uploadStore) siteKey(userID, projectID, deploymentID, name string) (string, error) {
	user := sanitizeSegment(userID, 64)
	projectID, deploymentID = sanitizeSegment(projectID, 64), sanitizeSegment(deploymentID, 64)
	name = strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "./")
	if user == "" || projectID == "" || deploymentID == "" || name == "" || name == "." ||
		strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") {
		return "", errUploadPath
	}
	key := u.prefix + "sites/" + user + "/" + projectID + "/" + deploymentID + "/" + name
	if strings.Contains(key, "..") {
		return "", errUploadPath
	}
	return key, nil
}

func (u *uploadStore) publicFor(key string) string {
	return u.publicURL + "/" + strings.TrimPrefix(key, u.prefix)
}

func (u *uploadStore) put(key string, body []byte, contentType string) (string, error) {
	ctx, cancel := uploadCtx()
	defer cancel()
	_, err := u.client.PutObject(ctx, u.bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{
		ContentType: contentType, CacheControl: "public, max-age=31536000, immutable",
	})
	if err != nil {
		return "", err
	}
	return u.publicFor(key), nil
}

// get keeps provider details behind the storage primitive. It is used by the
// hosted-app gateway so user pages and functions have a stable mojojojo URL
// even when the storage delivery hostname is not configured by a customer.
func (u *uploadStore) get(key string, maxBytes int64) ([]byte, error) {
	ctx, cancel := uploadCtx()
	defer cancel()
	object, err := u.client.GetObject(ctx, u.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer object.Close()
	data, err := io.ReadAll(io.LimitReader(object, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("stored object exceeds delivery limit")
	}
	return data, nil
}

func (u *uploadStore) remove(key string) error {
	ctx, cancel := uploadCtx()
	defer cancel()
	return u.client.RemoveObject(ctx, u.bucket, key, minio.RemoveObjectOptions{})
}

func (s *Server) storageUsed(userID string) int64 {
	var used int64
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(size_bytes), 0) FROM assets WHERE user_id = $1`, userID).Scan(&used)
	return used
}

func (s *Server) projectOwned(userID, projectID string) bool {
	if projectID == "" {
		return true
	}
	var ok bool
	_ = s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1 AND user_id = $2)`, projectID, userID).Scan(&ok)
	return ok
}

func (s *Server) recordAsset(t uploadTicket, size int64, url string) error {
	var project any
	if t.ProjectID != "" {
		project = t.ProjectID
	}
	_, err := s.db.Exec(`INSERT INTO assets
		(id, user_id, project_id, object_key, filename, content_type, size_bytes, public_url, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (object_key) DO UPDATE SET size_bytes = EXCLUDED.size_bytes,
			content_type = EXCLUDED.content_type, public_url = EXCLUDED.public_url`,
		t.AssetID, t.UserID, project, t.ObjectKey, t.Filename, t.ContentType, size, url, time.Now().UTC())
	return err
}

func (s *Server) signUpload(t uploadTicket) (string, error) {
	payload, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(s.cfg.StorageSigningKey))
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Server) verifyUpload(token string) (uploadTicket, error) {
	var t uploadTicket
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return t, errors.New("invalid upload ticket")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(sig) != parts[1] {
		return t, errors.New("invalid upload ticket")
	}
	mac := hmac.New(sha256.New, []byte(s.cfg.StorageSigningKey))
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return t, errors.New("invalid upload ticket")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(payload, &t) != nil || t.Expires < time.Now().Unix() ||
		t.UserID == "" || t.AssetID == "" || t.ObjectKey == "" || t.MaxBytes <= 0 {
		return uploadTicket{}, errors.New("expired or invalid upload ticket")
	}
	return t, nil
}

func (s *Server) handleUploadSign(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if s.uploads == nil {
		sendError(ctx, 503, "object storage is not configured on this deployment")
		return
	}
	var req signUploadRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		sendError(ctx, 400, "invalid JSON")
		return
	}
	if req.SizeBytes <= 0 || req.SizeBytes > s.cfg.StorageMaxUpload {
		sendError(ctx, 413, fmt.Sprintf("file size must be between 1 and %d bytes", s.cfg.StorageMaxUpload))
		return
	}
	if s.storageUsed(user.ID)+req.SizeBytes > s.cfg.StorageMaxBytes {
		sendError(ctx, 413, "account storage quota exceeded")
		return
	}
	if !s.projectOwned(user.ID, req.ProjectID) {
		sendError(ctx, 404, "project not found")
		return
	}
	assetID := randomToken(12)
	filename := safeFilename(req.Filename)
	key, err := s.uploads.keyFor(user.ID, assetID+"-"+filename)
	if err != nil {
		sendError(ctx, 400, err.Error())
		return
	}
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" || len(contentType) > 128 || strings.ContainsAny(contentType, "\r\n") {
		contentType = "application/octet-stream"
	}
	t := uploadTicket{UserID: user.ID, AssetID: assetID, ObjectKey: key, Filename: filename,
		ContentType: contentType, ProjectID: req.ProjectID, MaxBytes: req.SizeBytes,
		Expires: time.Now().Add(uploadTicketTTL).Unix()}
	token, err := s.signUpload(t)
	if err != nil {
		sendError(ctx, 500, "could not create upload ticket")
		return
	}
	sendJSON(ctx, map[string]any{
		"ok": true, "method": "PUT", "upload_url": s.cfg.SiteBaseURL + "/v1/uploads/" + token,
		"asset_url": s.uploads.publicFor(key), "expires_at": time.Unix(t.Expires, 0).UTC(),
		"max_bytes": t.MaxBytes,
	})
}

func (s *Server) handleSignedUpload(ctx *fasthttp.RequestCtx, token string) {
	if s.uploads == nil {
		sendError(ctx, 503, "object storage is not configured on this deployment")
		return
	}
	t, err := s.verifyUpload(token)
	if err != nil {
		sendError(ctx, 401, err.Error())
		return
	}
	body := ctx.PostBody()
	if len(body) == 0 || int64(len(body)) > t.MaxBytes || int64(len(body)) > s.cfg.StorageMaxUpload {
		sendError(ctx, 413, "upload does not match its signed size limit")
		return
	}
	if s.storageUsed(t.UserID)+int64(len(body)) > s.cfg.StorageMaxBytes {
		sendError(ctx, 413, "account storage quota exceeded")
		return
	}
	url, err := s.uploads.put(t.ObjectKey, body, t.ContentType)
	if err != nil {
		sendError(ctx, 502, "object storage temporarily unavailable")
		return
	}
	if err := s.recordAsset(t, int64(len(body)), url); err != nil {
		_ = s.uploads.remove(t.ObjectKey)
		sendError(ctx, 500, "could not record uploaded asset")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "id": t.AssetID, "url": url,
		"filename": t.Filename, "bytes": len(body), "content_type": t.ContentType})
}

func (s *Server) handleUpload(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if s.uploads == nil {
		sendError(ctx, 503, "object storage is not configured on this deployment")
		return
	}
	body := ctx.PostBody()
	filename := string(ctx.Request.Header.Peek("X-Filename"))
	contentType := string(ctx.Request.Header.ContentType())
	if form, err := ctx.MultipartForm(); err == nil && form != nil {
		files := form.File["file"]
		if len(files) == 0 {
			sendError(ctx, 400, "no file in the form")
			return
		}
		header := files[0]
		if header.Size > s.cfg.StorageMaxUpload {
			sendError(ctx, 413, fmt.Sprintf("file is larger than %d bytes", s.cfg.StorageMaxUpload))
			return
		}
		opened, err := header.Open()
		if err != nil {
			sendError(ctx, 400, "could not read the uploaded file")
			return
		}
		defer opened.Close()
		body, err = io.ReadAll(io.LimitReader(opened, s.cfg.StorageMaxUpload+1))
		if err != nil {
			sendError(ctx, 400, "could not read the uploaded file")
			return
		}
		filename = header.Filename
		if ct := header.Header.Get("Content-Type"); ct != "" {
			contentType = ct
		}
	}
	if len(body) == 0 || int64(len(body)) > s.cfg.StorageMaxUpload {
		sendError(ctx, 413, fmt.Sprintf("file size must be between 1 and %d bytes", s.cfg.StorageMaxUpload))
		return
	}
	if s.storageUsed(user.ID)+int64(len(body)) > s.cfg.StorageMaxBytes {
		sendError(ctx, 413, "account storage quota exceeded")
		return
	}
	if contentType == "" || len(contentType) > 128 || strings.ContainsAny(contentType, "\r\n") ||
		strings.HasPrefix(contentType, "multipart/") {
		contentType = "application/octet-stream"
	}
	filename = safeFilename(filename)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:12])
	key, err := s.uploads.keyFor(user.ID, digest+"-"+filename)
	if err != nil {
		sendError(ctx, 400, err.Error())
		return
	}
	assetID := hashToken(key)[:16]
	url, err := s.uploads.put(key, body, contentType)
	if err != nil {
		sendError(ctx, 502, "object storage temporarily unavailable")
		return
	}
	t := uploadTicket{UserID: user.ID, AssetID: assetID, ObjectKey: key,
		Filename: filename, ContentType: contentType}
	if err := s.recordAsset(t, int64(len(body)), url); err != nil {
		_ = s.uploads.remove(key)
		sendError(ctx, 500, "could not record uploaded asset")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "id": assetID, "url": url,
		"filename": filename, "bytes": len(body), "content_type": contentType,
		"note": "public asset — do not upload secrets"})
}

func (s *Server) handleStorage(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	used := s.storageUsed(user.ID)
	overage := used - s.cfg.StorageFreeBytes
	if overage < 0 {
		overage = 0
	}
	monthly := float64(overage) / float64(1<<30) * s.cfg.StorageGBMonthUSD
	rows, err := s.db.Query(`SELECT id, filename, content_type, size_bytes, public_url, created_at
		FROM assets WHERE user_id = $1 ORDER BY created_at DESC LIMIT 100`, user.ID)
	if err != nil {
		sendError(ctx, 500, "could not read storage usage")
		return
	}
	defer rows.Close()
	assets := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, contentType, url string
		var size int64
		var created time.Time
		if rows.Scan(&id, &name, &contentType, &size, &url, &created) == nil {
			assets = append(assets, map[string]any{"id": id, "filename": name,
				"content_type": contentType, "bytes": size, "url": url, "created_at": created})
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "used_bytes": used,
		"included_bytes": s.cfg.StorageFreeBytes, "quota_bytes": s.cfg.StorageMaxBytes,
		"max_upload_bytes": s.cfg.StorageMaxUpload, "gb_month_usd": s.cfg.StorageGBMonthUSD,
		"estimated_monthly_usd": float64(int(monthly*100+0.5)) / 100, "assets": assets})
}

func (s *Server) handleAssetDelete(ctx *fasthttp.RequestCtx, assetID string) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	if s.uploads == nil {
		sendError(ctx, 503, "object storage is not configured on this deployment")
		return
	}
	var key string
	var deploymentID *string
	err := s.db.QueryRow(`SELECT object_key, deployment_id FROM assets
		WHERE id = $1 AND user_id = $2`, assetID, user.ID).Scan(&key, &deploymentID)
	if err != nil {
		sendError(ctx, 404, "asset not found")
		return
	}
	if deploymentID != nil {
		sendError(ctx, 409, "deployed assets are immutable; deploy a new version")
		return
	}
	if err := s.uploads.remove(key); err != nil {
		sendError(ctx, 502, "object storage temporarily unavailable")
		return
	}
	if _, err := s.db.Exec(`DELETE FROM assets WHERE id = $1 AND user_id = $2`, assetID, user.ID); err != nil {
		sendError(ctx, 500, "could not remove asset metadata")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "deleted": assetID})
}

func uploadCtx() (context.Context, func()) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}
