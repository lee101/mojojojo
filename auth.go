package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie     = "mojojojo_session"
	appID             = "mojojojo"
	keyPrefix         = "mj_live_"
	sessionTTL        = 30 * 24 * time.Hour
	passwordMinLength = 6
	passwordMaxLength = 72
)

var (
	errInsufficientCredits = errors.New("insufficient credits")
	errEmailTaken          = errors.New("an account already exists for that email")
	errInvalidCredentials  = errors.New("email or password is incorrect")
	// A real bcrypt hash used to make unknown-email logins pay the same work as
	// known-email logins. It is deliberately not a valid account password.
	dummyPasswordHash, _ = bcrypt.GenerateFromPassword([]byte(randomToken(24)), bcrypt.DefaultCost)
)

type User struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Handle     string `json:"handle"`
	FreeCredit int    `json:"free_credits"`
	PlanCredit int    `json:"plan_credits"`
	PaidCredit int    `json:"paid_credits"`
	IsAdmin    bool   `json:"is_admin"`
	Via        string `json:"via"`
}

func (u *User) Credits() int { return u.FreeCredit + u.PlanCredit + u.PaidCredit }

// Funded is money the account put in: purchased credits or a live plan.
// Grants alone never unlock our machines.
func (u *User) Funded() bool { return u != nil && u.PaidCredit+u.PlanCredit > 0 }

func (u *User) DisplayName() string {
	if u == nil {
		return ""
	}
	if u.Handle != "" {
		return u.Handle
	}
	if i := strings.IndexByte(u.Email, '@'); i > 0 {
		return u.Email[:i]
	}
	return u.Email
}

// authStore owns mojojojo identities, sessions, API keys and the credit
// ledger. The database passed here is the same mojojojo Postgres database used
// by billing; it is never app.nz's database.
type authStore struct{ db *sql.DB }

func newAuthStore(db *sql.DB) *authStore { return &authStore{db: db} }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func sessionToken(ctx *fasthttp.RequestCtx) string {
	return strings.TrimSpace(string(ctx.Request.Header.Cookie(sessionCookie)))
}

func normalizeEmail(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if len(raw) < 3 || len(raw) > 254 {
		return "", errors.New("enter a valid email address")
	}
	parsed, err := mail.ParseAddress(raw)
	if err != nil || strings.ToLower(parsed.Address) != raw {
		return "", errors.New("enter a valid email address")
	}
	return raw, nil
}

func validatePassword(password string) error {
	if len(password) < passwordMinLength {
		return errors.New("password must be at least 6 characters")
	}
	if len(password) > passwordMaxLength {
		return errors.New("password must be at most 72 characters")
	}
	return nil
}

func (a *authStore) createUser(email, password string) (*User, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	u := &User{ID: randomToken(18), Email: email}
	_, err = a.db.Exec(`INSERT INTO users (id, email, password_hash, created_at)
		VALUES ($1, $2, $3, $4)`, u.ID, email, string(hash), time.Now().UTC())
	if err != nil {
		// Do not leak driver details or the existing account id.
		var exists int
		if scanErr := a.db.QueryRow(`SELECT 1 FROM users WHERE LOWER(email) = LOWER($1)`, email).Scan(&exists); scanErr == nil {
			return nil, errEmailTaken
		}
		return nil, err
	}
	return u, nil
}

func (a *authStore) authenticate(email, password string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var userID, hash string
	err := a.db.QueryRow(`SELECT id, password_hash FROM users
		WHERE LOWER(email) = LOWER($1) AND disabled_at IS NULL`, email).Scan(&userID, &hash)
	if err != nil {
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(password))
		return nil, errInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, errInvalidCredentials
	}
	u := a.userByID(userID)
	if u == nil {
		return nil, errInvalidCredentials
	}
	return u, nil
}

func (a *authStore) createSession(userID, userAgent string) (string, error) {
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	token := randomToken(32)
	now := time.Now().UTC()
	_, err := a.db.Exec(`INSERT INTO sessions
		(id_hash, user_id, created_at, expires_at, last_seen_at, user_agent)
		VALUES ($1, $2, $3, $4, $3, $5)`,
		hashToken(token), userID, now, now.Add(sessionTTL), userAgent)
	return token, err
}

func (a *authStore) deleteSession(token string) error {
	_, err := a.db.Exec(`DELETE FROM sessions WHERE id_hash = $1`, hashToken(token))
	return err
}

func bearerToken(ctx *fasthttp.RequestCtx) string {
	h := strings.TrimSpace(string(ctx.Request.Header.Peek("Authorization")))
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if k := strings.TrimSpace(string(ctx.Request.Header.Peek("X-Api-Key"))); k != "" {
		return k
	}
	return ""
}

