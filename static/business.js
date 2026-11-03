(() => {
  const root = document.querySelector('#biz');
  if (!root) return;
  const slug = root.dataset.slug;
  const stripeOn = root.dataset.stripe === '1';
  const pg = JSON.parse(root.dataset.pg || 'null');
  const acct = document.querySelector('#acct');
  const $ = (s, el = document) => el.querySelector(s);
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));

  async function api(path, body) {
    const r = await fetch(path, {
      method: body === undefined ? 'GET' : 'POST',
      headers: body === undefined ? {} : {'Content-Type': 'application/json'},
      credentials: 'same-origin',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    let data = {};
    try { data = await r.json(); } catch (_) { /* non-JSON error page */ }
    if (!r.ok) throw Object.assign(new Error(data.error || 'request failed (' + r.status + ')'), {status: r.status});
    return data;
  }

  let mode = 'signup';
  let state = null;

  function renderGuest(msg) {
    acct.innerHTML = `
      <div class="tabs" role="tablist">
        <button class="btn ${mode === 'signup' ? '' : 'ghost'}" role="tab" aria-selected="${mode === 'signup'}" data-mode="signup" type="button">Create account</button>
        <button class="btn ${mode === 'login' ? '' : 'ghost'}" role="tab" aria-selected="${mode === 'login'}" data-mode="login" type="button">Sign in</button>
      </div>
      <form id="auth-form" novalidate>
        <label class="dim">Email<input type="email" name="email" autocomplete="email" required></label>
        <label class="dim">Password<input type="password" name="password" autocomplete="${mode === 'signup' ? 'new-password' : 'current-password'}" minlength="6" required></label>
        <p class="err" id="auth-err" role="alert">${msg ? esc(msg) : ''}</p>
        <button class="btn" type="submit">${mode === 'signup' ? 'Sign up - $1 of free credits' : 'Sign in'}</button>
      </form>`;
    acct.querySelectorAll('[data-mode]').forEach((b) => b.addEventListener('click', () => { mode = b.dataset.mode; renderGuest(); }));
    $('#auth-form', acct).addEventListener('submit', async (e) => {
      e.preventDefault();
      const f = e.currentTarget;
      const btn = $('button[type=submit]', f);
      btn.disabled = true;
      try {
        state = await api('/_acct/' + mode, {email: f.email.value.trim(), password: f.password.value});
        render();
      } catch (err) {
        renderGuest(err.message);
      }
    });
  }

  function usageRows() {
    return (state.usage || []).map((u) => {
      const pct = u.free > 0 ? Math.min(100, (u.used / u.free) * 100) : 100;
      const left = Math.max(0, u.free - u.used);
      return `<div><span class="dim">${esc(u.label)}</span><div class="bar"><i style="width:${pct.toFixed(1)}%"></i></div>
        <span class="mono dim">${left.toLocaleString(undefined, {maximumFractionDigits: 3})} of ${u.free.toLocaleString()} free ${esc(u.unit)} left this month</span></div>`;
    }).join('');
  }

  function renderUser(extra) {
    const u = state.user;
    acct.innerHTML = `
      <div class="grid">
        <div><h3 class="acid" style="margin:0" id="bal">${u.credits.toLocaleString()}</h3><span class="dim">credits (${u.usd.toLocaleString(undefined, {style: 'currency', currency: 'USD'})})</span></div>
        <div><b>${esc(u.email)}</b><br><button class="btn ghost" id="logout" type="button">Sign out</button></div>
      </div>
      <div style="margin-top:14px">${usageRows()}</div>
      <div class="row" style="margin-top:10px">
        <button class="btn" id="mk-key" type="button">Create API key</button>
        <button class="btn ghost" data-buy="500" type="button" ${stripeOn ? '' : 'disabled'}>Add $5</button>
        <button class="btn ghost" data-buy="2000" type="button" ${stripeOn ? '' : 'disabled'}>Add $20</button>
        <button class="btn ghost" data-buy="5000" type="button" ${stripeOn ? '' : 'disabled'}>Add $50</button>
      </div>
      ${stripeOn ? '' : '<p class="dim">Card payments are not enabled on this deployment yet.</p>'}
      <div id="key-out">${extra || ''}</div>`;
    $('#logout', acct).addEventListener('click', async () => { await api('/_acct/logout', {}); state = {user: null}; render(); });
    $('#mk-key', acct).addEventListener('click', async () => {
      try {
        const k = await api('/_acct/key', {});
        $('#key-out', acct).innerHTML = `<p class="dim">Your key (shown once):</p><div class="key mono" id="key-val">${esc(k.key)}</div>
          <button class="btn ghost" id="copy-key" type="button">Copy</button>`;
        $('#copy-key', acct).addEventListener('click', () => navigator.clipboard && navigator.clipboard.writeText(k.key));
      } catch (err) { $('#key-out', acct).innerHTML = `<p class="err">${esc(err.message)}</p>`; }
    });
    acct.querySelectorAll('[data-buy]').forEach((b) => b.addEventListener('click', () => openCheckout(Number(b.dataset.buy))));
  }

  function render() {
    if (state && state.user) renderUser(); else renderGuest();
    playground();
  }

  // ---- Stripe embedded checkout, in a modal, without leaving the page
  const modal = $('#stripe-modal');
  let embedded = null;
  function closeCheckout() {
    if (embedded) { embedded.destroy(); embedded = null; }
    $('#stripe-checkout').replaceChildren();
    modal.hidden = true;
    modal.setAttribute('aria-hidden', 'true');
    document.body.style.overflow = '';
  }
  modal.querySelectorAll('[data-close]').forEach((el) => el.addEventListener('click', closeCheckout));
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !modal.hidden) closeCheckout(); });

  async function refreshBalance(untilChange) {
    const before = state.user.credits;
    for (let i = 0; i < 15; i++) {
      state = await api('/_acct/me');
      if (!untilChange || state.user.credits !== before) break;
      await new Promise((r) => setTimeout(r, 1500));
    }
    render();
  }

  async function openCheckout(cents) {
    const err = $('#stripe-error'), loading = $('#stripe-loading');
    err.hidden = true; loading.hidden = false; loading.textContent = 'Preparing checkout...';
    modal.hidden = false; modal.setAttribute('aria-hidden', 'false'); document.body.style.overflow = 'hidden';
    try {
      const c = await api('/_acct/checkout', {cents});
      if (!window.Stripe) throw new Error('Stripe.js did not load');
      const stripe = window.Stripe(c.publishable_key);
      embedded = await stripe.initEmbeddedCheckout({
        fetchClientSecret: async () => c.client_secret,
        onComplete: () => { closeCheckout(); acct.insertAdjacentHTML('afterbegin', '<p class="acid" role="status">Payment received - adding your credits...</p>'); refreshBalance(true); },
      });
      loading.hidden = true;
      embedded.mount('#stripe-checkout');
    } catch (e) {
      err.textContent = e.message; err.hidden = false; loading.hidden = true;
    }
  }

  // ---- Playground: runs a request through the same metered /v1 path with the session cookie
  let pgReady = false;
  function playground() {
    const box = $('#play');
    if (!pg || !state || !state.user) { box.hidden = true; return; }
    box.hidden = false;
    if (pgReady) return;
    pgReady = true;
    const body = $('#play-body'), out = $('#play-out'), status = $('#play-status');
    body.value = JSON.stringify(pg.body, null, 2);
    $('#play-note').textContent = 'POST ' + pg.path + ' - uses your credits.';
    if (pg.file) {
      $('#play-file-row').hidden = false;
      $('#play-file').accept = pg.file.accept;
      body.rows = 3;
    }
    $('#play-run').addEventListener('click', async () => {
      let payload;
      try { payload = JSON.parse(body.value || '{}'); } catch (_) { status.textContent = 'body is not valid JSON'; return; }
      if (pg.file) {
        const f = $('#play-file').files[0];
        if (!f) { status.textContent = pg.file.label; return; }
        const b64 = await new Promise((res, rej) => { const fr = new FileReader(); fr.onload = () => res(String(fr.result).split(',')[1]); fr.onerror = () => rej(fr.error); fr.readAsDataURL(f); });
        payload[pg.file.field] = pg.file.array ? [b64] : b64;
      }
      status.textContent = 'running...';
      const t0 = performance.now();
      const r = await fetch(pg.path, {method: 'POST', headers: {'Content-Type': 'application/json'}, credentials: 'same-origin', body: JSON.stringify(payload)});
      const text = await r.text();
      status.textContent = r.status + ' in ' + Math.round(performance.now() - t0) + ' ms';
      let shown = text;
      try { shown = JSON.stringify(JSON.parse(text), (k, v) => (Array.isArray(v) && v.length > 12 && typeof v[0] === 'number' ? v.slice(0, 8).concat(['... ' + (v.length - 8) + ' more']) : v), 2); } catch (_) { /* leave raw */ }
      out.textContent = shown;
      out.hidden = false;
      refreshBalance(false).catch(() => {});
    });
  }

  api('/_acct/me').then((s) => { state = s; render(); }).catch(() => { state = {user: null}; render(); });
})();
