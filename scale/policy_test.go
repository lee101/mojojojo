package scale

import (
	"strings"
	"testing"
	"time"
)

func cpuOffer() Offer {
	return Offer{Provider: "hetzner", Type: "cpx31", USDHour: 0.024, Kind: CPU}
}

// A deep, sustained queue with paying users behind it is the only thing that
// buys a machine.
func TestRentsWhenPaidDemandJustifiesIt(t *testing.T) {
	s := Snapshot{
		Kind: CPU, Queued: 8, QueuedFor: 2 * time.Minute,
		PaidCreditsPerHour: 200, // $0.20/h against a $0.024/h box
	}
	got := Decide(s, DefaultLimits(CPU), cpuOffer(), time.Hour)
	if got.Scale != 1 {
		t.Fatalf("did not rent: %s", got.Reason)
	}
	if !strings.Contains(got.Reason, "queued") {
		t.Fatalf("reason should say what it saw: %q", got.Reason)
	}
}

func TestDoesNotRentForAShallowQueue(t *testing.T) {
	s := Snapshot{Queued: 2, QueuedFor: 5 * time.Minute, PaidCreditsPerHour: 5000}
	if got := Decide(s, DefaultLimits(CPU), cpuOffer(), time.Hour); got.Scale != 0 {
		t.Fatalf("rented for a queue of 2: %s", got.Reason)
	}
}

func TestDoesNotRentForABurst(t *testing.T) {
	s := Snapshot{Queued: 40, QueuedFor: 3 * time.Second, PaidCreditsPerHour: 5000}
	got := Decide(s, DefaultLimits(CPU), cpuOffer(), time.Hour)
	if got.Scale != 0 {
		t.Fatal("rented a machine for a three-second spike")
	}
	if !strings.Contains(got.Reason, "burst") {
		t.Fatalf("reason %q", got.Reason)
	}
}

// The rule that matters most: no revenue, no machine, however long the queue.
func TestDoesNotRentForFreeLoad(t *testing.T) {
	s := Snapshot{Queued: 500, QueuedFor: time.Hour, PaidCreditsPerHour: 0}
	got := Decide(s, DefaultLimits(CPU), cpuOffer(), time.Hour)
	if got.Scale != 0 {
		t.Fatal("rented capacity for demand nobody is paying for")
	}
	if !strings.Contains(got.Reason, "paid demand") {
		t.Fatalf("reason %q", got.Reason)
	}
}

func TestRequiresMarginNotBreakEven(t *testing.T) {
	limits := DefaultLimits(CPU)
	offer := cpuOffer()
	// Revenue exactly equal to cost: covered, but not by the margin.
	s := Snapshot{Queued: 8, QueuedFor: time.Minute,
		PaidCreditsPerHour: offer.USDHour * 1000}
	if got := Decide(s, limits, offer, time.Hour); got.Scale != 0 {
		t.Fatalf("rented at break-even: %s", got.Reason)
	}
	s.PaidCreditsPerHour = offer.USDHour * 1000 * limits.MarginMultiple
	if got := Decide(s, limits, offer, time.Hour); got.Scale != 1 {
		t.Fatalf("did not rent at the margin: %s", got.Reason)
	}
}

func TestRespectsTheDailyBudget(t *testing.T) {
	limits := DefaultLimits(CPU)
	s := Snapshot{
		Queued: 20, QueuedFor: time.Hour, PaidCreditsPerHour: 100000,
		SpentTodayUSD: limits.DailyBudgetUSD,
	}
	got := Decide(s, limits, cpuOffer(), time.Hour)
	if got.Scale != 0 {
		t.Fatal("spent past the daily budget")
	}
	if !strings.Contains(got.Reason, "budget") {
		t.Fatalf("reason %q", got.Reason)
	}
}

