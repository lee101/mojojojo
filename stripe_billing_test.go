package main

import (
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
)

func insertStripeTestUser(t *testing.T, billing *stripeBilling, userID string, paid, plan int) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := billing.db.Exec(`INSERT INTO users
		(id,email,password_hash,created_at,paid_credits,plan_credits)
		VALUES ($1,$2,'x',$3,$4,$5)`, userID, userID+"@example.com", now, paid, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := billing.db.Exec(`INSERT INTO stripe_customers
		(user_id,customer_id,created_at,updated_at) VALUES ($1,$2,$3,$3)`,
		userID, "cus_"+userID, now); err != nil {
		t.Fatal(err)
	}
}

func TestStripeTopUpFulfillmentIsIdempotent(t *testing.T) {
	db := testPostgres(t)
	billing := &stripeBilling{db: db}
	insertStripeTestUser(t, billing, "topup-user", 0, 0)
	if _, err := db.Exec(`INSERT INTO stripe_checkouts
		(session_id,user_id,offer,mode,credits,expected_subtotal,created_at)
		VALUES ('cs_topup','topup-user','credits','payment',5000,500,$1)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	session := &stripe.CheckoutSession{
		ID: "cs_topup", ClientReferenceID: "topup-user",
		PaymentStatus:  stripe.CheckoutSessionPaymentStatusPaid,
		AmountSubtotal: 500, AmountTotal: 500,
		PaymentIntent: &stripe.PaymentIntent{ID: "pi_topup"},
	}
	if err := billing.completeCheckout(session); err != nil {
		t.Fatal(err)
	}
	if err := billing.completeCheckout(session); err != nil {
		t.Fatal(err)
	}
	var paid, grants, ledger int
	if err := db.QueryRow(`SELECT paid_credits FROM users WHERE id='topup-user'`).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM stripe_credit_grants WHERE external_id='cs_topup'`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM credit_ledger WHERE user_id='topup-user' AND source='stripe'`).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if paid != 5000 || grants != 1 || ledger != 1 {
		t.Fatalf("paid=%d grants=%d ledger=%d", paid, grants, ledger)
	}
}

func TestStripeTopUpRejectsSubtotalMismatch(t *testing.T) {
	db := testPostgres(t)
	billing := &stripeBilling{db: db}
	insertStripeTestUser(t, billing, "amount-user", 0, 0)
	if _, err := db.Exec(`INSERT INTO stripe_checkouts
		(session_id,user_id,offer,mode,credits,expected_subtotal,created_at)
		VALUES ('cs_amount','amount-user','credits','payment',5000,500,$1)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	err := billing.completeCheckout(&stripe.CheckoutSession{
		ID: "cs_amount", ClientReferenceID: "amount-user",
		PaymentStatus:  stripe.CheckoutSessionPaymentStatusPaid,
		AmountSubtotal: 1, AmountTotal: 1,
	})
	if err == nil {
		t.Fatal("expected subtotal mismatch to reject fulfillment")
	}
	var paid int
	if err := db.QueryRow(`SELECT paid_credits FROM users WHERE id='amount-user'`).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 0 {
		t.Fatalf("unexpected paid credits: %d", paid)
	}
}

func TestStripeSubscriptionRefreshAndCancellation(t *testing.T) {
	db := testPostgres(t)
	billing := &stripeBilling{db: db}
	insertStripeTestUser(t, billing, "subscriber", 30, 2000)
	periodEnd := time.Now().UTC().AddDate(0, 1, 0)
	subscription := &stripe.Subscription{
		ID: "sub_developer", Status: stripe.SubscriptionStatusActive,
		Customer: &stripe.Customer{ID: "cus_subscriber"},
	}
	if err := billing.refreshSubscriptionCredits("in_first", subscription, "subscriber",
		"developer_monthly", "price_monthly", 12000, periodEnd); err != nil {
		t.Fatal(err)
	}
	// Retrying the same invoice must not grant twice.
	if err := billing.refreshSubscriptionCredits("in_first", subscription, "subscriber",
		"developer_monthly", "price_monthly", 12000, periodEnd); err != nil {
		t.Fatal(err)
	}
	var plan, grants int
	if err := db.QueryRow(`SELECT plan_credits FROM users WHERE id='subscriber'`).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM stripe_credit_grants WHERE user_id='subscriber'`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if plan != 12000 || grants != 1 {
		t.Fatalf("plan=%d grants=%d", plan, grants)
	}

	if err := billing.cancelSubscription(subscription); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT plan_credits FROM users WHERE id='subscriber'`).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM stripe_subscriptions WHERE subscription_id='sub_developer'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if plan != 0 || status != "canceled" {
		t.Fatalf("plan=%d status=%s", plan, status)
	}
}

func TestStripeRefundClawsBackCreditsAndTracksDebt(t *testing.T) {
	db := testPostgres(t)
	billing := &stripeBilling{db: db}
	insertStripeTestUser(t, billing, "refund-user", 5000, 0)
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO stripe_checkouts
		(session_id,user_id,offer,mode,credits,expected_subtotal,amount_total,payment_intent_id,status,created_at)
		VALUES ('cs_refund','refund-user','credits','payment',5000,500,500,'pi_refund','paid',$1)`, now); err != nil {
		t.Fatal(err)
	}
	if err := billing.applyRefund("evt_half", &stripe.Charge{
		PaymentIntent: &stripe.PaymentIntent{ID: "pi_refund"}, AmountRefunded: 250,
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate spending the remaining paid credits before the rest is refunded.
	if _, err := db.Exec(`UPDATE users SET paid_credits=0 WHERE id='refund-user'`); err != nil {
		t.Fatal(err)
	}
	if err := billing.applyRefund("evt_full", &stripe.Charge{
		PaymentIntent: &stripe.PaymentIntent{ID: "pi_refund"}, AmountRefunded: 500,
	}); err != nil {
		t.Fatal(err)
	}
	var paid, debt, refunded int
	if err := db.QueryRow(`SELECT paid_credits FROM users WHERE id='refund-user'`).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT refund_debt_credits FROM stripe_customers WHERE user_id='refund-user'`).Scan(&debt); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT amount_refunded FROM stripe_checkouts WHERE session_id='cs_refund'`).Scan(&refunded); err != nil {
		t.Fatal(err)
	}
	if paid != 0 || debt != 2500 || refunded != 500 {
		t.Fatalf("paid=%d debt=%d refunded=%d", paid, debt, refunded)
	}
}
