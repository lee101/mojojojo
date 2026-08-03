# mojojojo Stripe integration plan

Generated for `https://mojojojo.cc` on 2026-08-02. The Stripe implementation
planner was not exposed in the current Codex session after installing the
curated Stripe plugin and registering `https://mcp.stripe.com`, so this plan
uses the requested official Stripe documentation fallback.

## Production state

- Live Stripe account `acct_1TzqL3HvAeDvSxUQ` is activated for charges and payouts.
- Payments, Billing, Invoicing, embedded Checkout, and the customer portal are configured.
- The production webhook is `https://mojojojo.cc/v1/billing/webhook`.
- Stripe Tax remains deliberately disabled: the account currently has no active Tax registration.
- Secret keys and the webhook signing secret are local deployment configuration; only Price IDs are safe to expose in client-facing output.

## Catalog and economics

| Offer | Stripe price | Entitlement | Renewal behavior |
| --- | ---: | --- | --- |
| Compute Credits | $0.01 per unit; $5 minimum | 10 mojojojo credits per cent | Paid credits do not expire |
| Developer monthly | $12/month | 12,000 plan credits | Refreshes to 12,000 each paid month; unused plan credits do not roll over |
| Developer annual | $120/year | 144,000 plan credits | Two months free; included credits expire at the annual renewal |
| Dedicated RTX 5090 | $699/month | Reserved 32 GB GPU workspace | Manual capacity/provisioning fulfillment |
| Dedicated RTX 5090 annual | $6,990/year | Same workspace | Two months free; manual capacity/provisioning fulfillment |

One mojojojo credit is worth $0.001 of metered service. At the current meters,
12,000 credits cover about 33.3 CPU core-hours at $0.36/core-hour or 400 GPU
minutes at $1.80/GPU-hour. The $5 top-up minimum prevents fixed card fees from
dominating very small purchases. The annual offers exchange two months of
headline revenue for longer retention and cash collected up front.

The dedicated offer should be enabled only while capacity is actually
available. Its price needs a quarterly review against electricity, hardware
amortization, failed-hardware reserve, bandwidth, support time, payment fees,
and the opportunity cost of occupying the GPU. The current code records the
paid subscription, but provisioning remains an operator workflow.

## Integration flow

1. A signed-in user chooses an offer on `/credits`.
2. The Go server creates or reuses a Stripe Customer and creates an embedded
   Checkout Session for a server-owned Price ID. Payment details go directly to Stripe.
3. Stripe redirects back to `/credits`, where the app retrieves only the
   session already recorded for that authenticated user.
4. A signature-verified webhook is authoritative for fulfillment.
5. One-time payments add idempotent paid-credit grants. `invoice.paid` refreshes
   subscription credits from the configured Price ID. Refunds claw back unused
   paid credits and record debt if refunded credits were already consumed.
6. The Stripe customer portal handles cards, invoices, and cancellation.

Every credit grant has a unique Stripe object ID, so event retries cannot grant
twice. Fulfillment checks the Checkout subtotal, customer ownership, and the
server-configured Price ID rather than trusting client input or mutable metadata.

## Payments, Billing, and Invoicing

- Use embedded Checkout Sessions; do not collect or proxy card data.
- One-time credit purchases request a Stripe invoice. Subscriptions create
  recurring invoices naturally.
- Developer credits work identically from browser notebooks, hosted functions,
  GPU/CPU runs, SDKs, and API keys because every path spends the same ledger.
- Plan credits are consumed before paid credits under the existing wallet
  policy; prepaid paid credits remain available after a subscription ends.
- The portal is configured for payment-method updates, invoice history, and
  cancellation at period end.
- Dedicated subscriptions require an operator capacity alert/provisioning step
  before this can be called fully automated fulfillment.

## Stripe Tax decision gate

Do not set `STRIPE_AUTOMATIC_TAX=true` until the business has registered in the
required jurisdictions and Stripe reports at least one active registration.
The application enforces that gate at startup.

Stripe's catalog offers several plausible codes, but the exact classification
must be confirmed for what customers actually buy:

- `txcd_10701100` — Website Hosting
- `txcd_10102000` — Platform as a service (PaaS), business use
- `txcd_10101000` — Infrastructure as a service (IaaS), business use
- `txcd_10701300` — Website Data Processing

After tax advice confirms the classifications, assign the chosen code to each
live product, add Stripe Tax registrations, test customer locations and tax-ID
collection, then enable automatic tax. Do not infer registrations from the
Stripe account country.

## Production checklist

- [x] Live account activated for charges and payouts
- [x] Live products and monthly/annual Prices created in USD
- [x] Embedded Checkout smoke-tested without a charge
- [x] Customer portal configured
- [x] Signature-verified production webhook configured
- [x] Database migrations, idempotent ledger fulfillment, refund debt, and subscription expiry implemented
- [x] Live configuration stored in a gitignored mode-0600 file
- [ ] Rotate the live secret key disclosed in the setup conversation and replace it in deployment configuration
- [ ] Confirm product tax codes and create required Stripe Tax registrations
- [ ] Add operator alerting and inventory gating for dedicated-workspace purchases
- [ ] Run one controlled low-value live purchase and refund after deployment, then verify the Stripe event log and local credit ledger

## API surface

- `POST /v1/billing/checkout` — authenticated embedded Checkout Session
- `GET /v1/billing/session` — authenticated return reconciliation
- `POST /v1/billing/portal` — authenticated customer portal session
- `POST /v1/billing/webhook` — Stripe-signed fulfillment events

The application targets Stripe API version `2026-07-29.dahlia` through
`stripe-go` v86.2.0.
