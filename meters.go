package main

// Usage meters for the mojo businesses and the Data database. Prices are in
// micro-credits (1e-6 credit, $1e-9) so request-sized charges are exact; whole
// credits move through the ledger as they accrue and the remainder stays owed.
// Adds are batched in memory and flushed, so a request costs no SQL.

import (
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"
)

const microPerCredit = 1e6

type meterDef struct {
	ID, Label, Unit string
	Micro           float64
	Free            float64
}

func (m meterDef) USD() float64 { return m.Micro / microPerCredit * usdPerCredit }

var meterDefs = map[string]meterDef{
	"db.read":       {"db.read", "Reads", "request", 400, 100000},
	"db.write":      {"db.write", "Writes", "request", 2000, 50000},
	"db.storage":    {"db.storage", "Hot storage (NVMe/Postgres)", "GB-hour", 342466, 73},
	"obj.storage":   {"obj.storage", "Object storage (R2-tiered)", "GB-hour", 68493, 730},
	"obj.put":       {"obj.put", "Object writes", "request", 4000, 10000},
	"obj.get":       {"obj.get", "Object reads", "request", 400, 100000},
	"obj.egress":    {"obj.egress", "Egress", "GB", 10_000_000, 1},
	"queue.job":     {"queue.job", "Queue jobs", "job", 1000, 100000},
	"search.query":  {"search.query", "Search queries", "query", 100000, 1000},
	"how.ask":       {"how.ask", "Agent runs", "run", 200000, 50},
	"how.tokens":    {"how.tokens", "Model tokens", "1k tokens", 400000, 100},
	"how.search":    {"how.search", "Knowledge-base searches", "query", 100000, 1000},
	"how.websearch": {"how.websearch", "Live web searches", "query", 5000000, 20},

	// Mojo model businesses (modeld). Prices are per reported unit, so a request
	// costs exactly what it used: 20000 micro = $0.00002 per 1k tokens ($0.02/M).
	"model.embed.tokens":       {"model.embed.tokens", "Embedding tokens", "1k tokens", 20000, 1000},
	"model.rerank.pairs":       {"model.rerank.pairs", "Query-document pairs", "1k pairs", 10_000_000, 1},
	"model.moderate.images":    {"model.moderate.images", "Images moderated", "image", 1_000_000, 100},
	"model.transcribe.minutes": {"model.transcribe.minutes", "Audio transcribed", "minute", 3_000_000, 10},
	"model.score.rows":         {"model.score.rows", "Rows trained or scored", "1k rows", 2_000_000, 100},
}

var partMeters = map[string][]string{
	"kv":           {"db.read", "db.write", "db.storage"},
	"objstore":     {"obj.storage", "obj.put", "obj.get", "obj.egress"},
	"s3-gateway":   {"obj.put", "obj.get", "obj.egress"},
	"r2-tier":      {"obj.storage"},
	"queue":        {"queue.job"},
	"search-hooks": {"search.query"},
	"how":          {"how.ask", "how.tokens", "how.search", "how.websearch"},

	"model-embed":      {"model.embed.tokens"},
	"model-rerank":     {"model.rerank.pairs"},
	"model-moderate":   {"model.moderate.images"},
	"model-transcribe": {"model.transcribe.minutes"},
	"model-score":      {"model.score.rows"},
}

