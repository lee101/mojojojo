package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	vectorCapacityProduct = "vector-performance"
	capacityPeriod        = time.Hour
)

type capacityReservation struct {
	ID                 string     `json:"id"`
	Product            string     `json:"product"`
	Status             string     `json:"status"`
	CreditsPerHour     int        `json:"credits_per_hour"`
	CurrentPeriodStart time.Time  `json:"current_period_start"`
	NextChargeAt       time.Time  `json:"next_charge_at"`
	CreatedAt          time.Time  `json:"created_at"`
	CancelledAt        *time.Time `json:"cancelled_at,omitempty"`
	StatusReason       string     `json:"status_reason,omitempty"`
}

func (s *Server) vectorCapacityAvailable() bool {
	if s == nil || s.vector == nil {
		return false
	}
	health, err := s.vector.health()
	if err != nil {
		return false
	}
	ready, _ := health["gpu"].(bool)
	return ready
}

func (s *Server) vectorReservationAvailable() bool {
	if !s.vectorCapacityAvailable() {
		return false
	}
	var open int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM capacity_reservations
		WHERE product=$1 AND status IN ('active','paused')`, vectorCapacityProduct).Scan(&open); err != nil {
		return false
	}
	return open < max(1, s.cfg.ReservedVectorSlots)
}

func (s *Server) hasActiveVectorReservation(userID string) bool {
	if s == nil || s.db == nil || userID == "" {
		return false
	}
	var active bool
	_ = s.db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM capacity_reservations
		WHERE user_id = $1 AND product = $2 AND status = 'active'
	)`, userID, vectorCapacityProduct).Scan(&active)
	return active
}

func (s *Server) handleCapacityReservations(ctx *fasthttp.RequestCtx) {
	u := s.vectorUser(ctx)
	if u == nil {
		return
	}
	if ctx.IsPost() {
		s.createCapacityReservation(ctx, u)
		return
	}

	rows, err := s.db.Query(`SELECT id, product, status, credits_per_hour,
		current_period_start, next_charge_at, created_at, cancelled_at, status_reason
		FROM capacity_reservations WHERE user_id = $1 ORDER BY created_at DESC`, u.ID)
	if err != nil {
		sendError(ctx, 500, "could not load reservations")
		return
	}
	defer rows.Close()
	reservations := make([]capacityReservation, 0)
	for rows.Next() {
		var reservation capacityReservation
		var cancelled sql.NullTime
		if err := rows.Scan(&reservation.ID, &reservation.Product, &reservation.Status,
			&reservation.CreditsPerHour, &reservation.CurrentPeriodStart,
			&reservation.NextChargeAt, &reservation.CreatedAt, &cancelled,
			&reservation.StatusReason); err != nil {
			sendError(ctx, 500, "could not load reservations")
			return
		}
		if cancelled.Valid {
			at := cancelled.Time
			reservation.CancelledAt = &at
		}
		reservations = append(reservations, reservation)
	}
	if err := rows.Err(); err != nil {
		sendError(ctx, 500, "could not load reservations")
		return
	}
	sendJSON(ctx, map[string]any{
		"ok":        true,
		"available": s.vectorReservationAvailable(),
		"plan": map[string]any{
			"product":          vectorCapacityProduct,
			"name":             "Reserved vector performance",
			"credits_per_hour": s.cfg.ReservedVectorCredits,
			"usd_per_hour":     float64(s.cfg.ReservedVectorCredits) / 1000,
			"billing_interval": "hourly",
		},
		"reservations": reservations,
	})
}

