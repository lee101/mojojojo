package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Placement decides where somebody's money goes, so the rules are pinned here
// rather than left to whichever tier answered first.

func planServer(t *testing.T) *Server {
	t.Helper()
	db := testPostgres(t)
	if err := createPlanTables(db); err != nil {
		t.Fatalf("tables: %v", err)
	}
	cfg := loadConfig()
	cfg.MsPerCredit, cfg.MsPerCreditGPU = 10000, 2000
	return &Server{cfg: cfg, db: db, runner: newRunnerClient("http://127.0.0.1:1")}
}

func TestProgramHashSeparatesLanguagesAndDevices(t *testing.T) {
	code := "print(1)"
	if programHash(code, "python", false) == programHash(code, "mojo", false) {
		t.Fatal("the same text in another language is another program")
	}
	if programHash(code, "python", false) == programHash(code, "python", true) {
		t.Fatal("a GPU run of the same program costs something else entirely")
	}
	if programHash(code, "python", false) != programHash(code, "python", false) {
		t.Fatal("hashing is not stable")
	}
}

func TestAnEstimateIsWhatTheProgramDidLastTime(t *testing.T) {
	s := planServer(t)
	req := RunRequest{Code: "print(1)", Language: "python"}
	result := &RunResult{BillableMs: 42}

	if s.recall(programHash(req.Code, "python", false)) != nil {
		t.Fatal("a program nobody has run has no estimate")
	}
	s.remember(req.Code, req, result)
	got := s.recall(programHash(req.Code, "python", false))
	if got == nil || got.Ms != 42 || got.Runs != 1 {
		t.Fatalf("estimate not remembered: %+v", got)
	}

	// The second run is the one that matters: it is what a third caller is
	// quoted, and the count is what says how much to trust it.
	result2 := &RunResult{BillableMs: 8}
	result2.Accel.NativeCalls = 6
	s.remember(req.Code, req, result2)
	got = s.recall(programHash(req.Code, "python", false))
	if got.Ms != 8 || got.Runs != 2 || !got.Native {
		t.Fatalf("second observation not recorded: %+v", got)
	}
}

func TestARunThatCostNothingTeachesNothing(t *testing.T) {
	s := planServer(t)
	req := RunRequest{Code: "print(1)"}
	s.remember(req.Code, req, &RunResult{BillableMs: 0})
	if s.recall(programHash(req.Code, "python", false)) != nil {
		t.Fatal("a run with no measured cost must not become an estimate")
	}
}

