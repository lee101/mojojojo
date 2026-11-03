package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
	"github.com/valyala/fasthttp"
)

const (
	stripeCreditsPerCent    = 10
	stripeMinimumTopUpCents = 500
	stripeMaximumTopUpCents = 50_000
	developerMonthlyCredits = 12_000
	developerAnnualCredits  = 144_000
	stripeRequestTimeout    = 20 * time.Second
	stripeIntegrationPrefix = "mojojojo_"
)

type stripeOffer struct {
	ID               string
	Mode             string
	PriceID          string
	CreditsPerPeriod int
}

type stripeGateway interface {
	CreateCustomer(context.Context, *stripe.CustomerCreateParams) (*stripe.Customer, error)
	CreateCheckoutSession(context.Context, *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error)
	RetrieveCheckoutSession(context.Context, string) (*stripe.CheckoutSession, error)
	CreatePortalSession(context.Context, *stripe.BillingPortalSessionCreateParams) (*stripe.BillingPortalSession, error)
	RetrieveSubscription(context.Context, string) (*stripe.Subscription, error)
	ConstructEvent([]byte, string, string) (stripe.Event, error)
}

type stripeSDKGateway struct{ client *stripe.Client }

func (g *stripeSDKGateway) CreateCustomer(ctx context.Context, p *stripe.CustomerCreateParams) (*stripe.Customer, error) {
	return g.client.V1Customers.Create(ctx, p)
}

func (g *stripeSDKGateway) CreateCheckoutSession(ctx context.Context, p *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	return g.client.V1CheckoutSessions.Create(ctx, p)
}

func (g *stripeSDKGateway) RetrieveCheckoutSession(ctx context.Context, id string) (*stripe.CheckoutSession, error) {
	return g.client.V1CheckoutSessions.Retrieve(ctx, id, nil)
}

func (g *stripeSDKGateway) CreatePortalSession(ctx context.Context, p *stripe.BillingPortalSessionCreateParams) (*stripe.BillingPortalSession, error) {
	return g.client.V1BillingPortalSessions.Create(ctx, p)
}

func (g *stripeSDKGateway) RetrieveSubscription(ctx context.Context, id string) (*stripe.Subscription, error) {
	return g.client.V1Subscriptions.Retrieve(ctx, id, nil)
}

func (g *stripeSDKGateway) ConstructEvent(payload []byte, signature, secret string) (stripe.Event, error) {
	return g.client.ConstructEvent(payload, signature, secret)
}

type stripeBilling struct {
	db            *sql.DB
	auth          *authStore
	cfg           Config
	gateway       stripeGateway
	mu            sync.Mutex
	customerLocks map[string]*sync.Mutex
}

func newStripeBilling(db *sql.DB, auth *authStore, cfg Config) (*stripeBilling, error) {
	if strings.TrimSpace(cfg.StripeSecretKey) == "" && strings.TrimSpace(cfg.StripePublishableKey) == "" {
		return nil, nil
	}
	if cfg.StripeSecretKey == "" || cfg.StripePublishableKey == "" {
		return nil, errors.New("Stripe requires both STRIPE_SECRET_KEY and STRIPE_PUBLISHABLE_KEY")
	}
	client := stripe.NewClient(cfg.StripeSecretKey)
	if cfg.StripeAutomaticTax {
		ctx, cancel := context.WithTimeout(context.Background(), stripeRequestTimeout)
		defer cancel()
		list := client.V1TaxRegistrations.List(ctx, &stripe.TaxRegistrationListParams{
			Status: stripe.String("active"),
		})
		if err := list.Err(); err != nil {
			return nil, fmt.Errorf("verify active Stripe Tax registrations: %w", err)
		}
		if len(list.Data()) == 0 {
			return nil, errors.New("STRIPE_AUTOMATIC_TAX=true but Stripe has no active tax registration")
		}
	}
	return &stripeBilling{
		db: db, auth: auth, cfg: cfg,
		gateway:       &stripeSDKGateway{client: client},
		customerLocks: map[string]*sync.Mutex{},
	}, nil
}

