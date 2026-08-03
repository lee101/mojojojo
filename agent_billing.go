package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

type agentCharge struct {
	Tokens          int64   `json:"tokens"`
	AccruedCredits  float64 `json:"accrued_credits"`
	ChargedCredits  int     `json:"charged_credits"`
	OwedCredits     float64 `json:"owed_credits"`
	Balance         int     `json:"balance"`
	TokensPerCredit float64 `json:"tokens_per_credit"`
}

func (s *Server) handleAgentSettlement(ctx *fasthttp.RequestCtx) {
	expected := "Bearer " + s.cfg.AgentToken
	presented := string(ctx.Request.Header.Peek("Authorization"))
	if s.cfg.AgentToken == "" || len(presented) != len(expected) ||
		subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
		sendError(ctx, http.StatusUnauthorized, "invalid agent service credential")
		return
	}
	var request struct {
		UserID string `json:"user_id"`
		RunID  string `json:"run_id"`
		Model  string `json:"model"`
		Tokens int64  `json:"tokens"`
	}
	if json.Unmarshal(ctx.PostBody(), &request) != nil {
		sendError(ctx, http.StatusBadRequest, "invalid JSON")
		return
	}
	request.UserID = strings.TrimSpace(request.UserID)
	request.RunID = strings.TrimSpace(request.RunID)
	request.Model = strings.TrimSpace(request.Model)
	if request.UserID == "" || request.RunID == "" || len(request.RunID) > 96 ||
		request.Model == "" || len(request.Model) > 128 || request.Tokens < 0 || request.Tokens > 100_000_000 {
		sendError(ctx, http.StatusBadRequest, "invalid agent settlement")
		return
	}
	user := s.auth.userByID(request.UserID)
	if user == nil {
		sendError(ctx, http.StatusNotFound, "user not found")
		return
	}
	charge, err := s.bill.chargeAgent(user, request.RunID, request.Model, request.Tokens)
	if err != nil {
		sendError(ctx, http.StatusInternalServerError, "agent usage settlement failed")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "charge": charge})
}

func (b *billing) chargeAgent(user *User, runID, model string, tokens int64) (agentCharge, error) {
	perCredit := b.cfg.AgentTokensPerCredit
	if !isFinitePositive(perCredit) {
		return agentCharge{}, fmt.Errorf("invalid agent token price")
	}
	lock := b.lockFor(user.ID)
	lock.Lock()
	defer lock.Unlock()

	var existing agentCharge
	err := b.db.QueryRow(`SELECT tokens,accrued_credits,charged_credits,
		owed_credits_after,balance_after FROM agent_usage WHERE run_id=$1 AND user_id=$2`,
		runID, user.ID).Scan(&existing.Tokens, &existing.AccruedCredits,
		&existing.ChargedCredits, &existing.OwedCredits, &existing.Balance)
	if err == nil {
		existing.TokensPerCredit = perCredit
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return agentCharge{}, err
	}

	rawCredits := float64(tokens) / perCredit
	var owed float64
	err = b.db.QueryRow(`SELECT owed_credits FROM agent_accrual WHERE user_id=$1`, user.ID).Scan(&owed)
	if err != nil && err != sql.ErrNoRows {
		return agentCharge{}, err
	}
	owed += rawCredits
	credits := int(math.Floor(owed))
	if credits > 0 {
		owed -= float64(credits)
	}
	balance := user.Credits()
	if credits > 0 {
		spent, nextBalance, err := b.spendReason(user.ID, credits,
			fmt.Sprintf("agent:%s:%d", model, tokens))
		if err != nil {
			return agentCharge{}, err
		}
		if spent < credits {
			owed += float64(credits - spent)
		}
		credits, balance = spent, nextBalance
	}
	now := time.Now().UTC()
	if _, err := b.db.Exec(`INSERT INTO agent_accrual
		(user_id,owed_credits,total_credits,updated_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT(user_id) DO UPDATE SET owed_credits=EXCLUDED.owed_credits,
		total_credits=agent_accrual.total_credits+EXCLUDED.total_credits,
		updated_at=EXCLUDED.updated_at`, user.ID, owed, rawCredits, now); err != nil {
		return agentCharge{}, err
	}
	if _, err := b.db.Exec(`INSERT INTO agent_usage
		(run_id,user_id,model,tokens,accrued_credits,charged_credits,
		 owed_credits_after,balance_after,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, runID, user.ID, model, tokens,
		rawCredits, credits, owed, balance, now); err != nil {
		return agentCharge{}, err
	}
	return agentCharge{Tokens: tokens, AccruedCredits: round3(rawCredits),
		ChargedCredits: credits, OwedCredits: round3(owed), Balance: balance,
		TokensPerCredit: perCredit}, nil
}

func isFinitePositive(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}