func (a *authStore) userFromRequest(ctx *fasthttp.RequestCtx) *User {
	if a == nil {
		return nil
	}
	if u := a.userFromSession(ctx); u != nil {
		return u
	}
	return a.userFromAPIKey(bearerToken(ctx))
}

func (a *authStore) userFromSession(ctx *fasthttp.RequestCtx) *User {
	tok := sessionToken(ctx)
	if tok == "" {
		return nil
	}
	var userID string
	var expiresAt time.Time
	err := a.db.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE id_hash = $1`, hashToken(tok)).
		Scan(&userID, &expiresAt)
	if err != nil || time.Now().After(expiresAt) {
		return nil
	}
	_, _ = a.db.Exec(`UPDATE sessions SET last_seen_at = $1 WHERE id_hash = $2`, time.Now().UTC(), hashToken(tok))
	u := a.userByID(userID)
	if u != nil {
		u.Via = "session"
	}
	return u
}

func (a *authStore) userFromAPIKey(token string) *User {
	if a == nil || !strings.HasPrefix(token, keyPrefix) {
		return nil
	}
	var userID string
	err := a.db.QueryRow(`SELECT user_id FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`, hashToken(token)).Scan(&userID)
	if err != nil {
		return nil
	}
	_, _ = a.db.Exec(`UPDATE api_keys SET last_used_at = $1 WHERE key_hash = $2`, time.Now().UTC(), hashToken(token))
	u := a.userByID(userID)
	if u != nil {
		u.Via = "api_key"
	}
	return u
}

func (a *authStore) userByID(id string) *User {
	var u User
	var planExpire sql.NullTime
	err := a.db.QueryRow(`SELECT id, email, handle, free_credits, plan_credits,
		plan_credits_expire_at, paid_credits, is_admin FROM users
		WHERE id = $1 AND disabled_at IS NULL`, id).
		Scan(&u.ID, &u.Email, &u.Handle, &u.FreeCredit, &u.PlanCredit, &planExpire, &u.PaidCredit, &u.IsAdmin)
	if err != nil {
		return nil
	}
	if planExpire.Valid && planExpire.Time.Before(time.Now()) {
		u.PlanCredit = 0
	}
	return &u
}

type LedgerEntry struct {
	At      time.Time `json:"at"`
	Delta   int       `json:"delta"`
	Reason  string    `json:"reason"`
	App     string    `json:"app"`
	Source  string    `json:"source"`
	Balance int       `json:"balance_after"`
}

func (a *authStore) ledger(userID string, limit int) ([]LedgerEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := a.db.Query(`SELECT created_at, delta, reason, app_id, source, balance_after
		FROM credit_ledger WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.At, &e.Delta, &e.Reason, &e.App, &e.Source, &e.Balance); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (a *authStore) ledgerTotals(userID string) (spent int, granted int) {
	_ = a.db.QueryRow(`SELECT COALESCE(-SUM(delta),0) FROM credit_ledger
		WHERE user_id = $1 AND app_id = $2 AND delta < 0`, userID, appID).Scan(&spent)
	_ = a.db.QueryRow(`SELECT COALESCE(SUM(delta),0) FROM credit_ledger
		WHERE user_id = $1 AND app_id = $2 AND delta > 0`, userID, appID).Scan(&granted)
	return spent, granted
}

type APIKey struct {
	ID       string
	Name     string
	Prefix   string
	Created  time.Time
	LastUsed *time.Time
	Secret   string
}

func (a *authStore) listKeys(userID string) ([]APIKey, error) {
	rows, err := a.db.Query(`SELECT id, name, key_prefix, created_at, last_used_at
		FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var last sql.NullTime
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Created, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			t := last.Time
			k.LastUsed = &t
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (a *authStore) createKey(userID, name string) (APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	secret := keyPrefix + randomToken(24)
	k := APIKey{ID: randomToken(12), Name: name, Prefix: secret[:len(keyPrefix)+6], Created: time.Now().UTC(), Secret: secret}
	_, err := a.db.Exec(`INSERT INTO api_keys
		(id, user_id, name, key_hash, key_prefix, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		k.ID, userID, k.Name, hashToken(secret), k.Prefix, k.Created)
	return k, err
}

func (a *authStore) revokeKey(userID, keyID string) error {
	_, err := a.db.Exec(`UPDATE api_keys SET revoked_at = $1 WHERE id = $2 AND user_id = $3`, time.Now().UTC(), keyID, userID)
	return err
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