func TestRespectsTheWorkerCap(t *testing.T) {
	limits := DefaultLimits(CPU)
	s := Snapshot{Queued: 50, QueuedFor: time.Hour, PaidCreditsPerHour: 100000,
		Rented: limits.MaxWorkers}
	if got := Decide(s, limits, cpuOffer(), time.Hour); got.Scale != 0 {
		t.Fatalf("passed the worker cap: %s", got.Reason)
	}
}

func TestReturnsIdleWorkers(t *testing.T) {
	s := Snapshot{Rented: 2, Queued: 0, Idle: 10 * time.Minute}
	got := Decide(s, DefaultLimits(CPU), cpuOffer(), time.Hour)
	if got.Scale != -1 {
		t.Fatalf("kept paying for an idle machine: %s", got.Reason)
	}
}

func TestKeepsWorkersThatAreStillBusy(t *testing.T) {
	s := Snapshot{Rented: 2, Queued: 3, Idle: time.Hour}
	if got := Decide(s, DefaultLimits(CPU), cpuOffer(), time.Hour); got.Scale == -1 {
		t.Fatal("returned a worker while runs were queued")
	}
}

// Returning capacity must never be blocked by the conditions for acquiring it.
func TestReturnsIdleWorkersEvenWithNoRevenue(t *testing.T) {
	s := Snapshot{Rented: 1, Queued: 0, Idle: time.Hour, PaidCreditsPerHour: 0}
	if got := Decide(s, DefaultLimits(CPU), cpuOffer(), time.Hour); got.Scale != -1 {
		t.Fatalf("stranded a rented machine: %s", got.Reason)
	}
}

func TestCooldownStopsOscillation(t *testing.T) {
	s := Snapshot{Queued: 50, QueuedFor: time.Hour, PaidCreditsPerHour: 100000}
	got := Decide(s, DefaultLimits(CPU), cpuOffer(), 5*time.Second)
	if got.Scale != 0 || !strings.Contains(got.Reason, "cooling down") {
		t.Fatalf("acted during the cooldown: %+v", got)
	}
}

func TestGPUIsHeldToAHigherBar(t *testing.T) {
	gpu := Offer{Provider: "runpod", Type: "RTX 4090", USDHour: 0.44, Kind: GPU}
	// Demand that would comfortably buy a CPU box.
	s := Snapshot{Kind: GPU, Queued: 3, QueuedFor: 50 * time.Second,
		PaidCreditsPerHour: 300}
	if got := Decide(s, DefaultLimits(GPU), gpu, time.Hour); got.Scale != 0 {
		t.Fatalf("rented a GPU on CPU-grade evidence: %s", got.Reason)
	}
}

func TestEveryDecisionExplainsItself(t *testing.T) {
	cases := []Snapshot{
		{}, {Queued: 100, QueuedFor: time.Hour},
		{Rented: 1, Idle: time.Hour},
		{Queued: 8, QueuedFor: time.Minute, PaidCreditsPerHour: 1e6},
	}
	for i, s := range cases {
		got := Decide(s, DefaultLimits(CPU), cpuOffer(), time.Hour)
		if strings.TrimSpace(got.Reason) == "" {
			t.Fatalf("case %d gave no reason", i)
		}
	}
}

func TestNoOfferMeansNoAction(t *testing.T) {
	s := Snapshot{Queued: 20, QueuedFor: time.Hour, PaidCreditsPerHour: 1e6}
	if got := Decide(s, DefaultLimits(CPU), Offer{}, time.Hour); got.Scale != 0 {
		t.Fatal("acted without an offer")
	}
}

func TestCheapestPicksByKindAndPrice(t *testing.T) {
	offers := []Offer{
		{Type: "big-gpu", USDHour: 2.0, Kind: GPU},
		{Type: "small-gpu", USDHour: 0.4, Kind: GPU},
		{Type: "cpu", USDHour: 0.01, Kind: CPU},
	}
	if got := Cheapest(offers, GPU); got.Type != "small-gpu" {
		t.Fatalf("picked %q", got.Type)
	}
	if got := Cheapest(offers, CPU); got.Type != "cpu" {
		t.Fatalf("picked %q", got.Type)
	}
	if got := Cheapest(nil, CPU); got.USDHour != 0 {
		t.Fatal("invented an offer")
	}
}