func (b *stripeBilling) offers() map[string]stripeOffer {
	return map[string]stripeOffer{
		"credits": {
			ID: "credits", Mode: "payment", PriceID: b.cfg.StripeCreditsPriceID,
		},
		"developer_monthly": {
			ID: "developer_monthly", Mode: "subscription",
			PriceID: b.cfg.StripeDeveloperMonthlyPriceID, CreditsPerPeriod: developerMonthlyCredits,
		},
		"developer_annual": {
			ID: "developer_annual", Mode: "subscription",
			PriceID: b.cfg.StripeDeveloperAnnualPriceID, CreditsPerPeriod: developerAnnualCredits,
		},
		"dedicated_monthly": {
			ID: "dedicated_monthly", Mode: "subscription", PriceID: b.cfg.StripeDedicatedMonthlyPriceID,
		},
		"dedicated_annual": {
			ID: "dedicated_annual", Mode: "subscription", PriceID: b.cfg.StripeDedicatedAnnualPriceID,
		},
	}
}

func (b *stripeBilling) configured() bool { return b != nil }

func (b *stripeBilling) customerLock(userID string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	lock := b.customerLocks[userID]
	if lock == nil {
		lock = &sync.Mutex{}
		b.customerLocks[userID] = lock
	}
	return lock
}

func (b *stripeBilling) customerID(ctx context.Context, user *User) (string, error) {
	lock := b.customerLock(user.ID)
	lock.Lock()
	defer lock.Unlock()

	var id string
	if err := b.db.QueryRowContext(ctx,
		"SELECT customer_id FROM stripe_customers WHERE user_id = $1", user.ID).Scan(&id); err == nil {
		return id, nil
	} else if err != sql.ErrNoRows {
		return "", err
	}

	params := &stripe.CustomerCreateParams{
		Email: stripe.String(user.Email),
		Name:  stripe.String(user.DisplayName()),
		Metadata: map[string]string{
			"mojojojo_user_id": user.ID,
		},
	}
	params.SetIdempotencyKey("mojojojo-customer-" + user.ID)
	customer, err := b.gateway.CreateCustomer(ctx, params)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	_, err = b.db.ExecContext(ctx, `INSERT INTO stripe_customers
		(user_id, customer_id, created_at, updated_at) VALUES ($1, $2, $3, $3)
		ON CONFLICT (user_id) DO UPDATE SET customer_id = EXCLUDED.customer_id, updated_at = EXCLUDED.updated_at`,
		user.ID, customer.ID, now)
	return customer.ID, err
}

type stripeCheckoutRequest struct {
	Offer    string `json:"offer"`
	Quantity int64  `json:"quantity"`
}

func (s *Server) handleStripeCheckout(ctx *fasthttp.RequestCtx) {
	s.startCheckout(ctx, false)
}