func metersFor(parts []string) []meterDef {
	seen := map[string]bool{}
	var out []meterDef
	for _, p := range parts {
		for _, id := range partMeters[p] {
			if !seen[id] {
				seen[id] = true
				out = append(out, meterDefs[id])
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// accrue is the whole billing rule: usage inside the monthly free allowance is
// free, the rest is priced exactly, and the owed total splits into whole
// credits to charge now and a remainder to carry.
func accrue(def meterDef, monthQty, add, owedMicro float64) (credits int, newOwed, billedMicro float64) {
	over := math.Max(0, monthQty+add-def.Free) - math.Max(0, monthQty-def.Free)
	billedMicro = over * def.Micro
	total := owedMicro + billedMicro
	credits = int(math.Floor(total / microPerCredit))
	return credits, total - float64(credits)*microPerCredit, billedMicro
}

type meterKey struct{ user, meter string }

type meterBuffer struct {
	mu  sync.Mutex
	buf map[meterKey]float64
}

func newMeterBuffer() *meterBuffer { return &meterBuffer{buf: map[meterKey]float64{}} }

func (m *meterBuffer) add(user, meter string, qty float64) {
	m.mu.Lock()
	m.buf[meterKey{user, meter}] += qty
	m.mu.Unlock()
}

func (m *meterBuffer) drain() map[meterKey]float64 {
	m.mu.Lock()
	out := m.buf
	m.buf = map[meterKey]float64{}
	m.mu.Unlock()
	return out
}

func meterMonth(t time.Time) string { return t.UTC().Format("2006-01") }

// meterAdd queues usage; flushMeters settles it.
func (s *Server) meterAdd(userID, meter string, qty float64) {
	if s.meters != nil && userID != "" && qty > 0 {
		s.meters.add(userID, meter, qty)
	}
}

func (s *Server) flushMeters() {
	if s.meters == nil || s.db == nil {
		return
	}
	month := meterMonth(time.Now())
	for k, qty := range s.meters.drain() {
		def, ok := meterDefs[k.meter]
		if !ok {
			continue
		}
		if err := s.settleMeter(k.user, def, month, qty); err != nil {
			log.Printf("meters: %s %s: %v", k.user, k.meter, err)
			s.meters.add(k.user, k.meter, qty)
		}
	}
}

func (s *Server) settleMeter(userID string, def meterDef, month string, qty float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO usage_meters(user_id,meter,month,qty,owed_micro,updated_at)
		VALUES($1,$2,$3,0,0,now()) ON CONFLICT DO NOTHING`, userID, def.ID, month); err != nil {
		return err
	}
	var cur, owed float64
	if err = tx.QueryRow(`SELECT qty,owed_micro FROM usage_meters
		WHERE user_id=$1 AND meter=$2 AND month=$3 FOR UPDATE`, userID, def.ID, month).Scan(&cur, &owed); err != nil {
		return err
	}
	credits, newOwed, _ := accrue(def, cur, qty, owed)
	if _, err = tx.Exec(`UPDATE usage_meters SET qty=$4,owed_micro=$5,updated_at=now()
		WHERE user_id=$1 AND meter=$2 AND month=$3`, userID, def.ID, month, cur+qty, newOwed); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if credits > 0 && s.bill != nil {
		taken, _, err := s.bill.spendReason(userID, credits, fmt.Sprintf("meter:%s:%g", def.ID, qty))
		if err != nil {
			return err
		}
		if taken < credits {
			_, err = s.db.Exec(`UPDATE usage_meters SET owed_micro=owed_micro+$4
				WHERE user_id=$1 AND meter=$2 AND month=$3`, userID, def.ID, month, float64(credits-taken)*microPerCredit)
			return err
		}
	}
	return nil
}

// meterBlocked reports whether a user with no credits has used up the free
// allowance, so a request must be refused rather than served on credit.
func (s *Server) meterBlocked(u *User, meter string) bool {
	if u == nil {
		return true
	}
	if u.Credits() > 0 {
		return false
	}
	def := meterDefs[meter]
	var qty float64
	_ = s.db.QueryRow(`SELECT COALESCE(qty,0) FROM usage_meters WHERE user_id=$1 AND meter=$2 AND month=$3`,
		u.ID, meter, meterMonth(time.Now())).Scan(&qty)
	return qty >= def.Free
}

func (s *Server) startMetering() {
	s.meters = newMeterBuffer()
	go func() {
		flush, storage := time.NewTicker(5*time.Second), time.NewTicker(time.Hour)
		for {
			select {
			case <-flush.C:
				s.flushMeters()
			case <-storage.C:
				s.accrueDataStorage()
			}
		}
	}()
}

func (s *Server) accrueDataStorage() {
	rows, err := s.db.Query(`SELECT n.user_id,
		COALESCE(SUM(pg_column_size(r.value)+octet_length(r.search_text)+octet_length(r.record_key)),0)
		FROM data_namespaces n JOIN data_records r ON r.namespace_id=n.id GROUP BY n.user_id`)
	if err != nil {
		log.Printf("meters: storage scan: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var user string
		var bytes float64
		if rows.Scan(&user, &bytes) == nil {
			s.meterAdd(user, "db.storage", bytes/(1<<30))
		}
	}
}