func TestWhatTheEdgeCanTake(t *testing.T) {
	cases := []struct {
		name string
		req  RunRequest
		want bool
	}{
		{"stdlib only", RunRequest{Code: "import math\nprint(math.pi)"}, true},
		{"no imports", RunRequest{Code: "print(sum(range(10)))"}, true},
		{"numpy", RunRequest{Code: "import numpy as np"}, false},
		{"a package was asked for", RunRequest{Code: "print(1)", Packages: []string{"pandas"}}, false},
		{"a GPU was asked for", RunRequest{Code: "print(1)", GPU: true}, false},
		{"mojo", RunRequest{Code: "def main(): pass", Language: "mojo"}, false},
		{"torch", RunRequest{Code: "from torch import nn"}, false},
	}
	for _, tc := range cases {
		if got := edgeCapable(tc.req); got != tc.want {
			t.Errorf("%s: edgeCapable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTheEdgeIsNotOfferedWorkItWouldLose(t *testing.T) {
	s := planServer(t)
	s.cfg.EdgeURL = "https://edge.example"
	req := RunRequest{Code: "print(sum(range(10)))"}

	if !s.edgeSuitable(req) {
		t.Fatal("a small unseen program is exactly what the edge is for")
	}

	// A program we have watched take a second belongs on the box we own.
	s.remember(req.Code, req, &RunResult{BillableMs: 1200})
	if s.edgeSuitable(req) {
		t.Fatal("a long program should not be sent to rented CPU-milliseconds")
	}
}

func TestAProgramWithACompiledKernelStaysWhereTheKernelIs(t *testing.T) {
	s := planServer(t)
	s.cfg.EdgeURL = "https://edge.example"
	req := RunRequest{Code: "def k(n: int) -> int:\n    return n\nprint(k(1))"}
	result := &RunResult{BillableMs: 30}
	result.Accel.NativeCalls = 8
	s.remember(req.Code, req, result)

	if s.edgeSuitable(req) {
		t.Fatal("the edge has no compiler; sending a native program there is a downgrade")
	}
	if got := s.suggest(req, PlanResponse{
		Fleet:    fleetView{Edge: tierView{Up: true}},
		Estimate: s.recall(programHash(req.Code, "python", false)),
	}); got.Where != "home" {
		t.Fatalf("suggested %s (%s), want home", got.Where, got.Why)
	}
}

func TestWithNoEdgeConfiguredNothingIsRoutedToOne(t *testing.T) {
	s := planServer(t)
	s.cfg.EdgeURL = ""
	if s.edgeSuitable(RunRequest{Code: "print(1)"}) {
		t.Fatal("there is no edge to route to")
	}
	if s.fleet().Edge.Up {
		t.Fatal("the fleet must not claim an edge that does not exist")
	}
}

func TestTheSuggestionQuotesAPrice(t *testing.T) {
	s := planServer(t)
	req := RunRequest{Code: "import torch\nprint(1)"}
	s.remember(req.Code, req, &RunResult{BillableMs: 50000})
	plan := PlanResponse{Estimate: s.recall(programHash(req.Code, "python", false))}

	got := s.suggest(req, plan)
	if got.Where != "home" {
		t.Fatalf("torch belongs on our box, got %s", got.Where)
	}
	if got.Credits != 5 || got.PriceUSD <= 0 {
		t.Fatalf("50s at 10s/credit is 5 credits: %+v", got)
	}
}

func TestAGpuRunIsQuotedAtTheGpuRate(t *testing.T) {
	s := planServer(t)
	req := RunRequest{Code: "import torch", GPU: true}
	s.remember(req.Code, req, &RunResult{BillableMs: 20000})
	plan := PlanResponse{Estimate: s.recall(programHash(req.Code, "python", true))}

	got := s.suggest(req, plan)
	if got.Credits != 10 {
		t.Fatalf("20s at 2s/credit is 10 credits, got %d", got.Credits)
	}
}

func TestOutOfMemoryIsRecognisedWhereverItComesFrom(t *testing.T) {
	for _, text := range []string{
		"torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 20.00 GiB",
		"RuntimeError: CUDA error: out of memory",
		"cudaErrorMemoryAllocation",
		"failed to allocate 8589934592 bytes on device",
	} {
		if !outOfMemory(&RunResult{OK: false, Stderr: text}) {
			t.Errorf("not recognised as a machine problem: %q", text)
		}
	}
	// A program that simply failed is the caller's, and moving it would run it
	// twice and bill for both.
	if outOfMemory(&RunResult{OK: false, Stderr: "ValueError: boom"}) {
		t.Error("an ordinary exception is not a reason to move a run")
	}
	if outOfMemory(&RunResult{OK: true, Stderr: "out of memory"}) {
		t.Error("a run that succeeded is not retried, whatever it printed")
	}
	if outOfMemory(nil) {
		t.Error("nil is not an out-of-memory")
	}
}

func TestImportScanMatchesTheBrowsersView(t *testing.T) {
	// This list is mirrored in web/route.js; the two deciding differently is
	// how a program ends up bounced between tiers.
	got := importedModules("import a\nfrom b.c import d\n    import e\n#import f\n")
	want := map[string]bool{"a": true, "b": true, "e": true}
	if len(got) != len(want) {
		t.Fatalf("scanned %v, want %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("scanned %v, want %v", got, want)
		}
	}
}

// --- edge tickets: money, so the rules are pinned ------------------------

func ticketServer(t *testing.T) *Server {
	t.Helper()
	s := planServer(t)
	if err := createEdgeTables(s.db); err != nil {
		t.Fatalf("edge tables: %v", err)
	}
	s.cfg.EdgeURL = "https://edge.example"
	s.cfg.EdgeToken = "shared-secret"
	if _, err := s.db.Exec(`INSERT INTO users (id, email, password_hash, created_at, paid_credits)
		VALUES ('user-1', 'edge@example.com', 'x', $1, 1000)`, time.Now().UTC()); err != nil {
		t.Fatalf("edge user: %v", err)
	}
	return s
}

func TestATicketVerifiesAndCarriesItsLimits(t *testing.T) {
	s := ticketServer(t)
	raw, maxMs, ok := s.mintTicket("user-1")
	if !ok || raw == "" {
		t.Fatal("no ticket issued")
	}
	if maxMs != ticketMaxMs {
		t.Fatalf("ticket budget %d, want %d", maxMs, ticketMaxMs)
	}
	ticket, err := s.openTicket(raw)
	if err != nil {
		t.Fatalf("our own ticket does not verify: %v", err)
	}
	if ticket.User != "user-1" || ticket.MaxMs != ticketMaxMs {
		t.Fatalf("ticket says %+v", ticket)
	}
}

func TestATamperedTicketDoesNotVerify(t *testing.T) {
	s := ticketServer(t)
	raw, _, _ := s.mintTicket("user-1")

	// The signature is over the payload, so editing either half breaks it —
	// which is the whole reason a browser can be trusted to carry one.
	parts := strings.SplitN(raw, ".", 2)
	forged := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"u":"user-2","x":99999999999,"m":30000,"n":"stolen"}`))
	if _, err := s.openTicket(forged + "." + parts[1]); err == nil {
		t.Fatal("a rewritten ticket verified")
	}
	if _, err := s.openTicket(parts[0] + ".00"); err == nil {
		t.Fatal("a rewritten signature verified")
	}
	if _, err := s.openTicket("nonsense"); err == nil {
		t.Fatal("a malformed ticket verified")
	}
}

func TestAnExpiredTicketIsRefused(t *testing.T) {
	s := ticketServer(t)
	payload, _ := json.Marshal(edgeTicket{
		User: "user-1", Expires: time.Now().Add(-time.Minute).Unix(),
		MaxMs: 1000, Nonce: "old",
	})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	if _, err := s.openTicket(encoded + "." + sign(s.cfg.EdgeToken, encoded)); err == nil {
		t.Fatal("an expired ticket verified")
	}
}

func TestOnlyTheEdgeCanSignAReceipt(t *testing.T) {
	// The caller reports their own bill, so the receipt is what makes the
	// number unforgeable: it is an HMAC over the nonce and the milliseconds.
	real := receiptFor("shared-secret", "nonce-1", 12.5)
	if receiptFor("shared-secret", "nonce-1", 12.4) == real {
		t.Fatal("a receipt must cover the milliseconds it is for")
	}
	if receiptFor("shared-secret", "nonce-2", 12.5) == real {
		t.Fatal("a receipt must cover the ticket it is for")
	}
	if receiptFor("another-secret", "nonce-1", 12.5) == real {
		t.Fatal("a receipt must be unforgeable without the secret")
	}
}

func TestOutstandingTicketsAreCapped(t *testing.T) {
	// An unsettled ticket is a run we may never be paid for. Bounded on
	// purpose: the cap times max_ms is the most an account can walk away with.
	s := ticketServer(t)
	for i := 0; i < maxOutstanding; i++ {
		if _, _, ok := s.mintTicket("user-1"); !ok {
			t.Fatalf("ticket %d refused early", i)
		}
	}
	if _, _, ok := s.mintTicket("user-1"); ok {
		t.Fatal("issued more tickets than the cap allows")
	}
	// Another account is unaffected: the cap is per person, not global.
	if _, err := s.db.Exec(`INSERT INTO users (id, email, password_hash, created_at, paid_credits)
		VALUES ('user-2', 'edge2@example.com', 'x', $1, 1000)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.mintTicket("user-2"); !ok {
		t.Fatal("one account's tickets blocked another's")
	}
}

func TestNoEdgeMeansNoTickets(t *testing.T) {
	s := ticketServer(t)
	s.cfg.EdgeToken = ""
	if _, _, ok := s.mintTicket("user-1"); ok {
		t.Fatal("issued a ticket for an edge we cannot verify receipts from")
	}
}