// startCheckout creates an embedded Checkout Session for the signed-in user.
// inline sessions stay on the page that opened them (a business subdomain): they
// complete with redirect_on_completion=never and the page learns of success from
// Stripe.js, while the credits themselves arrive through the same webhook as
// every other purchase.
func (s *Server) startCheckout(ctx *fasthttp.RequestCtx, inline bool) {
	user := s.auth.userFromSession(ctx)
	if user == nil {
		sendError(ctx, fasthttp.StatusUnauthorized, "sign in before starting checkout")
		return
	}
	if s.stripe == nil {
		sendError(ctx, fasthttp.StatusServiceUnavailable, "Stripe checkout is not configured")
		return
	}
	var request stripeCheckoutRequest
	if err := json.Unmarshal(ctx.PostBody(), &request); err != nil {
		sendError(ctx, fasthttp.StatusBadRequest, "invalid JSON body")
		return
	}
	offer, ok := s.stripe.offers()[request.Offer]
	if !ok || offer.PriceID == "" {
		sendError(ctx, fasthttp.StatusBadRequest, "that billing offer is unavailable")
		return
	}
	quantity := int64(1)
	credits := offer.CreditsPerPeriod
	expectedSubtotal := int64(0)
	if offer.ID == "credits" {
		quantity = request.Quantity
		if quantity < stripeMinimumTopUpCents || quantity > stripeMaximumTopUpCents {
			sendError(ctx, fasthttp.StatusBadRequest, "credit top-ups must be between $5 and $500")
			return
		}
		credits = int(quantity) * stripeCreditsPerCent
		expectedSubtotal = quantity
	} else if strings.HasPrefix(offer.ID, "developer_") && s.stripe.hasOpenDeveloperSubscription(user.ID) {
		sendError(ctx, fasthttp.StatusConflict, "a developer subscription is already active; use Manage billing to change it")
		return
	}

	requestCtx, cancel := context.WithTimeout(context.Background(), stripeRequestTimeout)
	defer cancel()
	customerID, err := s.stripe.customerID(requestCtx, user)
	if err != nil {
		log.Printf("stripe customer user=%s: %v", user.ID, err)
		sendError(ctx, fasthttp.StatusBadGateway, "Stripe could not create a customer")
		return
	}

	returnURL := s.cfg.SiteBaseURL + "/credits?checkout=return&session_id={CHECKOUT_SESSION_ID}"
	params := &stripe.CheckoutSessionCreateParams{
		Mode:                 stripe.String(offer.Mode),
		UIMode:               stripe.String(string(stripe.CheckoutSessionUIModeEmbeddedPage)),
		ReturnURL:            stripe.String(returnURL),
		RedirectOnCompletion: stripe.String(string(stripe.CheckoutSessionRedirectOnCompletionAlways)),
		Customer:             stripe.String(customerID),
		CustomerUpdate: &stripe.CheckoutSessionCreateCustomerUpdateParams{
			Address: stripe.String("auto"), Name: stripe.String("auto"),
		},
		ClientReferenceID:     stripe.String(user.ID),
		IntegrationIdentifier: stripe.String(stripeIntegrationPrefix + randomLetters(8)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			Price: stripe.String(offer.PriceID), Quantity: stripe.Int64(quantity),
		}},
		TaxIDCollection: &stripe.CheckoutSessionCreateTaxIDCollectionParams{Enabled: stripe.Bool(true)},
		Metadata: map[string]string{
			"mojojojo_user_id": user.ID,
			"mojojojo_offer":   offer.ID,
		},
	}
	if s.cfg.StripeAutomaticTax {
		params.AutomaticTax = &stripe.CheckoutSessionCreateAutomaticTaxParams{Enabled: stripe.Bool(true)}
	}
	if offer.Mode == "payment" {
		params.InvoiceCreation = &stripe.CheckoutSessionCreateInvoiceCreationParams{Enabled: stripe.Bool(true)}
		params.Metadata["mojojojo_credits"] = strconv.Itoa(credits)
	} else {
		params.AllowPromotionCodes = stripe.Bool(true)
		params.SubscriptionData = &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Description: stripe.String("mojojojo " + strings.ReplaceAll(offer.ID, "_", " ")),
			Metadata: map[string]string{
				"mojojojo_user_id":            user.ID,
				"mojojojo_offer":              offer.ID,
				"mojojojo_credits_per_period": strconv.Itoa(offer.CreditsPerPeriod),
			},
		}
	}
	if inline {
		params.ReturnURL = nil
		params.RedirectOnCompletion = stripe.String(string(stripe.CheckoutSessionRedirectOnCompletionNever))
	}
	params.SetIdempotencyKey("mojojojo-checkout-" + randomToken(18))
	session, err := s.stripe.gateway.CreateCheckoutSession(requestCtx, params)
	if err != nil {
		log.Printf("stripe checkout user=%s offer=%s: %v", user.ID, offer.ID, err)
		sendError(ctx, fasthttp.StatusBadGateway, "Stripe could not start checkout")
		return
	}
	_, err = s.db.ExecContext(requestCtx, `INSERT INTO stripe_checkouts
		(session_id, user_id, offer, mode, credits, expected_subtotal, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, session.ID, user.ID, offer.ID, offer.Mode,
		credits, expectedSubtotal, time.Now().UTC())
	if err != nil {
		log.Printf("stripe checkout record session=%s: %v", session.ID, err)
		sendError(ctx, fasthttp.StatusInternalServerError, "checkout was created but could not be recorded; try again")
		return
	}
	sendJSON(ctx, map[string]any{
		"ok": true, "client_secret": session.ClientSecret,
		"publishable_key": s.cfg.StripePublishableKey, "session_id": session.ID,
	})
}

func (b *stripeBilling) hasOpenDeveloperSubscription(userID string) bool {
	var exists bool
	_ = b.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM stripe_subscriptions
		WHERE user_id = $1 AND offer LIKE 'developer_%' AND status IN ('active','trialing','past_due'))`,
		userID).Scan(&exists)
	return exists
}

