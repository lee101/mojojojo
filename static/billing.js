(() => {
  const modal = document.querySelector('#stripe-modal');
  if (!modal) return;

  const checkoutNode = document.querySelector('#stripe-checkout');
  const loading = document.querySelector('#stripe-loading');
  const errorNode = document.querySelector('#stripe-error');
  const title = document.querySelector('#stripe-modal-title');
  let embeddedCheckout = null;
  let openingCheckout = false;

  const labels = {
    credits: 'Add prepaid credits',
    developer_monthly: 'Developer · monthly',
    developer_annual: 'Developer · annual',
    dedicated_monthly: 'Dedicated RTX 5090 · monthly',
    dedicated_annual: 'Dedicated RTX 5090 · annual',
  };

  function showError(message) {
    errorNode.textContent = message;
    errorNode.hidden = false;
    loading.hidden = true;
  }

  async function closeCheckout() {
    if (embeddedCheckout) {
      embeddedCheckout.destroy();
      embeddedCheckout = null;
    }
    checkoutNode.replaceChildren();
    modal.hidden = true;
    modal.setAttribute('aria-hidden', 'true');
    document.body.style.overflow = '';
  }

  async function openCheckout(offer, quantity) {
    if (openingCheckout) return;
    openingCheckout = true;
    errorNode.hidden = true;
    loading.hidden = false;
    loading.textContent = 'Preparing checkout…';
    title.textContent = labels[offer] || 'Complete payment';
    modal.hidden = false;
    modal.setAttribute('aria-hidden', 'false');
    document.body.style.overflow = 'hidden';

    try {
      const response = await fetch('/v1/billing/checkout', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({offer, quantity: Number(quantity || 1)}),
      });
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || 'Checkout could not be started');
      if (!window.Stripe) throw new Error('Stripe.js did not load');

      const stripe = window.Stripe(body.publishable_key);
      embeddedCheckout = await stripe.initEmbeddedCheckout({
        fetchClientSecret: async () => body.client_secret,
      });
      loading.hidden = true;
      embeddedCheckout.mount('#stripe-checkout');
    } catch (error) {
      showError(error instanceof Error ? error.message : 'Checkout could not be started');
    } finally {
      openingCheckout = false;
    }
  }

  document.querySelectorAll('[data-stripe-offer]').forEach((button) => {
    button.addEventListener('click', () => openCheckout(button.dataset.stripeOffer, button.dataset.stripeQuantity));
  });

  // Site-wide Add credits links land on /credits?buy=credits. Once this page
  // is already open, keep the interaction in place instead of reloading it.
  document.querySelectorAll('a[href]').forEach((link) => {
    const target = new URL(link.href, window.location.href);
    if (target.origin === window.location.origin && target.pathname === '/credits' && target.searchParams.get('buy') === 'credits') {
      link.addEventListener('click', (event) => {
        event.preventDefault();
        openCheckout('credits', 500);
      });
    }
  });
  document.querySelectorAll('[data-stripe-close]').forEach((button) => button.addEventListener('click', closeCheckout));
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !modal.hidden) closeCheckout();
  });

  const portal = document.querySelector('#stripe-portal');
  portal?.addEventListener('click', async () => {
    portal.disabled = true;
    try {
      const response = await fetch('/v1/billing/portal', {method: 'POST'});
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || 'Billing portal could not be opened');
      window.location.assign(body.url);
    } catch (error) {
      portal.disabled = false;
      window.alert(error instanceof Error ? error.message : 'Billing portal could not be opened');
    }
  });

  const params = new URLSearchParams(window.location.search);
  if (params.get('buy') === 'credits') {
    params.delete('buy');
    const query = params.toString();
    window.history.replaceState({}, '', `${window.location.pathname}${query ? `?${query}` : ''}${window.location.hash}`);
    openCheckout('credits', 500);
  }
  const sessionID = params.get('session_id');
  const returnBanner = document.querySelector('#stripe-return');
  if (params.get('checkout') === 'return' && sessionID && returnBanner) {
    returnBanner.hidden = false;
    returnBanner.textContent = 'Confirming your Stripe payment…';
    fetch(`/v1/billing/session?session_id=${encodeURIComponent(sessionID)}`)
      .then(async (response) => ({ok: response.ok, body: await response.json()}))
      .then(({ok, body}) => {
        if (!ok) throw new Error(body.error || 'Payment confirmation is pending');
        returnBanner.textContent = body.payment_status === 'paid'
          ? 'Payment confirmed. Your balance has been updated.'
          : 'Checkout returned. Stripe is still confirming the payment method.';
        if (body.payment_status === 'paid') {
          window.setTimeout(() => window.location.assign('/credits'), 900);
        }
      })
      .catch((error) => { returnBanner.textContent = error.message; });
  }
})();
