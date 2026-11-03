package main

// Database as a service: public pricing, tier limits, and per-account usage
// for the Data API. Free-tier accounts stay on shared limits; an account that
// has bought credits gets the usage tier. Dedicated Hetzner instances are
// priced here but provisioned by hand until the scale/ provider is wired.

import (
	"time"

	"github.com/valyala/fasthttp"
)

type dataTier struct {
	Name               string `json:"name"`
	Databases          int    `json:"databases"`
	RecordsPerDatabase int64  `json:"records_per_database"`
	BytesPerDatabase   int64  `json:"bytes_per_database"`
}

var (
	dataTierFree = dataTier{"free", dataNamespaceLimit, dataRecordLimit, dataDatabaseMaxBytes}
	dataTierPaid = dataTier{"usage", 100, 10_000_000, 10 << 30}
)

func dataTierFor(u *User) dataTier {
	if u != nil && u.PaidCredit+u.PlanCredit > 0 {
		return dataTierPaid
	}
	return dataTierFree
}

type dedicatedPlan struct {
	Name     string `json:"name"`
	USDMonth int    `json:"usd_per_month"`
	VCPU     int    `json:"vcpu"`
	RAMGB    int    `json:"ram_gb"`
	DiskGB   int    `json:"nvme_gb"`
	Backup   string `json:"backup"`
}

var dedicatedPlans = []dedicatedPlan{
	{"small", 15, 2, 4, 80, "continuous to R2, 7 day PITR"},
	{"standard", 49, 4, 16, 160, "continuous to R2, 14 day PITR"},
	{"large", 129, 8, 32, 240, "continuous to R2, 30 day PITR"},
}

func (s *Server) dbPricing() map[string]any {
	meters := []map[string]any{}
	for _, id := range []string{"db.read", "db.write", "db.storage"} {
		m := meterDefs[id]
		meters = append(meters, map[string]any{"id": m.ID, "label": m.Label, "unit": m.Unit,
			"usd_per_unit": m.USD(), "usd_per_million": m.USD() * 1e6, "free_per_month": m.Free})
	}
	return map[string]any{
		"ok": true, "currency": "USD", "credit_usd": usdPerCredit,
		"tiers": []dataTier{dataTierFree, dataTierPaid}, "meters": meters,
		"dedicated": dedicatedPlans, "dedicated_status": "preview",
		"backends": map[string]string{
			"shared": "postgres on NVMe", "cold": "R2 via mojocloud r2-tier (verified upload, read-through LRU)",
			"dedicated": "Hetzner NVMe instance, rented on demand"},
	}
}

func (s *Server) handleDBPricing(ctx *fasthttp.RequestCtx) { sendJSON(ctx, s.dbPricing()) }

func (s *Server) handleDBUsage(ctx *fasthttp.RequestCtx) {
	u := s.dataUser(ctx)
	if u == nil {
		return
	}
	s.flushMeters()
	month := meterMonth(time.Now())
	rows, err := s.db.Query(`SELECT meter,qty,owed_micro FROM usage_meters WHERE user_id=$1 AND month=$2 ORDER BY meter`, u.ID, month)
	if err != nil {
		sendError(ctx, 500, "could not read usage")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	var estimate float64
	for rows.Next() {
		var id string
		var qty, owed float64
		if rows.Scan(&id, &qty, &owed) != nil {
			continue
		}
		def := meterDefs[id]
		billable := max(0, qty-def.Free)
		cost := billable * def.USD()
		estimate += cost
		items = append(items, map[string]any{"meter": id, "label": def.Label, "unit": def.Unit,
			"quantity": qty, "free": def.Free, "billable": billable, "usd": cost})
	}
	sendJSON(ctx, map[string]any{"ok": true, "month": month, "tier": dataTierFor(u).Name,
		"meters": items, "estimated_usd": estimate, "credits": u.Credits()})
}

func (s *Server) handleDatabasePage(ctx *fasthttp.RequestCtx) {
	meters := []meterDef{meterDefs["db.read"], meterDefs["db.write"], meterDefs["db.storage"]}
	s.render(ctx, "database.html", map[string]any{
		"Meta": s.metaAt("mojojojo database — pay-per-use document database API",
			"A tenant-scoped JSON database API with a free tier, usage pricing per request and GB-hour, R2 cold storage and dedicated Hetzner instances.",
			"/database"),
		"User": s.auth.userFromSession(ctx), "Meters": meters, "Dedicated": dedicatedPlans,
		"Free": dataTierFree, "Paid": dataTierPaid,
	})
}