// --- providers ---

func TestDryRunNeverSpends(t *testing.T) {
	inner := &Hetzner{Token: "would-work"}
	d := DryRun{Inner: inner}
	id, err := d.Launch(cpuOffer(), "#!/bin/sh")
	if err != nil || !strings.HasPrefix(id, "dryrun-") {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if !strings.Contains(d.Name(), "dry-run") {
		t.Fatalf("dry run must be visible in the name: %q", d.Name())
	}
}

func TestProvidersNeedCredentials(t *testing.T) {
	if _, err := NewRunPod("", "").Launch(Offer{Type: "x"}, ""); err == nil {
		t.Fatal("launched a pod without an API key")
	}
	if _, err := NewHetzner("").Launch(cpuOffer(), ""); err == nil {
		t.Fatal("launched a server without a token")
	}
}

func TestFromEnvDefaultsToDryRun(t *testing.T) {
	t.Setenv("RUNPOD_API_KEY", "rpa_test")
	t.Setenv("HETZNER_API_TOKEN", "hz_test")
	t.Setenv("SCALE_DRY_RUN", "")
	providers := FromEnv()
	if len(providers) != 2 {
		t.Fatalf("got %d providers", len(providers))
	}
	for _, p := range providers {
		if !strings.Contains(p.Name(), "dry-run") {
			t.Fatalf("%s would spend money without being asked", p.Name())
		}
	}
}

func TestSpendingIsOptIn(t *testing.T) {
	t.Setenv("HETZNER_API_TOKEN", "hz_test")
	t.Setenv("SCALE_DRY_RUN", "false")
	providers := FromEnv()
	if len(providers) == 0 {
		t.Fatal("no providers built from credentials")
	}
	for _, p := range providers {
		if strings.Contains(p.Name(), "dry-run") {
			t.Fatalf("explicit opt-in did not take effect for %s", p.Name())
		}
	}
}

func TestNoCredentialsMeansNoProviders(t *testing.T) {
	t.Setenv("RUNPOD_API_KEY", "")
	t.Setenv("HETZNER_API_TOKEN", "")
	if got := FromEnv(); len(got) != 0 {
		t.Fatalf("invented %d providers", len(got))
	}
}

func TestBootstrapScriptIsSelfContained(t *testing.T) {
	script := BootstrapScript("https://mojojojo.cc", "secret-token", GPU)
	for _, want := range []string{
		"worker.tar.gz", "MOJOJOJO_RUNNER_TOKEN=secret-token",
		"MOJOJOJO_GPU=1", "systemctl enable --now mojojojo-runner",
		"useradd --system", "uv venv",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("bootstrap is missing %q", want)
		}
	}
}

func TestVectorBootstrapUsesRealCAGRAOnlyForGPU(t *testing.T) {
	cpu := VectorBootstrapScript("https://mojojojo.cc", "vector-token", CPU)
	gpu := VectorBootstrapScript("https://mojojojo.cc", "vector-token", GPU)
	for _, want := range []string{"vector-worker.tar.gz", "VECTOR_WORKER_TOKEN=vector-token",
		"VECTOR_PORT=4342", "libmojo_embed.so", "systemctl enable --now mojojojo-vector"} {
		if !strings.Contains(cpu, want) {
			t.Fatalf("vector bootstrap is missing %q", want)
		}
	}
	if strings.Contains(cpu, `if [ "1" = "1" ]`) || strings.Contains(cpu, "Environment=VECTOR_GPU_REQUESTED=1") {
		t.Fatal("CPU vector worker would install or advertise a GPU backend")
	}
	for _, want := range []string{"cuvs-cu12", "cupy-cuda12x", `if [ "1" = "1" ]`,
		"Environment=VECTOR_GPU_REQUESTED=1"} {
		if !strings.Contains(gpu, want) {
			t.Fatalf("GPU vector bootstrap is missing %q", want)
		}
	}
}
