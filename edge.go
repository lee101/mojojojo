package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

// Tickets: how a browser is allowed to talk to the edge without talking to us
// first.
//
// The edge only earns its keep if the caller reaches it directly. Routing a
// Californian through Auckland to get to a Worker in San Jose is slower than
// the box in Auckland, so "we proxy to the edge" is a tier that helps nobody
// except our own queue.
//
// So the control plane hands out a ticket: a signed statement that a named
// account may spend up to so many milliseconds at the edge before a deadline.
// The Worker verifies the signature with a secret only the two of us have,
// runs the program, and signs a receipt for what it actually used. The browser
// brings the receipt back and we bill it.
//
// What this trades: a caller who never returns a receipt has had a free run,
// capped at the ticket's `max_ms` and one ticket at a time per account. That is
// a bounded, deliberate loss — the alternative is billing before the work,
// which charges people for runs that never happened. Compute is cheap and
// wrong charges are not.

const (
	// A ticket is for the run the caller is about to make, not for keeping.
	ticketTTL = 2 * time.Minute
	// The most an unsettled edge run can cost us, per outstanding ticket.
	ticketMaxMs = 30_000
	// Outstanding tickets per account. More than a couple means either a very
	// parallel notebook or somebody collecting free runs; both are better served
	// by making them settle.
	maxOutstanding = 4
)

type edgeTicket struct {
	User    string `json:"u"`
	Expires int64  `json:"x"`
	MaxMs   int    `json:"m"`
	Nonce   string `json:"n"`
}

func createEdgeTables(db *sql.DB) error {
	return migrateDatabase(db)
}

// mintTicket issues one ticket, or nothing if this account already has too many
// in the air.
func (s *Server) mintTicket(userID string) (string, int, bool) {
	if s.cfg.EdgeURL == "" || s.cfg.EdgeToken == "" || s.db == nil {
		return "", 0, false
	}
	var open int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM edge_tickets
		WHERE user_id = $1 AND settled_at IS NULL AND expires_at > $2`,
		userID, time.Now().UTC()).Scan(&open)
	if err != nil || open >= maxOutstanding {
		return "", 0, false
	}
	nonce := randomToken(16)
	now := time.Now().UTC()
	ticket := edgeTicket{
		User: userID, Expires: now.Add(ticketTTL).Unix(),
		MaxMs: ticketMaxMs, Nonce: nonce,
	}
	payload, err := json.Marshal(ticket)
	if err != nil {
		return "", 0, false
	}
	if _, err := s.db.Exec(`
		INSERT INTO edge_tickets (nonce, user_id, issued_at, expires_at)
		VALUES ($1, $2, $3, $4)`,
		nonce, userID, now, now.Add(ticketTTL)); err != nil {
		return "", 0, false
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + sign(s.cfg.EdgeToken, encoded), ticket.MaxMs, true
}

func sign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// receiptFor is what the edge signs when a run is over: the ticket it was for,
// and the milliseconds it used. Recomputed here rather than trusted, which is
// the whole point — the number a browser hands us is a number a browser could
// have made up.
func receiptFor(secret, nonce string, ms float64) string {
	return sign(secret, nonce+":"+strconv.FormatFloat(ms, 'f', 3, 64))
}

// handleSettle bills a run that happened at the edge.
func (s *Server) handleSettle(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromRequest(ctx)
	if user == nil {
		sendError(ctx, 401, "sign in at "+s.cfg.LoginURL)
		return
	}
	var req struct {
		Ticket  string  `json:"ticket"`
		Ms      float64 `json:"ms"`
		Receipt string  `json:"receipt"`
		Native  bool    `json:"native"`
		Source  string  `json:"source"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		sendError(ctx, 400, "invalid json body")
		return
	}
	ticket, err := s.openTicket(req.Ticket)
	if err != nil {
		sendError(ctx, 400, err.Error())
		return
	}
	if ticket.User != user.ID {
		// Somebody else's ticket. Refused rather than billed to whoever
		// presented it — a receipt is not a bearer instrument.
		sendError(ctx, 403, "this ticket belongs to another account")
		return
	}
	if req.Ms < 0 || req.Ms > float64(ticket.MaxMs) {
		sendError(ctx, 400, "reported milliseconds are outside the ticket")
		return
	}
	want := receiptFor(s.cfg.EdgeToken, ticket.Nonce, req.Ms)
	if subtle.ConstantTimeCompare([]byte(want), []byte(req.Receipt)) != 1 {
		sendError(ctx, 403, "that receipt was not signed by the edge")
		return
	}

	// One settlement per ticket, decided by the database rather than by us:
	// two tabs racing the same receipt must bill once.
	res, err := s.db.Exec(`
		UPDATE edge_tickets SET settled_at = $1, ms = $2
		WHERE nonce = $3 AND settled_at IS NULL`,
		time.Now().UTC(), req.Ms, ticket.Nonce)
	if err != nil {
		sendError(ctx, 500, "could not settle")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		sendJSON(ctx, map[string]any{"ok": true, "already_settled": true})
		return
	}

	source := req.Source
	if source == "" {
		source = "edge"
	}
	ch, err := s.bill.charge(user, req.Ms, false, req.Native, source)
	if err != nil {
		sendError(ctx, 500, "could not charge")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "charge": ch})
}

func (s *Server) openTicket(raw string) (*edgeTicket, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed ticket")
	}
	if subtle.ConstantTimeCompare([]byte(sign(s.cfg.EdgeToken, parts[0])),
		[]byte(parts[1])) != 1 {
		return nil, fmt.Errorf("ticket signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("malformed ticket")
	}
	var ticket edgeTicket
	if err := json.Unmarshal(payload, &ticket); err != nil {
		return nil, fmt.Errorf("malformed ticket")
	}
	if time.Now().Unix() > ticket.Expires {
		return nil, fmt.Errorf("ticket expired")
	}
	return &ticket, nil
}
