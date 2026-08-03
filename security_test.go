package main

import (
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

// The web tier's job is to decide who you are and what a millisecond costs.
// These are the ways that decision can be wrong in someone else's favour.

func testUploadStore() *uploadStore {
	return &uploadStore{
		bucket:    "appstatic",
		prefix:    "mojojojo/",
		publicURL: "https://appstatic.app.nz/mojojojo",
	}
}

func TestUploadKeysCannotEscapeTheUploadsPrefix(t *testing.T) {
	store := testUploadStore()
	// The bucket is shared with app.nz's own sites and with the compiled
	// kernel cache. Anything that lands outside uploads/ is a foothold.
	names := []string{
		"../cache/deadbeef.so",
		"../../app/static/index.html",
		"/etc/passwd",
		`..\..\windows\system32`,
		"....//....//cache//x.so",
		"a/b/c/../../../../../escape",
		"%2e%2e%2fcache%2fx",
		strings.Repeat("../", 40) + "root",
	}
	for _, name := range names {
		key, err := store.keyFor("user-1", name)
		if err != nil {
			continue // refusing outright is also correct
		}
		if !strings.HasPrefix(key, "mojojojo/uploads/user-1/") {
			t.Fatalf("%q produced %q, outside the user's directory", name, key)
		}
		if strings.Contains(key, "..") || strings.Contains(key, "/cache/") {
			t.Fatalf("%q produced %q", name, key)
		}
	}
}

func TestUploadKeysAreNamespacedPerUser(t *testing.T) {
	store := testUploadStore()
	a, err := store.keyFor("alice", "data.csv")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.keyFor("bob", "data.csv")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.Contains(a, "/alice/") || !strings.Contains(b, "/bob/") {
		t.Fatalf("keys are not per-user: %q %q", a, b)
	}
}

func TestAHostileUserIDCannotEscapeEither(t *testing.T) {
	store := testUploadStore()
	key, err := store.keyFor("../../../root", "x.txt")
	if err != nil {
		return
	}
	if !strings.HasPrefix(key, "mojojojo/uploads/") || strings.Contains(key, "..") {
		t.Fatalf("user id escaped: %q", key)
	}
}

func TestAnEmptyUserIsRefused(t *testing.T) {
	store := testUploadStore()
	if _, err := store.keyFor("", "x.txt"); err == nil {
		t.Fatal("accepted an upload with no owner")
	}
	if _, err := store.keyFor("///", "x.txt"); err == nil {
		t.Fatal("accepted an upload whose owner sanitises to nothing")
	}
}

func TestUploadNamesAreLengthCapped(t *testing.T) {
	store := testUploadStore()
	key, err := store.keyFor("u", strings.Repeat("a", 5000)+".bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(key) > 260 {
		t.Fatalf("key is %d bytes", len(key))
	}
}

func TestSanitizeDropsEverythingUnexpected(t *testing.T) {
	got := sanitizeSegment("a b/c\\d;e$f`g'h\"i\nj\x00k-l_m.n1", 100)
	if strings.ContainsAny(got, " /\\;$`'\"\n\x00") {
		t.Fatalf("sanitize left something dangerous in %q", got)
	}
	if got != "abcdefghijk-l_m.n1" {
		t.Fatalf("unexpected result %q", got)
	}
}

// --- credentials ---

func TestAPIKeysAreOnlyEverStoredHashed(t *testing.T) {
	b, u := testBilling(t, 10)
	key, err := b.auth.createKey(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	var stored, prefix string
	if err := b.auth.db.QueryRow(
		"SELECT key_hash, key_prefix FROM api_keys WHERE id = $1", key.ID).
		Scan(&stored, &prefix); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, key.Secret) || stored == key.Secret {
		t.Fatal("the raw key is in the database")
	}
	if stored != hashToken(key.Secret) {
		t.Fatal("stored value is not the expected hash")
	}
	if len(prefix) >= len(key.Secret) {
		t.Fatal("the stored prefix is long enough to be useful to a thief")
	}
}

func TestARevokedKeyStopsResolving(t *testing.T) {
	b, u := testBilling(t, 10)
	key, err := b.auth.createKey(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.auth.userFromAPIKey(key.Secret); got == nil {
		t.Fatal("a fresh key does not work")
	}
	if err := b.auth.revokeKey(u.ID, key.ID); err != nil {
		t.Fatal(err)
	}
	if got := b.auth.userFromAPIKey(key.Secret); got != nil {
		t.Fatal("a revoked key still resolves")
	}
}

func TestOneUserCannotRevokeAnothersKey(t *testing.T) {
	b, u := testBilling(t, 10)
	key, err := b.auth.createKey(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.auth.revokeKey("someone-else", key.ID); err != nil {
		t.Fatal(err)
	}
	if got := b.auth.userFromAPIKey(key.Secret); got == nil {
		t.Fatal("another user revoked this key")
	}
}

func TestKeysCarryTheirPrefixSoALeakIsGreppable(t *testing.T) {
	b, u := testBilling(t, 10)
	key, err := b.auth.createKey(u.ID, "x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.Secret, keyPrefix) {
		t.Fatalf("key %q has no recognisable prefix", key.Secret)
	}
	if len(key.Secret) < len(keyPrefix)+24 {
		t.Fatalf("key is too short to be unguessable: %d chars", len(key.Secret))
	}
}

// --- billing integrity ---

func TestABrokeUserCannotBeChargedIntoCredit(t *testing.T) {
	b, u := testBilling(t, 0)
	ch, err := b.charge(u, 100000, false, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Balance < 0 || ch.Credits < 0 {
		t.Fatalf("negative balance or charge: %+v", ch)
	}
}

func TestChargingIsSerialisedPerUser(t *testing.T) {
	b, u := testBilling(t, 1000)
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			_, err := b.charge(&User{ID: u.ID, FreeCredit: 1000}, 500, false, false, "race")
			done <- err
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// 20 × 500ms = 10,000ms at 1000ms/credit = exactly 10 credits, no more.
	var free int
	if err := b.auth.db.QueryRow(
		"SELECT free_credits FROM users WHERE id = $1", u.ID).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free != 990 {
		t.Fatalf("balance is %d after 20 concurrent charges, want 990", free)
	}
}

func TestTheWelcomeGrantHappensExactlyOnceUnderConcurrency(t *testing.T) {
	b, u := testBilling(t, 0)
	b.cfg.WelcomeCredits = 1000

	granted := make(chan int, 10)
	for i := 0; i < 10; i++ {
		go func() {
			amount, _ := b.grantWelcome(&User{ID: u.ID})
			granted <- amount
		}()
	}
	total := 0
	for i := 0; i < 10; i++ {
		total += <-granted
	}
	if total != 1000 {
		t.Fatalf("granted %d credits across concurrent first runs, want 1000", total)
	}

	var ledgerRows int
	if err := b.auth.db.QueryRow(
		"SELECT COUNT(*) FROM credit_ledger WHERE reason = 'mojojojo:welcome'").
		Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 1 {
		t.Fatalf("%d welcome ledger rows", ledgerRows)
	}
}

func TestFreeTierIsNotAWayToGetUnlimitedCompute(t *testing.T) {
	f := newFreeTier(1000)
	ip := "9.9.9.9"
	total := 0.0
	for i := 0; i < 50; i++ {
		if !f.allow(ip) {
			break
		}
		f.spend(ip, 100)
		total += 100
	}
	if total > 1100 {
		t.Fatalf("free tier handed out %.0fms against a 1000ms budget", total)
	}
}

func TestClientIPIgnoresHeadersACallerControls(t *testing.T) {
	// The free tier is keyed on this. nginx overwrites X-Real-IP on every
	// request; X-Forwarded-For is appended to, so a caller can put anything in
	// it — and rotating it would mint a fresh free budget per request.
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("X-Forwarded-For", "1.2.3.4")
	ctx.Request.Header.Set("X-Real-IP", "5.6.7.8")
	if got := clientIP(ctx); got != "5.6.7.8" {
		t.Fatalf("clientIP = %q, want the proxy-set address", got)
	}

	spoofed := &fasthttp.RequestCtx{}
	spoofed.Request.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := clientIP(spoofed); got == "9.9.9.9" {
		t.Fatal("a caller-supplied X-Forwarded-For was trusted")
	}
}
