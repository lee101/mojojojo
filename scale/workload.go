package scale

import (
	"fmt"
	"math"
	"time"
)

// Workload describes one homogeneous batch, pinned to a model/data/runtime revision.
// Measurements must come from the operator's benchmark store, not a public request.
type Workload struct {
	Revision     string
	Jobs         int
	MemoryGiB    float64
	Deadline     time.Duration
	MaxCostUSD   float64
	Measurements []WorkloadMeasurement
}

type WorkloadMeasurement struct {
	Provider     string
	Type         string
	Revision     string
	JobTime      time.Duration
	ColdStart    time.Duration
	MinimumBill  time.Duration
	ExtraCostUSD float64
}

type WorkloadQuote struct {
	Offer    Offer
	Duration time.Duration
	CostUSD  float64
	Reason   string
}

func positiveFinite(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func nonnegativeFinite(v float64) bool {
	return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// CheapestWorkload minimizes estimated total batch cost, not hourly price.
// No revision-matched evidence means no rental, rather than guessed throughput.
func CheapestWorkload(offers []Offer, kind Kind, w Workload) WorkloadQuote {
	best := WorkloadQuote{Reason: "no feasible revision-matched workload measurement"}
	if w.Revision == "" || w.Jobs <= 0 || !positiveFinite(w.MemoryGiB) ||
		w.Deadline < 0 || !positiveFinite(w.MaxCostUSD) {
		best.Reason = "invalid workload: require revision, jobs, memory and a finite positive cost cap"
		return best
	}
	for _, o := range offers {
		if o.Kind != kind || !positiveFinite(o.USDHour) || !positiveFinite(o.MemoryGiB) ||
			o.MemoryGiB < w.MemoryGiB {
			continue
		}
		for _, m := range w.Measurements {
			if m.Provider != o.Provider || m.Type != o.Type || m.Revision != w.Revision ||
				m.JobTime <= 0 || m.ColdStart < 0 || m.MinimumBill < 0 ||
				!nonnegativeFinite(m.ExtraCostUSD) {
				continue
			}
			// Guard duration multiplication before converting the batch size.
			if uint64(w.Jobs) > uint64((time.Duration(math.MaxInt64)-m.ColdStart)/m.JobTime) {
				continue
			}
			elapsed := m.ColdStart + time.Duration(w.Jobs)*m.JobTime
			if w.Deadline > 0 && elapsed > w.Deadline {
				continue
			}
			billed := max(elapsed, m.MinimumBill)
			cost := billed.Hours()*o.USDHour + m.ExtraCostUSD
			if !positiveFinite(cost) || cost > w.MaxCostUSD {
				continue
			}
			if best.Offer.USDHour == 0 || cost < best.CostUSD ||
				(cost == best.CostUSD && elapsed < best.Duration) {
				best = WorkloadQuote{Offer: o, Duration: elapsed, CostUSD: cost,
					Reason: fmt.Sprintf("workload %s: %d jobs, estimated $%.6f over %s including cold start and billing floor",
						w.Revision, w.Jobs, cost, elapsed)}
			}
		}
	}
	return best
}
