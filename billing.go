package main

import (
	"database/sql"
	"fmt"
	"math"
	"sync"
	"time"
)

// Billing is per millisecond, but the mojojojo ledger is denominated in whole
// credits — so charging a 40ms run by rounding up to 1 credit would be a 25x
// overcharge and charging it down would be free. Instead every user carries a
// millisecond balance in mojojojo's own database, runs accrue into it, and a
// whole credit moves through the local ledger each time it crosses the price.
// Nothing is lost to rounding in either direction, and the ledger line the
// user sees here says how many milliseconds it paid for.
type billing struct {
	db   *sql.DB
	auth *authStore
	cfg  Config

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-user, so two runs cannot race the accrual
}

type charge struct {
	BilledMs   float64 `json:"billed_ms"`
	Credits    int     `json:"credits"`
	OwedMs     float64 `json:"owed_ms"`
	Balance    int     `json:"balance"`
	MsPerCredi float64 `json:"ms_per_credit"`
}

func newBilling(db *sql.DB, auth *authStore, cfg Config) (*billing, error) {
	if db == nil || auth == nil {
		return nil, fmt.Errorf("billing requires the mojojojo database and auth store")
	}
	return &billing{db: db, auth: auth, cfg: cfg, locks: map[string]*sync.Mutex{}}, nil
}

func (b *billing) lockFor(userID string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.locks[userID]
	if !ok {
		m = &sync.Mutex{}
		b.locks[userID] = m
	}
	return m
}

// price returns the millisecond price of a run of this shape.
func (b *billing) msPerCredit(gpu bool) float64 {
	if gpu {
		return b.cfg.MsPerCreditGPU
	}
	return b.cfg.MsPerCredit
}

// charge accrues `ms` against the user and moves whole credits out of the
// local ledger as they are earned. The run has already happened by the time
// this is called: refusing to bill would be giving compute away, so a user who
// cannot pay is charged what they have and blocked on the *next* call.
func (b *billing) charge(u *User, ms float64, gpu, native bool, source string) (charge, error) {
	lock := b.lockFor(u.ID)
	lock.Lock()
	defer lock.Unlock()

	perCredit := b.msPerCredit(gpu)
	if ms < 0 {
		ms = 0
	}

	var owed float64
	err := b.db.QueryRow("SELECT owed_ms FROM accrual WHERE user_id = $1", u.ID).Scan(&owed)
	if err != nil && err != sql.ErrNoRows {
		return charge{}, err
	}
	owed += ms
	credits := int(math.Floor(owed / perCredit))
	if credits > 0 {
		owed -= float64(credits) * perCredit
	}

	balance := u.Credits()
	if credits > 0 {
		kind := "cpu"
		if gpu {
			kind = "gpu"
		}
		spent, newBalance, err := b.spendReason(u.ID, credits,
			fmt.Sprintf("exec:%s:%.0fms", kind, ms))
		if err != nil {
			return charge{}, err
		}
		// A partial spend (the user ran out mid-accrual) leaves the rest owed;
		// it is collected next time they top up, and blocks runs until then.
		if spent < credits {
			owed += float64(credits-spent) * perCredit
		}
		credits = spent
		balance = newBalance
	}

	now := time.Now().UTC()
	if _, err := b.db.Exec(`
		INSERT INTO accrual (user_id, owed_ms, total_ms, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT(user_id) DO UPDATE SET owed_ms = EXCLUDED.owed_ms,
			total_ms = accrual.total_ms + EXCLUDED.total_ms, updated_at = EXCLUDED.updated_at`,
		u.ID, owed, ms, now); err != nil {
		return charge{}, err
	}
	if _, err := b.db.Exec(
		`INSERT INTO runs (id, user_id, created_at, ms, gpu, ok, credits, native, source)
		 VALUES ($1, $2, $3, $4, $5, TRUE, $6, $7, $8)`,
		randomToken(12), u.ID, now, ms, gpu, credits, native, source); err != nil {
		return charge{}, err
	}

	return charge{BilledMs: round3(ms), Credits: credits, OwedMs: round3(owed),
		Balance: balance, MsPerCredi: perCredit}, nil
}

// spend moves credits out of mojojojo's own wallet, free -> plan -> paid.
func (b *billing) spend(userID string, credits int, ms float64, gpu bool) (int, int, error) {
	kind := "cpu"
	if gpu {
		kind = "gpu"
	}
	return b.spendReason(userID, credits, fmt.Sprintf("exec:%s:%.0fms", kind, ms))
}