func (s *Server) handleStripePortal(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	if user == nil {
		sendError(ctx, fasthttp.StatusUnauthorized, "sign in before managing billing")
		return
	}
	if s.stripe == nil {
		sendError(ctx, fasthttp.StatusServiceUnavailable, "Stripe billing is not configured")
		return
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), stripeRequestTimeout)
	defer cancel()
	customerID, err := s.stripe.customerID(requestCtx, user)
	if err != nil {
		sendError(ctx, fasthttp.StatusBadGateway, "Stripe could not load this billing account")
		return
	}
	portal, err := s.stripe.gateway.CreatePortalSession(requestCtx, &stripe.BillingPortalSessionCreateParams{
		Customer: stripe.String(customerID), ReturnURL: stripe.String(s.cfg.SiteBaseURL + "/credits"),
	})
	if err != nil {
		log.Printf("stripe portal user=%s: %v", user.ID, err)
		sendError(ctx, fasthttp.StatusBadGateway, "Stripe could not open the billing portal")
		return
	}
	sendJSON(ctx, map[string]any{"ok": true, "url": portal.URL})
}

func (s *Server) handleStripeSession(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	if user == nil || s.stripe == nil {
		sendError(ctx, fasthttp.StatusUnauthorized, "sign in to view checkout")
		return
	}
	sessionID := strings.TrimSpace(string(ctx.QueryArgs().Peek("session_id")))
	var owner string
	if sessionID == "" || s.db.QueryRow("SELECT user_id FROM stripe_checkouts WHERE session_id = $1", sessionID).Scan(&owner) != nil || owner != user.ID {
		sendError(ctx, fasthttp.StatusNotFound, "checkout session not found")
		return
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), stripeRequestTimeout)
	defer cancel()
	session, err := s.stripe.gateway.RetrieveCheckoutSession(requestCtx, sessionID)
	if err != nil {
		sendError(ctx, fasthttp.StatusBadGateway, "Stripe could not retrieve checkout")
		return
	}
	if session.Status == stripe.CheckoutSessionStatusComplete && session.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid {
		if err := s.stripe.completeCheckout(session); err != nil {
			log.Printf("stripe reconcile session=%s: %v", session.ID, err)
			sendError(ctx, fasthttp.StatusInternalServerError, "payment is complete but fulfillment is still pending")
			return
		}
	}
	sendJSON(ctx, map[string]any{"ok": true, "status": session.Status, "payment_status": session.PaymentStatus})
}

func (s *Server) handleStripeWebhook(ctx *fasthttp.RequestCtx) {
	if s.stripe == nil || s.cfg.StripeWebhookSecret == "" {
		sendError(ctx, fasthttp.StatusServiceUnavailable, "Stripe webhook is not configured")
		return
	}
	signature := string(ctx.Request.Header.Peek("Stripe-Signature"))
	event, err := s.stripe.gateway.ConstructEvent(ctx.PostBody(), signature, s.cfg.StripeWebhookSecret)
	if err != nil {
		log.Printf("stripe webhook rejected: %v", err)
		sendError(ctx, fasthttp.StatusBadRequest, "invalid Stripe webhook signature")
		return
	}
	if event.Data == nil {
		sendError(ctx, fasthttp.StatusBadRequest, "Stripe event has no data")
		return
	}
	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted, stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded:
		var session stripe.CheckoutSession
		err = json.Unmarshal(event.Data.Raw, &session)
		if err == nil {
			err = s.stripe.completeCheckout(&session)
		}
	case stripe.EventTypeInvoicePaid:
		var invoice stripe.Invoice
		err = json.Unmarshal(event.Data.Raw, &invoice)
		if err == nil {
			err = s.stripe.completeInvoice(&invoice)
		}
	case stripe.EventTypeCustomerSubscriptionDeleted:
		var subscription stripe.Subscription
		err = json.Unmarshal(event.Data.Raw, &subscription)
		if err == nil {
			err = s.stripe.cancelSubscription(&subscription)
		}
	case stripe.EventTypeChargeRefunded:
		var charge stripe.Charge
		err = json.Unmarshal(event.Data.Raw, &charge)
		if err == nil {
			err = s.stripe.applyRefund(event.ID, &charge)
		}
	default:
		// Stripe emits many informational events. Acknowledge those we don't
		// use so it doesn't retry events that cannot change local entitlement.
	}
	if err != nil {
		log.Printf("stripe webhook type=%s id=%s: %v", event.Type, event.ID, err)
		sendError(ctx, fasthttp.StatusInternalServerError, "Stripe event processing failed")
		return
	}
	sendJSON(ctx, map[string]any{"received": true})
}

