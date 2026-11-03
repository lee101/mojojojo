package scale

import (
	"log"
	"sync"
	"time"
)

// Manager runs the control loop: watch the queue, ask the policy, rent or
// return a machine, and keep a list of workers the web tier can send overflow
// runs to.
//
// It is deliberately boring. Every tick does at most one thing, every decision
// is logged with its reason whether or not it acts, and the default provider
// wrapper refuses to spend money — so the failure mode of the whole subsystem
// is "nothing happens", which is a failure mode a service can survive.
type Manager struct {
	Providers []Provider
	Limits    map[Kind]Limits
	// Bootstrap builds the script a new worker runs to become one.
	Bootstrap func(kind Kind) string
	// Signal reports the current state of the world. Supplied by the caller so
	// this package never has to know what a run or a credit is.
	Signal func(kind Kind) Snapshot
	// Interval between ticks.
	Interval time.Duration

	mu         sync.Mutex
	workers    map[string]*Worker
	lastChange map[Kind]time.Time
	spentToday float64
	spendDay   string
	decisions  []Decision
	stop       chan struct{}
}

// Decision is a log line, kept in memory so /v1/scale can show what the
// controller has been thinking without anyone reading journald.
type Decision struct {
	At     time.Time `json:"at"`
	Kind   Kind      `json:"kind"`
	Scale  int       `json:"scale"`
	Reason string    `json:"reason"`
	Worker string    `json:"worker,omitempty"`
	Err    string    `json:"error,omitempty"`
}

func NewManager(providers []Provider, signal func(Kind) Snapshot,
	bootstrap func(Kind) string) *Manager {
	return &Manager{
		Providers: providers,
		Limits:    map[Kind]Limits{CPU: DefaultLimits(CPU), GPU: DefaultLimits(GPU)},
		Bootstrap: bootstrap,
		Signal:    signal,
		Interval:  30 * time.Second,
		workers:   map[string]*Worker{},
		lastChange: map[Kind]time.Time{
			// Not zero: a controller that starts up mid-incident should watch
			// for one cooldown before it starts spending.
			CPU: time.Now(), GPU: time.Now(),
		},
		stop: make(chan struct{}),
	}
}

func (m *Manager) Start() {
	if len(m.Providers) == 0 {
		log.Printf("scale: no providers configured — overflow stays queued locally")
		return
	}
	names := make([]string, 0, len(m.Providers))
	for _, p := range m.Providers {
		names = append(names, p.Name())
	}
	log.Printf("scale: watching queue, providers: %v", names)
	go m.loop()
}

func (m *Manager) Stop() { close(m.stop) }

func (m *Manager) loop() {
	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.refresh()
			for _, kind := range []Kind{GPU, CPU} {
				m.tick(kind)
			}
		}
	}
}

func (m *Manager) tick(kind Kind) {
	snapshot := m.Signal(kind)
	snapshot.Kind = kind
	snapshot.Rented = m.count(kind)
	snapshot.SpentTodayUSD = m.spent()

	offers := m.allOffers()
	offer := Cheapest(offers, kind)
	var workloadReason string
	if snapshot.Workload != nil {
		quote := CheapestWorkload(offers, kind, *snapshot.Workload)
		offer, workloadReason = quote.Offer, quote.Reason
	}
	action := Decide(snapshot, m.Limits[kind], offer, time.Since(m.changed(kind)))
	if workloadReason != "" {
		action.Reason += "; " + workloadReason
	}

	switch action.Scale {
	case 1:
		m.rent(kind, offer, action.Reason)
	case -1:
		m.returnOne(kind, action.Reason)
	default:
		// Recorded but not logged: a controller that decides nothing every 30
		// seconds would drown the journal in its own calm.
		m.record(Decision{At: time.Now(), Kind: kind, Scale: 0, Reason: action.Reason})
	}
}

func (m *Manager) rent(kind Kind, offer Offer, reason string) {
	provider := m.providerFor(offer.Provider)
	if provider == nil {
		return
	}
	id, err := provider.Launch(offer, m.Bootstrap(kind))
	decision := Decision{At: time.Now(), Kind: kind, Scale: 1, Reason: reason, Worker: id}
	if err != nil {
		decision.Err = err.Error()
		log.Printf("scale: could not rent %s from %s: %v", offer.Type, offer.Provider, err)
		m.record(decision)
		return
	}
	log.Printf("scale: renting %s from %s ($%.3f/h) — %s", offer.Type, provider.Name(),
		offer.USDHour, reason)
	m.mu.Lock()
	m.workers[id] = &Worker{ID: id, Provider: offer.Provider, Kind: kind,
		Type: offer.Type, USDHour: offer.USDHour, Started: time.Now()}
	m.lastChange[kind] = time.Now()
	m.mu.Unlock()
	m.record(decision)
}

