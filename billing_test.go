package main

import (
	"testing"
	"time"
)

func testBilling(t *testing.T, freeCredits int) (*billing, *User) {
	t.Helper()
	db := testPostgres(t)
	if _, err := db.Exec(
		`INSERT INTO users (id, email, password_hash, created_at, free_credits) VALUES ('u1','a@b.c','x',$1,$2)`,
		time.Now().UTC(), freeCredits); err != nil {
		t.Fatal(err)
	}
	cfg := Config{MsPerCredit: 1000, MsPerCreditGPU: 100}
	auth := newAuthStore(db)
	b, err := newBilling(db, auth, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b, &User{ID: "u1", FreeCredit: freeCredits}
}

// The headline claim: sub-credit runs accrue instead of rounding up, and the
// credit only moves once the milliseconds have actually added up to one.
func TestShortRunsAccrueWithoutCharging(t *testing.T) {
	b, u := testBilling(t, 10)

	for i := 0; i < 9; i++ {
		ch, err := b.charge(u, 100, false, false, "test")
		if err != nil {
			t.Fatal(err)
		}
		if ch.Credits != 0 {
			t.Fatalf("run %d: charged %d credits for 100ms, want 0", i, ch.Credits)
		}
	}
	if owed := b.owed("u1"); owed != 900 {
		t.Fatalf("owed = %v ms, want 900", owed)
	}

	ch, err := b.charge(u, 100, false, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Credits != 1 {
		t.Fatalf("charged %d credits at the 1000ms mark, want 1", ch.Credits)
	}
	if ch.OwedMs != 0 {
		t.Fatalf("owed %v ms after the charge, want 0", ch.OwedMs)
	}
	if ch.Balance != 9 {
		t.Fatalf("balance %d, want 9", ch.Balance)
	}
}

func TestLongRunChargesManyCreditsAndRemainderCarries(t *testing.T) {
	b, u := testBilling(t, 100)

	ch, err := b.charge(u, 3500, false, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Credits != 3 {
		t.Fatalf("3500ms charged %d credits, want 3", ch.Credits)
	}
	if ch.OwedMs != 500 {
		t.Fatalf("carried %v ms, want 500", ch.OwedMs)
	}
}

func TestGPUCostsMore(t *testing.T) {
	b, u := testBilling(t, 100)

	ch, err := b.charge(u, 1000, true, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Credits != 10 {
		t.Fatalf("1000ms of GPU charged %d credits, want 10 at 100ms/credit", ch.Credits)
	}
}

// Running out mid-accrual must not silently forgive the debt: the unpaid
// milliseconds stay owed so the next top-up collects them.
func TestSpendingBeyondBalanceKeepsTheDebt(t *testing.T) {
	b, u := testBilling(t, 2)

	ch, err := b.charge(u, 5000, false, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Credits != 2 {
		t.Fatalf("charged %d credits with a balance of 2, want 2", ch.Credits)
	}
	if ch.Balance != 0 {
		t.Fatalf("balance %d, want 0", ch.Balance)
	}
	if ch.OwedMs != 3000 {
		t.Fatalf("owed %v ms after running out, want 3000 (3 unpaid credits)", ch.OwedMs)
	}
}

func TestLedgerLineIsTraceable(t *testing.T) {
	b, u := testBilling(t, 10)
	if _, err := b.charge(u, 2500, false, false, "test"); err != nil {
		t.Fatal(err)
	}

	var reason, app string
	var delta, after int
	if err := b.auth.db.QueryRow(
		`SELECT delta, reason, app_id, balance_after FROM credit_ledger`).
		Scan(&delta, &reason, &app, &after); err != nil {
		t.Fatal(err)
	}
	if delta != -2 || app != appID || after != 8 {
		t.Fatalf("ledger row delta=%d app=%s after=%d", delta, app, after)
	}
	if reason != "exec:cpu:2500ms" {
		t.Fatalf("reason %q does not say what was bought", reason)
	}
}

// The credits page is the user's answer to "where did my money go", so the
// ledger it reads has to show every app's lines — one balance, many spenders —
// while the totals it headlines count only what mojojojo did.
func TestLedgerShowsEveryAppButTotalsOnlyOurs(t *testing.T) {
	b, u := testBilling(t, 10)
	if _, err := b.charge(u, 2500, false, false, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.auth.db.Exec(
		`INSERT INTO credit_ledger (id, user_id, delta, reason, app_id, source, balance_after, created_at)
		 VALUES ('x','u1',-5,'admin:adjustment','admin','adjustment',3,$1)`,
		time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	b.cfg.WelcomeCredits = 1000
	if amount, ok := b.grantWelcome(u); !ok || amount != 1000 {
		t.Fatal("welcome grant failed")
	}

	entries, err := b.auth.ledger("u1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("ledger has %d entries, want 3 (ours, another app's, the grant)", len(entries))
	}
	if entries[0].At.Before(entries[len(entries)-1].At) {
		t.Fatal("ledger is not newest-first")
	}
	var sawOther bool
	for _, e := range entries {
		if e.App == "admin" {
			sawOther = true
		}
	}
	if !sawOther {
		t.Fatal("another app's spend is missing from the balance history")
	}

	spent, granted := b.auth.ledgerTotals("u1")
	if spent != 2 {
		t.Fatalf("spent on mojojojo = %d credits, want 2 (not the other app's 5)", spent)
	}
	if granted != 1000 {
		t.Fatalf("granted by mojojojo = %d, want 1000", granted)
	}
}

func TestLedgerOfAQuietAccountIsEmptyNotAnError(t *testing.T) {
	b, _ := testBilling(t, 10)
	entries, err := b.auth.ledger("u1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d entries for an account that has done nothing", len(entries))
	}
	if spent, granted := b.auth.ledgerTotals("u1"); spent != 0 || granted != 0 {
		t.Fatalf("totals %d/%d for an account that has done nothing", spent, granted)
	}
}

func TestFreeTierIsPerDayAndPerAddress(t *testing.T) {
	f := newFreeTier(1000)
	if !f.allow("1.2.3.4") {
		t.Fatal("first request should be allowed")
	}
	f.spend("1.2.3.4", 1200)
	if f.allow("1.2.3.4") {
		t.Fatal("address over budget should be refused")
	}
	if !f.allow("5.6.7.8") {
		t.Fatal("a different address has its own budget")
	}
}
