// Package scale decides when to rent more machines, and when to give them
// back.
//
// The rule is the one that keeps a business alive: capacity is bought out of
// revenue that already exists, never out of hope. A worker is only rented when
// paying users are actually queueing for one, when the queue has been deep
// enough for long enough that it is not a spike, and when the money spent so
// far today leaves room for the bill.
//
// Everything here is a pure function of a snapshot. No clocks, no network, no
// goroutines — those live in the manager, which is the part that can go wrong
// in production. This part can be tested exhaustively, and is.
package scale

import (
	"fmt"
	"time"
)

// Kind is what a worker is for. GPUs and CPUs come from different providers
// with different economics, so they scale independently.
type Kind string

const (
	CPU Kind = "cpu"
	GPU Kind = "gpu"
)

// Snapshot is everything the decision depends on.
type Snapshot struct {
	Kind Kind
	// Queued is how many runs are waiting for a slot of this kind right now.
	Queued int
	// QueuedFor is how long the queue has been non-empty. A queue that has
	// existed for two seconds is a burst; one that has existed for two minutes
	// is a shortage.
	QueuedFor time.Duration
	// Idle is how long the local capacity has had nothing to do — the signal
	// to give rented machines back.
	Idle time.Duration

	// PaidCreditsPerHour is revenue actually being billed right now,
	// annualised to an hour, in credits. Free-tier and welcome-credit runs are
	// deliberately excluded: they are marketing, and marketing does not get to
	// authorise spending.
	PaidCreditsPerHour float64

	// Rented is what we already have running of this kind.
	Rented int
	// SpentTodayUSD is the day's rental bill so far.
	SpentTodayUSD float64

	// Workload is supplied by trusted queue telemetry, never a client's timing claim.
	Workload *Workload
}

// Limits are the hard edges. Nothing in this package will cross one.
type Limits struct {
	MaxWorkers      int
	DailyBudgetUSD  float64
	MinQueueDepth   int           // runs waiting before renting is considered
	MinQueueFor     time.Duration // ...and for how long
	IdleBeforeStop  time.Duration
	MarginMultiple  float64 // revenue must exceed cost by this much
	CooldownBetween time.Duration
}

// DefaultLimits are deliberately conservative: this is a service that runs on
// one box and rents help, not a fleet that happens to have a home node.
func DefaultLimits(kind Kind) Limits {
	l := Limits{
		MaxWorkers:      4,
		DailyBudgetUSD:  25,
		MinQueueDepth:   4,
		MinQueueFor:     45 * time.Second,
		IdleBeforeStop:  5 * time.Minute,
		MarginMultiple:  2.0,
		CooldownBetween: 3 * time.Minute,
	}
	if kind == GPU {
		// A GPU is 20x the price of a CPU box and takes minutes to come up, so
		// it needs a deeper, longer queue to justify and a shorter idle leash
		// to stop paying for.
		l.MaxWorkers = 2
		l.DailyBudgetUSD = 60
		l.MinQueueDepth = 2
		l.MinQueueFor = 90 * time.Second
		l.IdleBeforeStop = 4 * time.Minute
	}
	return l
}

// Action is what the manager should do next.
type Action struct {
	Scale  int    // +1 rent one, -1 return one, 0 do nothing
	Reason string // always set, including when Scale is 0
}

// Offer is a machine a provider will sell us.
type Offer struct {
	Provider  string
	Type      string
	USDHour   float64
	Kind      Kind
	MemoryGiB float64
}

// Decide returns the single next action for this snapshot.
//
// One worker at a time, on purpose: each new worker changes the queue, and a
// controller that reacts to its own last action is a controller that
// oscillates.
func Decide(s Snapshot, l Limits, offer Offer, sinceLastChange time.Duration) Action {
	if sinceLastChange < l.CooldownBetween {
		return Action{0, fmt.Sprintf("cooling down (%s of %s)",
			round(sinceLastChange), round(l.CooldownBetween))}
	}

	// Give machines back first: an idle rented worker is money leaving with
	// nothing coming back, and that check must never be gated behind the
	// conditions for renting one.
	if s.Rented > 0 && s.Queued == 0 && s.Idle >= l.IdleBeforeStop {
		return Action{-1, fmt.Sprintf("idle %s with an empty queue", round(s.Idle))}
	}

	if s.Queued < l.MinQueueDepth {
		return Action{0, fmt.Sprintf("queue %d is below the threshold of %d",
			s.Queued, l.MinQueueDepth)}
	}
	if s.QueuedFor < l.MinQueueFor {
		return Action{0, fmt.Sprintf("queue is only %s old, want %s — could be a burst",
			round(s.QueuedFor), round(l.MinQueueFor))}
	}
	if s.Rented >= l.MaxWorkers {
		return Action{0, fmt.Sprintf("already at the cap of %d rented workers", l.MaxWorkers)}
	}
	if !positiveFinite(offer.USDHour) {
		return Action{0, "no offer available from any provider"}
	}
	if !nonnegativeFinite(s.SpentTodayUSD) || !nonnegativeFinite(s.PaidCreditsPerHour) ||
		!positiveFinite(l.DailyBudgetUSD) || !positiveFinite(l.MarginMultiple) {
		return Action{0, "invalid budget or paid-demand telemetry"}
	}
	if s.SpentTodayUSD+offer.USDHour > l.DailyBudgetUSD {
		return Action{0, fmt.Sprintf("today's spend $%.2f + $%.2f/h would pass the $%.2f budget",
			s.SpentTodayUSD, offer.USDHour, l.DailyBudgetUSD)}
	}

	// The money test. Credits are $0.001, so revenue per hour in dollars is
	// credits per hour over a thousand. A worker is worth renting only when
	// what is being billed already covers its cost several times over —
	// otherwise the queue is real but the demand behind it is not paying.
	revenueUSDHour := s.PaidCreditsPerHour / 1000.0
	needed := offer.USDHour * l.MarginMultiple
	if revenueUSDHour < needed {
		return Action{0, fmt.Sprintf(
			"paid demand is $%.2f/h; $%.2f/h needed to justify a $%.2f/h %s",
			revenueUSDHour, needed, offer.USDHour, offer.Type)}
	}

	return Action{1, fmt.Sprintf(
		"%d runs queued for %s, paid demand $%.2f/h covers a $%.2f/h %s %sx over",
		s.Queued, round(s.QueuedFor), revenueUSDHour, offer.USDHour, offer.Type,
		fmt.Sprintf("%.1f", revenueUSDHour/offer.USDHour))}
}

// Cheapest picks the offer that costs least per hour for this kind.
func Cheapest(offers []Offer, kind Kind) Offer {
	var best Offer
	for _, o := range offers {
		if o.Kind != kind || !positiveFinite(o.USDHour) {
			continue
		}
		if best.USDHour == 0 || o.USDHour < best.USDHour {
			best = o
		}
	}
	return best
}

func round(d time.Duration) time.Duration { return d.Round(time.Second) }