func (m *Manager) returnOne(kind Kind, reason string) {
	m.mu.Lock()
	var target *Worker
	for _, w := range m.workers {
		if w.Kind == kind {
			// Oldest first: it has had the most of its hour used up.
			if target == nil || w.Started.Before(target.Started) {
				target = w
			}
		}
	}
	m.mu.Unlock()
	if target == nil {
		return
	}
	provider := m.providerFor(target.Provider)
	decision := Decision{At: time.Now(), Kind: kind, Scale: -1, Reason: reason,
		Worker: target.ID}
	if provider != nil {
		if err := provider.Terminate(target.ID); err != nil {
			decision.Err = err.Error()
			log.Printf("scale: could not return %s: %v", target.ID, err)
			m.record(decision)
			return
		}
	}
	log.Printf("scale: returning %s (%s) — %s", target.ID, target.Type, reason)
	m.mu.Lock()
	m.chargeFor(target)
	delete(m.workers, target.ID)
	m.lastChange[kind] = time.Now()
	m.mu.Unlock()
	m.record(decision)
}

// chargeFor books what a worker cost. Called under the lock.
func (m *Manager) chargeFor(w *Worker) {
	hours := time.Since(w.Started).Hours()
	if hours < 1.0/60 {
		hours = 1.0 / 60 // nothing is free, not even a minute
	}
	m.rollDay()
	m.spentToday += hours * w.USDHour
}

func (m *Manager) rollDay() {
	day := time.Now().UTC().Format("2006-01-02")
	if m.spendDay != day {
		m.spendDay = day
		m.spentToday = 0
	}
}

// refresh polls providers for readiness and books the running bill.
func (m *Manager) refresh() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.workers))
	for id := range m.workers {
		ids = append(ids, id)
	}
	m.rollDay()
	m.mu.Unlock()

	for _, id := range ids {
		m.mu.Lock()
		w := m.workers[id]
		m.mu.Unlock()
		if w == nil {
			continue
		}
		provider := m.providerFor(w.Provider)
		if provider == nil {
			continue
		}
		status, err := provider.Status(id)
		if err != nil {
			continue
		}
		m.mu.Lock()
		if live := m.workers[id]; live != nil {
			live.IP = status.IP
			if status.Ready && !live.Ready {
				live.Ready = true
				log.Printf("scale: worker %s ready at %s", id, status.IP)
			}
		}
		m.mu.Unlock()
	}
}

// Ready returns rented workers that can take traffic.
func (m *Manager) Ready(kind Kind) []Worker {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Worker
	for _, w := range m.workers {
		if w.Ready && w.Kind == kind && w.IP != "" {
			out = append(out, *w)
		}
	}
	return out
}

// Status is what /v1/scale reports.
func (m *Manager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	workers := make([]Worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, *w)
	}
	recent := m.decisions
	if len(recent) > 20 {
		recent = recent[len(recent)-20:]
	}
	providers := make([]string, 0, len(m.Providers))
	for _, p := range m.Providers {
		providers = append(providers, p.Name())
	}
	return map[string]any{
		"providers":       providers,
		"workers":         workers,
		"spent_today_usd": round2(m.spentToday),
		"limits":          m.Limits,
		"decisions":       recent,
	}
}

func (m *Manager) count(kind Kind) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, w := range m.workers {
		if w.Kind == kind {
			n++
		}
	}
	return n
}

func (m *Manager) spent() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollDay()
	// Running workers are billing right now; count what they have used so far
	// or the budget only bites after a machine is returned.
	total := m.spentToday
	for _, w := range m.workers {
		total += time.Since(w.Started).Hours() * w.USDHour
	}
	return total
}

func (m *Manager) changed(kind Kind) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastChange[kind]
}

func (m *Manager) allOffers() []Offer {
	var out []Offer
	for _, p := range m.Providers {
		out = append(out, p.Offers()...)
	}
	return out
}

func (m *Manager) providerFor(name string) Provider {
	for _, p := range m.Providers {
		if p.Name() == name || p.Name() == name+" (dry-run)" {
			return p
		}
	}
	return nil
}

func (m *Manager) record(d Decision) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisions = append(m.decisions, d)
	if len(m.decisions) > 200 {
		m.decisions = m.decisions[len(m.decisions)-200:]
	}
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