func (b *billing) spendReason(userID string, credits int, reason string) (int, int, error) {
	tx, err := b.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	var free, plan, paid int
	var planExpire sql.NullTime
	if err := tx.QueryRow(
		`SELECT COALESCE(free_credits,0), COALESCE(plan_credits,0), plan_credits_expire_at,
		        COALESCE(paid_credits,0) FROM users WHERE id = $1 FOR UPDATE`, userID).
		Scan(&free, &plan, &planExpire, &paid); err != nil {
		return 0, 0, err
	}
	if planExpire.Valid && planExpire.Time.Before(time.Now()) {
		plan = 0
	}
	available := free + plan + paid
	take := credits
	if take > available {
		take = available
	}
	if take <= 0 {
		return 0, available, nil
	}

	remaining := take
	useFree := min(remaining, free)
	remaining -= useFree
	usePlan := min(remaining, plan)
	remaining -= usePlan
	usePaid := remaining
	free -= useFree
	plan -= usePlan
	paid -= usePaid
	after := free + plan + paid

	if _, err := tx.Exec(
		"UPDATE users SET free_credits = $1, plan_credits = $2, paid_credits = $3 WHERE id = $4",
		free, plan, paid, userID); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(
		`INSERT INTO credit_ledger (id, user_id, delta, reason, app_id, source, balance_after, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		randomToken(18), userID, -take, reason, appID, "spend", after, time.Now().UTC()); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return take, after, nil
}

// userFunded is User.Funded read from the database, for work that runs later
// than the request that asked for it.
func userFunded(q interface {
	QueryRow(string, ...any) *sql.Row
}, userID string) bool {
	var funded bool
	return q.QueryRow(`SELECT paid_credits + CASE WHEN plan_credits_expire_at IS NULL
		OR plan_credits_expire_at > $2 THEN plan_credits ELSE 0 END > 0
		FROM users WHERE id = $1 AND disabled_at IS NULL`, userID, time.Now().UTC()).
		Scan(&funded) == nil && funded
}

// owed reports the user's un-charged millisecond balance.
func (b *billing) owed(userID string) float64 {
	var owed float64
	_ = b.db.QueryRow("SELECT owed_ms FROM accrual WHERE user_id = $1", userID).Scan(&owed)
	return owed
}

type usageRow struct {
	At      time.Time `json:"at"`
	Ms      float64   `json:"ms"`
	Credits int       `json:"credits"`
	GPU     bool      `json:"gpu"`
	Native  bool      `json:"native"`
	Source  string    `json:"source"`
}

func (b *billing) recent(userID string, limit int) ([]usageRow, error) {
	rows, err := b.db.Query(
		`SELECT created_at, ms, credits, gpu, native, source FROM runs
		 WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usageRow
	for rows.Next() {
		var r usageRow
		if err := rows.Scan(&r.At, &r.Ms, &r.Credits, &r.GPU, &r.Native, &r.Source); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// totals reports lifetime milliseconds executed by this user.
func (b *billing) totalMs(userID string) float64 {
	var total float64
	_ = b.db.QueryRow("SELECT total_ms FROM accrual WHERE user_id = $1", userID).Scan(&total)
	return total
}

// creditsSince reports credits actually spent since `t`, annualised to an
// hour. This is the autoscaler's revenue signal, so it counts money that left
// the ledger and nothing else — no free tier, no welcome grants, no estimates.
func (b *billing) creditsSince(t time.Time) float64 {
	var credits int
	if err := b.db.QueryRow(
		"SELECT COALESCE(SUM(credits),0) FROM runs WHERE created_at >= $1", t.UTC()).
		Scan(&credits); err != nil {
		return 0
	}
	elapsed := time.Since(t).Hours()
	if elapsed <= 0 {
		return 0
	}
	return float64(credits) / elapsed
}

// lastRunAt is when anything last ran, for the idle check.
func (b *billing) lastRunAt() time.Time {
	var at sql.NullTime
	if err := b.db.QueryRow("SELECT MAX(created_at) FROM runs").Scan(&at); err != nil {
		return time.Time{}
	}
	if !at.Valid {
		return time.Time{}
	}
	return at.Time
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