func (b *stripeBilling) completeCheckout(session *stripe.CheckoutSession) error {
	if session == nil || session.ID == "" {
		return errors.New("missing checkout session")
	}
	if session.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid && session.PaymentStatus != stripe.CheckoutSessionPaymentStatusNoPaymentRequired {
		return nil
	}
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var userID, offer, mode string
	var credits int
	var expectedSubtotal int64
	if err := tx.QueryRow(`SELECT user_id, offer, mode, credits, expected_subtotal FROM stripe_checkouts
		WHERE session_id = $1 FOR UPDATE`, session.ID).Scan(&userID, &offer, &mode, &credits, &expectedSubtotal); err != nil {
		return fmt.Errorf("checkout %s was not created by mojojojo: %w", session.ID, err)
	}
	if session.ClientReferenceID != "" && session.ClientReferenceID != userID {
		return errors.New("checkout client reference does not match its owner")
	}
	paymentIntentID := ""
	if session.PaymentIntent != nil {
		paymentIntentID = session.PaymentIntent.ID
	}
	if mode == "payment" {
		if expectedSubtotal <= 0 || session.AmountSubtotal != expectedSubtotal {
			return fmt.Errorf("checkout subtotal %d does not match expected subtotal %d", session.AmountSubtotal, expectedSubtotal)
		}
		result, err := tx.Exec(`INSERT INTO stripe_credit_grants
			(external_id, user_id, kind, credits, created_at) VALUES ($1, $2, 'topup', $3, $4)
			ON CONFLICT (external_id) DO NOTHING`, session.ID, userID, credits, time.Now().UTC())
		if err != nil {
			return err
		}
		inserted, _ := result.RowsAffected()
		if inserted > 0 {
			if err := b.addPaidCredits(tx, userID, credits, "stripe:topup:"+session.ID); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`UPDATE stripe_checkouts SET status = 'paid', amount_total = $2,
		payment_intent_id = $3, completed_at = COALESCE(completed_at, $4) WHERE session_id = $1`,
		session.ID, session.AmountTotal, paymentIntentID, time.Now().UTC())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (b *stripeBilling) addPaidCredits(tx *sql.Tx, userID string, credits int, reason string) error {
	var debt int
	if err := tx.QueryRow(`SELECT refund_debt_credits FROM stripe_customers WHERE user_id = $1 FOR UPDATE`,
		userID).Scan(&debt); err != nil {
		return err
	}
	debtPaid := min(credits, debt)
	credits -= debtPaid
	debt -= debtPaid
	if _, err := tx.Exec(`UPDATE stripe_customers SET refund_debt_credits = $1, updated_at = $2 WHERE user_id = $3`,
		debt, time.Now().UTC(), userID); err != nil {
		return err
	}
	if credits == 0 {
		return nil
	}
	var free, plan, paid int
	if err := tx.QueryRow(`SELECT free_credits, plan_credits, paid_credits FROM users WHERE id = $1 FOR UPDATE`,
		userID).Scan(&free, &plan, &paid); err != nil {
		return err
	}
	paid += credits
	if _, err := tx.Exec("UPDATE users SET paid_credits = $1 WHERE id = $2", paid, userID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO credit_ledger
		(id, user_id, delta, reason, app_id, source, balance_after, created_at)
		VALUES ($1, $2, $3, $4, $5, 'stripe', $6, $7)`, randomToken(18), userID,
		credits, reason, appID, free+plan+paid, time.Now().UTC())
	return err
}

func (b *stripeBilling) completeInvoice(invoice *stripe.Invoice) error {
	if invoice == nil || invoice.ID == "" || invoice.Status != stripe.InvoiceStatusPaid {
		return nil
	}
	if invoice.BillingReason != stripe.InvoiceBillingReasonSubscriptionCreate && invoice.BillingReason != stripe.InvoiceBillingReasonSubscriptionCycle {
		return nil
	}
	if invoice.Parent == nil || invoice.Parent.SubscriptionDetails == nil || invoice.Parent.SubscriptionDetails.Subscription == nil {
		return nil
	}
	subscriptionID := invoice.Parent.SubscriptionDetails.Subscription.ID
	requestCtx, cancel := context.WithTimeout(context.Background(), stripeRequestTimeout)
	defer cancel()
	subscription, err := b.gateway.RetrieveSubscription(requestCtx, subscriptionID)
	if err != nil {
		return fmt.Errorf("retrieve subscription %s: %w", subscriptionID, err)
	}
	userID := subscription.Metadata["mojojojo_user_id"]
	metadataOffer := subscription.Metadata["mojojojo_offer"]
	if userID == "" {
		return errors.New("subscription is missing mojojojo fulfillment metadata")
	}
	periodEnd, priceID := subscriptionPeriod(subscription)
	offer, ok := b.offerForPrice(priceID)
	if !ok || offer.Mode != "subscription" {
		return fmt.Errorf("subscription uses unrecognized price %q", priceID)
	}
	if metadataOffer != "" && metadataOffer != offer.ID {
		return errors.New("subscription offer metadata does not match its configured price")
	}
	if subscription.Customer == nil || subscription.Customer.ID == "" {
		return errors.New("subscription has no customer")
	}
	var mappedUserID string
	if err := b.db.QueryRow(`SELECT user_id FROM stripe_customers WHERE customer_id=$1`,
		subscription.Customer.ID).Scan(&mappedUserID); err != nil || mappedUserID != userID {
		return errors.New("subscription customer does not match its mojojojo owner")
	}
	if periodEnd.IsZero() {
		periodEnd = time.Now().UTC().AddDate(0, 1, 0)
		if strings.HasSuffix(offer.ID, "_annual") {
			periodEnd = time.Now().UTC().AddDate(1, 0, 0)
		}
	}
	return b.refreshSubscriptionCredits(invoice.ID, subscription, userID, offer.ID, priceID, offer.CreditsPerPeriod, periodEnd)
}

func (b *stripeBilling) offerForPrice(priceID string) (stripeOffer, bool) {
	for _, offer := range b.offers() {
		if offer.PriceID != "" && offer.PriceID == priceID {
			return offer, true
		}
	}
	return stripeOffer{}, false
}

func subscriptionPeriod(subscription *stripe.Subscription) (time.Time, string) {
	if subscription == nil || subscription.Items == nil || len(subscription.Items.Data) == 0 {
		return time.Time{}, ""
	}
	item := subscription.Items.Data[0]
	priceID := ""
	if item.Price != nil {
		priceID = item.Price.ID
	}
	if item.CurrentPeriodEnd <= 0 {
		return time.Time{}, priceID
	}
	return time.Unix(item.CurrentPeriodEnd, 0).UTC(), priceID
}

func (b *stripeBilling) refreshSubscriptionCredits(invoiceID string, subscription *stripe.Subscription, userID, offer, priceID string, credits int, periodEnd time.Time) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO stripe_credit_grants
		(external_id, user_id, kind, credits, created_at) VALUES ($1, $2, 'subscription', $3, $4)
		ON CONFLICT (external_id) DO NOTHING`, invoiceID, userID, credits, time.Now().UTC())
	if err != nil {
		return err
	}
	inserted, _ := result.RowsAffected()
	if inserted > 0 && credits > 0 {
		var free, oldPlan, paid int
		if err := tx.QueryRow(`SELECT free_credits, plan_credits, paid_credits FROM users WHERE id = $1 FOR UPDATE`,
			userID).Scan(&free, &oldPlan, &paid); err != nil {
			return err
		}
		delta := credits - oldPlan
		if _, err := tx.Exec(`UPDATE users SET plan_credits = $1, plan_credits_expire_at = $2 WHERE id = $3`,
			credits, periodEnd, userID); err != nil {
			return err
		}
		if delta != 0 {
			if _, err := tx.Exec(`INSERT INTO credit_ledger
				(id, user_id, delta, reason, app_id, source, balance_after, created_at)
				VALUES ($1, $2, $3, $4, $5, 'stripe', $6, $7)`, randomToken(18), userID,
				delta, "stripe:subscription:"+invoiceID, appID, free+credits+paid, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	customerID := ""
	if subscription.Customer != nil {
		customerID = subscription.Customer.ID
	}
	_, err = tx.Exec(`INSERT INTO stripe_subscriptions
		(subscription_id, user_id, customer_id, offer, price_id, status, credits_per_period,
		 current_period_end, latest_invoice_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		ON CONFLICT (subscription_id) DO UPDATE SET status=EXCLUDED.status,
			price_id=EXCLUDED.price_id, credits_per_period=EXCLUDED.credits_per_period,
			current_period_end=EXCLUDED.current_period_end, latest_invoice_id=EXCLUDED.latest_invoice_id,
			updated_at=EXCLUDED.updated_at`, subscription.ID, userID, customerID, offer, priceID,
		string(subscription.Status), credits, periodEnd, invoiceID, time.Now().UTC())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (b *stripeBilling) cancelSubscription(subscription *stripe.Subscription) error {
	if subscription == nil || subscription.ID == "" {
		return nil
	}
	var userID string
	var credits int
	if err := b.db.QueryRow(`SELECT user_id, credits_per_period FROM stripe_subscriptions
		WHERE subscription_id=$1`, subscription.ID).Scan(&userID, &credits); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if userID == "" {
		return nil
	}
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if credits > 0 {
		var free, plan, paid int
		if err := tx.QueryRow(`SELECT free_credits, plan_credits, paid_credits FROM users WHERE id = $1 FOR UPDATE`,
			userID).Scan(&free, &plan, &paid); err != nil {
			return err
		}
		if plan > 0 {
			if _, err := tx.Exec(`UPDATE users SET plan_credits=0, plan_credits_expire_at=NULL WHERE id=$1`, userID); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO credit_ledger
				(id,user_id,delta,reason,app_id,source,balance_after,created_at)
				VALUES ($1,$2,$3,$4,$5,'stripe',$6,$7)`, randomToken(18), userID, -plan,
				"stripe:subscription_cancelled:"+subscription.ID, appID, free+paid, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`UPDATE stripe_subscriptions SET status='canceled', updated_at=$2
		WHERE subscription_id=$1`, subscription.ID, time.Now().UTC())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (b *stripeBilling) applyRefund(eventID string, charge *stripe.Charge) error {
	if charge == nil || charge.PaymentIntent == nil || charge.AmountRefunded <= 0 {
		return nil
	}
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sessionID, userID string
	var credits, amountTotal, alreadyRefunded int
	if err := tx.QueryRow(`SELECT session_id,user_id,credits,amount_total,amount_refunded
		FROM stripe_checkouts WHERE payment_intent_id=$1 AND mode='payment' FOR UPDATE`,
		charge.PaymentIntent.ID).Scan(&sessionID, &userID, &credits, &amountTotal, &alreadyRefunded); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if amountTotal <= 0 {
		return errors.New("cannot apply refund to checkout with no paid amount")
	}
	cumulative := int(charge.AmountRefunded)
	if cumulative > amountTotal {
		cumulative = amountTotal
	}
	if cumulative <= alreadyRefunded {
		return nil
	}
	desiredClawback := credits * cumulative / amountTotal
	previousClawback := credits * alreadyRefunded / amountTotal
	clawback := desiredClawback - previousClawback
	var free, plan, paid, debt int
	if err := tx.QueryRow(`SELECT u.free_credits,u.plan_credits,u.paid_credits,c.refund_debt_credits
		FROM users u JOIN stripe_customers c ON c.user_id=u.id WHERE u.id=$1 FOR UPDATE`,
		userID).Scan(&free, &plan, &paid, &debt); err != nil {
		return err
	}
	deducted := min(clawback, paid)
	paid -= deducted
	debt += clawback - deducted
	if _, err := tx.Exec("UPDATE users SET paid_credits=$1 WHERE id=$2", paid, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE stripe_customers SET refund_debt_credits=$1,updated_at=$2 WHERE user_id=$3`,
		debt, time.Now().UTC(), userID); err != nil {
		return err
	}
	if deducted > 0 {
		if _, err := tx.Exec(`INSERT INTO credit_ledger
			(id,user_id,delta,reason,app_id,source,balance_after,created_at)
			VALUES ($1,$2,$3,$4,$5,'stripe_refund',$6,$7)`, randomToken(18), userID, -deducted,
			"stripe:refund:"+eventID, appID, free+plan+paid, time.Now().UTC()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE stripe_checkouts SET amount_refunded=$1,status=$2 WHERE session_id=$3`,
		cumulative, map[bool]string{true: "refunded", false: "partially_refunded"}[cumulative == amountTotal], sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func randomLetters(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, n)
	random := make([]byte, n)
	if _, err := rand.Read(random); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	for i := range out {
		out[i] = alphabet[int(random[i])%len(alphabet)]
	}
	return string(out)
}
