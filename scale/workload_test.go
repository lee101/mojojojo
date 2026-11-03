package scale

import (
	"math"
	"strings"
	"testing"
	"time"
)

func workloadFixture() ([]Offer, Workload) {
	offers := []Offer{
		{Provider: "fixture", Type: "slow", Kind: GPU, USDHour: 0.25, MemoryGiB: 24},
		{Provider: "fixture", Type: "fast", Kind: GPU, USDHour: 1, MemoryGiB: 32},
	}
	w := Workload{Revision: "model-data-runtime-sha", Jobs: 10, MemoryGiB: 20, MaxCostUSD: 1,
		Measurements: []WorkloadMeasurement{
			{Provider: "fixture", Type: "slow", Revision: "model-data-runtime-sha", JobTime: 10 * time.Minute},
			{Provider: "fixture", Type: "fast", Revision: "model-data-runtime-sha", JobTime: time.Minute},
		}}
	return offers, w
}

func TestWorkloadSelectsTotalCostNotHourlyPrice(t *testing.T) {
	offers, w := workloadFixture()
	got := CheapestWorkload(offers, GPU, w)
	if got.Offer.Type != "fast" || got.Duration != 10*time.Minute || math.Abs(got.CostUSD-1.0/6) > 1e-10 {
		t.Fatalf("unexpected quote: %+v", got)
	}
}

func TestWorkloadColdStartAndBillingFloor(t *testing.T) {
	offers, w := workloadFixture()
	w.Jobs = 1
	w.Measurements[1].ColdStart = time.Hour
	if got := CheapestWorkload(offers, GPU, w); got.Offer.Type != "slow" {
		t.Fatalf("ignored cold start: %+v", got)
	}
	w.Measurements[1].ColdStart = 0
	w.Measurements[1].MinimumBill = time.Hour
	if got := CheapestWorkload(offers, GPU, w); got.Offer.Type != "slow" {
		t.Fatalf("ignored minimum billing: %+v", got)
	}
}

func TestWorkloadRejectsInfeasibleOrUnmeasuredJobs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Workload)
	}{
		{"revision", func(w *Workload) { w.Revision = "different" }},
		{"memory", func(w *Workload) { w.MemoryGiB = 40 }},
		{"deadline", func(w *Workload) { w.Deadline = time.Minute }},
		{"budget", func(w *Workload) { w.MaxCostUSD = 0.001 }},
		{"no budget", func(w *Workload) { w.MaxCostUSD = 0 }},
		{"infinite budget", func(w *Workload) { w.MaxCostUSD = math.Inf(1) }},
		{"nan memory", func(w *Workload) { w.MemoryGiB = math.NaN() }},
		{"jobs", func(w *Workload) { w.Jobs = 0 }},
		{"overflow", func(w *Workload) {
			w.Jobs = math.MaxInt
			w.Measurements[0].JobTime = time.Duration(math.MaxInt64)
			w.Measurements[1].JobTime = time.Duration(math.MaxInt64)
		}},
		{"extra cost", func(w *Workload) {
			for i := range w.Measurements {
				w.Measurements[i].ExtraCostUSD = 2
			}
		}},
		{"invalid time", func(w *Workload) {
			for i := range w.Measurements {
				w.Measurements[i].ColdStart = -1
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offers, w := workloadFixture()
			tc.edit(&w)
			if got := CheapestWorkload(offers, GPU, w); got.Offer.USDHour != 0 || got.Reason == "" {
				t.Fatalf("accepted infeasible workload: %+v", got)
			}
		})
	}
}

func TestWorkloadRejectsInvalidOfferAndMeasurements(t *testing.T) {
	offers, w := workloadFixture()
	offers[0].USDHour = math.NaN()
	w.Measurements[1].ExtraCostUSD = math.Inf(1)
	if got := CheapestWorkload(offers, GPU, w); got.Offer.USDHour != 0 {
		t.Fatalf("accepted nonfinite values: %+v", got)
	}
	if got := Cheapest(offers, GPU); got.Type != "fast" {
		t.Fatalf("hourly selector accepted NaN: %+v", got)
	}
	if got := CheapestWorkload(offers, CPU, w); got.Offer.USDHour != 0 {
		t.Fatalf("selected wrong resource kind: %+v", got)
	}
}

func TestWorkloadManagerUsesMeasuredOfferAndRentalGates(t *testing.T) {
	offers, w := workloadFixture()
	p := &workloadProvider{offers: offers}
	snapshot := Snapshot{Queued: 10, QueuedFor: time.Hour, PaidCreditsPerHour: 10000, Workload: &w}
	m := NewManager([]Provider{p}, func(Kind) Snapshot { return snapshot }, func(Kind) string { return "fixture" })
	m.lastChange[GPU] = time.Now().Add(-time.Hour)
	m.tick(GPU)
	if p.launched.Type != "fast" {
		t.Fatalf("manager ignored workload cost: %+v", p.launched)
	}
	if !strings.Contains(m.decisions[0].Reason, w.Revision) {
		t.Fatal("missing workload audit reason")
	}
	p.launched = Offer{}
	m.lastChange[GPU] = time.Now().Add(-time.Hour)
	snapshot.PaidCreditsPerHour = 0
	m.tick(GPU)
	if p.launched.Type != "" {
		t.Fatal("workload quote bypassed paid-demand gate")
	}
	snapshot.PaidCreditsPerHour = 10000
	w.Revision = "unmeasured"
	m.tick(GPU)
	if p.launched.Type != "" {
		t.Fatal("unmeasured workload fell back to hourly rental")
	}
}

type workloadProvider struct {
	offers   []Offer
	launched Offer
}

func (p *workloadProvider) Name() string    { return "fixture" }
func (p *workloadProvider) Offers() []Offer { return p.offers }
func (p *workloadProvider) Launch(o Offer, _ string) (string, error) {
	p.launched = o
	return "fixture-worker", nil
}
func (p *workloadProvider) Status(id string) (Worker, error) { return Worker{ID: id}, nil }
func (p *workloadProvider) Terminate(string) error           { return nil }

func TestPolicyRejectsNonfiniteMoney(t *testing.T) {
	s := Snapshot{Queued: 10, QueuedFor: time.Hour, PaidCreditsPerHour: 10000}
	l := DefaultLimits(GPU)
	o := Offer{Kind: GPU, USDHour: math.NaN()}
	if got := Decide(s, l, o, time.Hour); got.Scale != 0 {
		t.Fatalf("accepted NaN price: %+v", got)
	}
	o.USDHour = 1
	s.PaidCreditsPerHour = math.NaN()
	if got := Decide(s, l, o, time.Hour); got.Scale != 0 {
		t.Fatalf("accepted NaN revenue: %+v", got)
	}
}