func (s *Server) createCapacityReservation(ctx *fasthttp.RequestCtx, u *User) {
	var request struct {
		Product string `json:"product"`
	}
	if len(ctx.PostBody()) > 0 && json.Unmarshal(ctx.PostBody(), &request) != nil {
		sendError(ctx, 400, "invalid JSON")
		return
	}
	if request.Product != "" && request.Product != vectorCapacityProduct {
		sendError(ctx, 400, "unknown capacity product")
		return
	}
	if !s.vectorCapacityAvailable() {
		sendError(ctx, 503, "reserved vector capacity is currently unavailable")
		return
	}
	credits := s.cfg.ReservedVectorCredits
	if credits <= 0 {
		sendError(ctx, 503, "reserved vector capacity is not configured")
		return
	}

	now := time.Now().UTC()
	reservation := capacityReservation{
		ID: "cap_" + randomToken(12), Product: vectorCapacityProduct, Status: "active",
		CreditsPerHour: credits, CurrentPeriodStart: now,
		NextChargeAt: now.Add(capacityPeriod), CreatedAt: now,
	}
	tx, err := s.db.Begin()
	if err != nil {
		sendError(ctx, 500, "could not create reservation")
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('mojojojo-vector-capacity'))`); err != nil {
		sendError(ctx, 500, "could not create reservation")
		return
	}
	var existing int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM capacity_reservations
		WHERE user_id = $1 AND product = $2 AND status IN ('active','paused')`,
		u.ID, vectorCapacityProduct).Scan(&existing); err != nil {
		sendError(ctx, 500, "could not create reservation")
		return
	}
	if existing > 0 {
		sendError(ctx, 409, "you already have an open vector reservation")
		return
	}
	var open int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM capacity_reservations
		WHERE product = $1 AND status IN ('active','paused')`, vectorCapacityProduct).Scan(&open); err != nil {
		sendError(ctx, 500, "could not create reservation")
		return
	}
	if open >= max(1, s.cfg.ReservedVectorSlots) {
		sendError(ctx, 409, "reserved vector capacity is fully allocated")
		return
	}
	if err = spendExactTx(tx, u.ID, credits,
		fmt.Sprintf("capacity:%s:%s", vectorCapacityProduct, reservation.ID), now); err != nil {
		if errors.Is(err, errInsufficientCredits) {
			sendError(ctx, 402, "not enough credits for the first reserved hour")
		} else {
			sendError(ctx, 500, "could not charge reservation")
		}
		return
	}
	_, err = tx.Exec(`INSERT INTO capacity_reservations
		(id, user_id, product, status, credits_per_hour, current_period_start,
		 next_charge_at, created_at, updated_at)
		VALUES ($1,$2,$3,'active',$4,$5,$6,$5,$5)`, reservation.ID, u.ID,
		reservation.Product, credits, now, reservation.NextChargeAt)
	if err != nil || tx.Commit() != nil {
		sendError(ctx, 500, "could not create reservation")
		return
	}
	ctx.SetStatusCode(201)
	sendJSON(ctx, map[string]any{"ok": true, "reservation": reservation})
}

func (s *Server) handleCapacityReservationCancel(ctx *fasthttp.RequestCtx, reservationID string) {
	u := s.vectorUser(ctx)
	if u == nil {
		return
	}
	now := time.Now().UTC()
	result, err := s.db.Exec(`UPDATE capacity_reservations
		SET status = CASE WHEN status='active' THEN 'active' ELSE 'cancelled' END,
			cancelled_at = $1, updated_at = $1,
			status_reason = CASE WHEN status='active' THEN 'ends at next renewal' ELSE '' END
		WHERE id = $2 AND user_id = $3 AND status IN ('active','paused') AND cancelled_at IS NULL`,
		now, reservationID, u.ID)
	if err != nil {
		sendError(ctx, 500, "could not cancel reservation")
		return
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		sendError(ctx, 404, "reservation not found")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "cancelled_at": now})
}

// spendExactTx moves a fixed recurring charge through the same wallet and
// ledger used by compute. It either spends the full amount or spends nothing.
func spendExactTx(tx *sql.Tx, userID string, credits int, reason string, now time.Time) error {
	var free, plan, paid int
	var planExpire sql.NullTime
	if err := tx.QueryRow(`SELECT COALESCE(free_credits,0), COALESCE(plan_credits,0),
		plan_credits_expire_at, COALESCE(paid_credits,0)
		FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&free, &plan, &planExpire, &paid); err != nil {
		return err
	}
	if planExpire.Valid && planExpire.Time.Before(now) {
		plan = 0
	}
	if free+plan+paid < credits {
		return errInsufficientCredits
	}
	remaining := credits
	useFree := min(remaining, free)
	free -= useFree
	remaining -= useFree
	usePlan := min(remaining, plan)
	plan -= usePlan
	remaining -= usePlan
	paid -= remaining
	after := free + plan + paid
	if _, err := tx.Exec(`UPDATE users SET free_credits=$1, plan_credits=$2, paid_credits=$3
		WHERE id=$4`, free, plan, paid, userID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO credit_ledger
		(id, user_id, delta, reason, app_id, source, balance_after, created_at)
		VALUES ($1,$2,$3,$4,$5,'spend',$6,$7)`, randomToken(18), userID,
		-credits, reason, appID, after, now)
	return err
}

func (s *Server) startCapacityBilling() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for now := range ticker.C {
			if err := s.settleCapacityReservations(now.UTC()); err != nil {
				log.Printf("capacity billing: %v", err)
			}
		}
	}()
}

func (s *Server) settleCapacityReservations(now time.Time) error {
	if !s.vectorCapacityAvailable() {
		_, err := s.db.Exec(`UPDATE capacity_reservations SET status='paused',
			status_reason='capacity temporarily unavailable', updated_at=$1
			WHERE product=$2 AND status='active'`, now, vectorCapacityProduct)
		return err
	}
	rows, err := s.db.Query(`SELECT id FROM capacity_reservations
		WHERE product=$1 AND status IN ('active','paused') AND next_charge_at <= $2`,
		vectorCapacityProduct, now)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.settleCapacityReservation(id, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) settleCapacityReservation(id string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID, product, status string
	var credits int
	var due time.Time
	var cancelled sql.NullTime
	err = tx.QueryRow(`SELECT user_id, product, status, credits_per_hour, next_charge_at, cancelled_at
		FROM capacity_reservations WHERE id=$1 FOR UPDATE`, id).
		Scan(&userID, &product, &status, &credits, &due, &cancelled)
	if err == sql.ErrNoRows || status == "cancelled" || due.After(now) {
		return nil
	}
	if err != nil {
		return err
	}
	if cancelled.Valid {
		_, err = tx.Exec(`UPDATE capacity_reservations SET status='cancelled',
			status_reason='', updated_at=$1 WHERE id=$2`, now, id)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := spendExactTx(tx, userID, credits,
		fmt.Sprintf("capacity:%s:%s", product, id), now); err != nil {
		if !errors.Is(err, errInsufficientCredits) {
			return err
		}
		_, err = tx.Exec(`UPDATE capacity_reservations SET status='paused',
			status_reason='not enough credits', next_charge_at=$1, updated_at=$2 WHERE id=$3`,
			now.Add(capacityPeriod), now, id)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.Exec(`UPDATE capacity_reservations SET status='active', status_reason='',
		current_period_start=$1, next_charge_at=$2, updated_at=$1 WHERE id=$3`,
		now, now.Add(capacityPeriod), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
