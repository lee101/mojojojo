package main

// Creator pricing: the monetisation half of mojojojo's caller-paid functions.
//
// A function with `billing: caller` and `price_credits: N` charges every caller
// N credits per call on top of the compute, and the app's owner earns a share.
// The rules are deliberately the same as app.nz Pay's, so a creator who knows
// one knows both:
//
//   - the owner earns 80% (CREATOR_SHARE_BPS) of the fee
//   - only credits the caller paid for count (plan + purchased), never the
//     free signup credits, so fresh accounts cannot print money
//   - an owner calling their own function earns nothing
//   - earnings vest after CREATOR_HOLD_DAYS (default 7) so refunds can still land
//   - vested earnings convert 1:1 into the owner's own credits
//
// Cash-out stays with app.nz (Stripe Connect); mojojojo only keeps the ledger.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	maxCreatorPriceCredits = 100000
	defaultCreatorShareBps = 8000
	defaultCreatorHoldDays = 7
)

var errCreatorFeeUnaffordable = errors.New("the caller cannot afford the creator fee")

func migrateCreatorEarnings(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS creator_earnings (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			function TEXT NOT NULL,
			owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			payer_id TEXT NOT NULL,
			fee_credits BIGINT NOT NULL,
			paid_credits BIGINT NOT NULL,
			share_credits BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			available_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS creator_earnings_owner_idx ON creator_earnings(owner_id, status, available_at);
		CREATE TABLE IF NOT EXISTS creator_settlements (
			id TEXT PRIMARY KEY,
			owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			credits BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS creator_settlements_owner_idx ON creator_settlements(owner_id);
	`)
	return err
}

func creatorShareBps() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CREATOR_SHARE_BPS"))); err == nil && n >= 0 && n <= 10000 {
		return n
	}
	return defaultCreatorShareBps
}

func creatorHold() time.Duration {
	days := defaultCreatorHoldDays
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CREATOR_HOLD_DAYS"))); err == nil && n >= 0 {
		days = n
	}
	return time.Duration(days) * 24 * time.Hour
}

// spendSplit is spendReason that also reports how much of the spend came out
// of credits the user paid for (plan + purchased) rather than free credits.
func (b *billing) spendSplit(userID string, credits int, reason string) (taken, after, paidPart int, err error) {
	tx, err := b.db.Begin()
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback()
	var free, plan, paid int
	var planExpire sql.NullTime
	if err := tx.QueryRow(
		`SELECT COALESCE(free_credits,0), COALESCE(plan_credits,0), plan_credits_expire_at,
		        COALESCE(paid_credits,0) FROM users WHERE id = $1 FOR UPDATE`, userID).
		Scan(&free, &plan, &planExpire, &paid); err != nil {
		return 0, 0, 0, err
	}
	if planExpire.Valid && planExpire.Time.Before(time.Now()) {
		plan = 0
	}
	if free+plan+paid < credits {
		return 0, free + plan + paid, 0, nil
	}
	remaining := credits
	useFree := min(remaining, free)
	remaining -= useFree
	usePlan := min(remaining, plan)
	remaining -= usePlan
	usePaid := remaining
	free, plan, paid = free-useFree, plan-usePlan, paid-usePaid
	after = free + plan + paid
	if _, err := tx.Exec("UPDATE users SET free_credits = $1, plan_credits = $2, paid_credits = $3 WHERE id = $4", free, plan, paid, userID); err != nil {
		return 0, 0, 0, err
	}
	if _, err := tx.Exec(
		`INSERT INTO credit_ledger (id, user_id, delta, reason, app_id, source, balance_after, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		randomToken(18), userID, -credits, reason, appID, "spend", after, time.Now().UTC()); err != nil {
		return 0, 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, err
	}
	return credits, after, usePlan + usePaid, nil
}

// chargeCreatorFee takes the fee from the caller, all or nothing, and books the
// owner's share. It returns a non-nil error when the call must be refused.
func (s *Server) chargeCreatorFee(payer *User, ownerID, projectID, function string, fee int) error {
	taken, _, paidPart, err := s.bill.spendSplit(payer.ID, fee, "app-fee:"+projectID+":"+function)
	if err != nil {
		return err
	}
	if taken < fee {
		return errCreatorFeeUnaffordable
	}
	if payer.ID == ownerID || ownerID == "" {
		return nil
	}
	share := paidPart * creatorShareBps() / 10000
	if share <= 0 {
		return nil
	}
	now := time.Now().UTC()
	_, err = s.db.Exec(
		`INSERT INTO creator_earnings (id, project_id, function, owner_id, payer_id, fee_credits, paid_credits, share_credits, status, available_at, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'active',$9,$10)`,
		randomToken(18), projectID, function, ownerID, payer.ID, fee, paidPart, share, now.Add(creatorHold()), now)
	return err
}

func (s *Server) creatorFeeRefused(ctx *fasthttp.RequestCtx, err error, fee int) {
	if errors.Is(err, errCreatorFeeUnaffordable) {
		ctx.SetStatusCode(402)
		sendJSON(ctx, map[string]any{"ok": false, "error": fmt.Sprintf("this function costs %d credits per call plus compute; add credits at %s", fee, s.cfg.TopUpURL),
			"code": "insufficient_credits", "price_credits": fee, "topup_url": s.cfg.TopUpURL})
		return
	}
	sendError(ctx, 500, "could not charge the creator fee")
}

type creatorSummary struct {
	Available int `json:"available_credits"`
	Pending   int `json:"pending_credits"`
	Lifetime  int `json:"lifetime_credits"`
	Converted int `json:"converted_credits"`
}

func (s *Server) creatorEarningsFor(q interface {
	QueryRow(string, ...any) *sql.Row
}, owner string) (creatorSummary, error) {
	var out creatorSummary
	var vested int
	if err := q.QueryRow(
		`SELECT COALESCE(SUM(CASE WHEN available_at <= now() THEN share_credits ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN available_at >  now() THEN share_credits ELSE 0 END),0),
		        COALESCE(SUM(share_credits),0)
		 FROM creator_earnings WHERE owner_id = $1 AND status = 'active'`, owner).Scan(&vested, &out.Pending, &out.Lifetime); err != nil {
		return out, err
	}
	if err := q.QueryRow(`SELECT COALESCE(SUM(credits),0) FROM creator_settlements WHERE owner_id = $1`, owner).Scan(&out.Converted); err != nil {
		return out, err
	}
	out.Available = vested - out.Converted
	return out, nil
}

// GET /v1/earnings and POST /v1/earnings/convert
func (s *Server) handleCreatorEarnings(ctx *fasthttp.RequestCtx) {
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		sendError(ctx, 401, "sign in or send an API key")
		return
	}
	path, method := string(ctx.Path()), string(ctx.Method())
	switch {
	case path == "/v1/earnings" && method == "GET":
		sum, err := s.creatorEarningsFor(s.db, u.ID)
		if err != nil {
			sendError(ctx, 500, "could not load earnings")
			return
		}
		sendJSON(ctx, map[string]any{"ok": true, "earnings": sum, "rules": map[string]any{
			"share_bps": creatorShareBps(), "hold_days": int(creatorHold() / (24 * time.Hour)),
			"earns_on": "credits the caller paid for (plan and purchased); free credits never earn", "self_calls_earn": false}})
	case path == "/v1/earnings/convert" && method == "POST":
		s.creatorConvert(ctx, u)
	default:
		sendError(ctx, 404, "not found")
	}
}

func (s *Server) creatorConvert(ctx *fasthttp.RequestCtx, u *User) {
	var req struct {
		Credits int `json:"credits"`
	}
	if len(ctx.PostBody()) > 0 {
		if json.Unmarshal(ctx.PostBody(), &req) != nil || req.Credits < 0 {
			sendError(ctx, 400, "credits must be a non-negative integer")
			return
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		sendError(ctx, 500, "could not convert earnings")
		return
	}
	defer tx.Rollback()
	// Serialise settlements per owner so two converts cannot spend one balance.
	if _, err := tx.Exec(`SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, u.ID); err != nil {
		sendError(ctx, 500, "could not convert earnings")
		return
	}
	sum, err := s.creatorEarningsFor(tx, u.ID)
	if err != nil {
		sendError(ctx, 500, "could not convert earnings")
		return
	}
	amount := req.Credits
	if amount == 0 {
		amount = sum.Available
	}
	if amount <= 0 || amount > sum.Available {
		sendError(ctx, 409, fmt.Sprintf("only %d vested credits are available", max(sum.Available, 0)))
		return
	}
	now := time.Now().UTC()
	var paid int
	if _, err := tx.Exec(`INSERT INTO creator_settlements (id, owner_id, credits, created_at) VALUES ($1,$2,$3,$4)`, randomToken(18), u.ID, amount, now); err != nil {
		sendError(ctx, 500, "could not convert earnings")
		return
	}
	var free, plan int
	if err := tx.QueryRow(`UPDATE users SET paid_credits = paid_credits + $2 WHERE id = $1 RETURNING COALESCE(free_credits,0), COALESCE(plan_credits,0), paid_credits`, u.ID, amount).Scan(&free, &plan, &paid); err != nil {
		sendError(ctx, 500, "could not convert earnings")
		return
	}
	if _, err := tx.Exec(
		`INSERT INTO credit_ledger (id, user_id, delta, reason, app_id, source, balance_after, created_at)
		 VALUES ($1,$2,$3,'creator earnings converted to credits',$4,'earnings',$5,$6)`,
		randomToken(18), u.ID, amount, appID, free+plan+paid, now); err != nil {
		sendError(ctx, 500, "could not convert earnings")
		return
	}
	if err := tx.Commit(); err != nil {
		sendError(ctx, 500, "could not convert earnings")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "converted_credits": amount, "balance_after": free + plan + paid})
}

// minPriceCredits is the cheapest priced function in a deployment manifest, or 0.
func minPriceCredits(manifestJSON string) int {
	var m deploymentManifest
	if json.Unmarshal([]byte(manifestJSON), &m) != nil {
		return 0
	}
	best := 0
	for _, fn := range m.Functions {
		if fn.Billing == "caller" && fn.PriceCredits > 0 && (best == 0 || fn.PriceCredits < best) {
			best = fn.PriceCredits
		}
	}
	return best
}
