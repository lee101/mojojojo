package main

import (
	"log"
	"time"
)

// grantWelcome credits a mojojojo account once, atomically in mojojojo's own
// Postgres database. Concurrent first requests cannot double-grant it.
func (b *billing) grantWelcome(u *User) (int, bool) {
	if b.cfg.WelcomeCredits <= 0 || u == nil {
		return 0, false
	}
	lock := b.lockFor(u.ID)
	lock.Lock()
	defer lock.Unlock()

	tx, err := b.db.Begin()
	if err != nil {
		return 0, false
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO grants (user_id, credits, granted_at)
		VALUES ($1, $2, $3) ON CONFLICT (user_id) DO NOTHING`,
		u.ID, b.cfg.WelcomeCredits, time.Now().UTC())
	if err != nil {
		return 0, false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false
	}
	var free, plan, paid int
	if err := tx.QueryRow(`SELECT free_credits, plan_credits, paid_credits
		FROM users WHERE id = $1 FOR UPDATE`, u.ID).Scan(&free, &plan, &paid); err != nil {
		return 0, false
	}
	free += b.cfg.WelcomeCredits
	if _, err := tx.Exec(`UPDATE users SET free_credits = $1 WHERE id = $2`, free, u.ID); err != nil {
		return 0, false
	}
	if _, err := tx.Exec(`INSERT INTO credit_ledger
		(id, user_id, delta, reason, app_id, source, balance_after, created_at)
		VALUES ($1, $2, $3, $4, $5, 'grant', $6, $7)`, randomToken(18), u.ID,
		b.cfg.WelcomeCredits, "mojojojo:welcome", appID, free+plan+paid, time.Now().UTC()); err != nil {
		return 0, false
	}
	if err := tx.Commit(); err != nil {
		return 0, false
	}
	u.FreeCredit += b.cfg.WelcomeCredits
	log.Printf("welcome: granted %d credits to %s", b.cfg.WelcomeCredits, u.ID)
	return b.cfg.WelcomeCredits, true
}

func (b *billing) granted(userID string) bool {
	var n int
	return b.db.QueryRow(`SELECT credits FROM grants WHERE user_id = $1`, userID).Scan(&n) == nil
}
