/**
 * Browser end-to-end for a business landing page: inline sign-up and sign-in
 * with no page load, API key creation, the playground running a metered call
 * with the session cookie, the embedded-Stripe modal flow (Stripe.js is stubbed
 * and /_acct/checkout answered by the test), and sign-out.
 *
 *   node tests/business_browser.mjs http://embed.mojojojo.test:4398
 *
 * Driven by tests/business_e2e.sh, which supplies the server, a fake upstream
 * and a host-resolver rule for *.mojojojo.test. Playwright is borrowed from
 * ../app-site; without it the file skips (exit 0).
 */
import { createRequire } from "node:module";

const base = (process.argv[2] || "").replace(/\/$/, "");
if (!base) { console.error("usage: business_browser.mjs URL"); process.exit(2); }
let chromium;
for (const from of [import.meta.url, "file:///nvme0n1-disk/code/app-site/package.json"]) {
  try { ({ chromium } = createRequire(from)("playwright")); break; } catch { /* next */ }
}
if (!chromium) { console.log("skip: playwright not available"); process.exit(0); }

const port = new URL(base).port;
const browser = await chromium.launch({ args: [`--host-resolver-rules=MAP *.mojojojo.test 127.0.0.1`] });
const ctx = await browser.newContext();
const page = await ctx.newPage();
const errors = [];
// the deliberate wrong password makes the browser log one 401; anything else is a bug
page.on("console", (m) => { if (m.type() === "error" && !/status of 401/.test(m.text())) errors.push(m.text()); });
page.on("pageerror", (e) => errors.push(String(e)));

// Stripe.js is a third-party script: stub it so the page's own wiring is what is tested.
await ctx.route("https://js.stripe.com/**", (r) => r.fulfill({ contentType: "text/javascript", body: `
  window.Stripe = (pk) => ({ initEmbeddedCheckout: async (o) => {
    window.__stripe = { pk, secret: await o.fetchClientSecret() };
    return { mount: (sel) => { document.querySelector(sel).innerHTML = '<div id="fake-stripe">card form</div>'; window.__complete = o.onComplete; }, destroy: () => { window.__destroyed = true; } };
  } });` }));
let checkoutBody = null;
await page.route("**/_acct/checkout", async (r) => {
  checkoutBody = JSON.parse(r.request().postData() || "{}");
  await r.fulfill({ contentType: "application/json", body: JSON.stringify({ ok: true, client_secret: "cs_test_secret", publishable_key: "pk_test_fake", session_id: "cs_1" }) });
});

let failed = 0;
const check = (name, ok, detail = "") => { console.log((ok ? "ok   " : "FAIL ") + name + (ok ? "" : " " + detail)); if (!ok) failed++; };
const email = `e2e-${Date.now()}@example.com`;

await page.goto(base + "/");
await page.evaluate(() => { window.__marker = "same-document"; });
check("landing renders the business", (await page.textContent("h1")).includes("Embeddings"));
await page.waitForSelector("#auth-form");
check("guest sees sign-up form", (await page.textContent("#acct")).includes("Create account"));

// a wrong sign-in is reported inline
await page.click('[data-mode="login"]');
await page.fill('input[name=email]', email);
await page.fill('input[name=password]', "nope-nope");
await page.click("button[type=submit]");
await page.waitForSelector("#auth-err:not(:empty)");
const badMsg = await page.textContent("#auth-err");
check("bad sign-in shows an inline error", badMsg.toLowerCase().includes("incorrect"), JSON.stringify(badMsg));

// sign up without a navigation
await page.click('[data-mode="signup"]');
await page.fill('input[name=email]', email);
await page.fill('input[name=password]', "correct horse battery");
await page.click("button[type=submit]");
await page.waitForSelector("#logout, #auth-err:not(:empty)");
check("sign-up succeeded", await page.isVisible("#logout"), await page.textContent("#acct"));
check("sign-up happened in place", (await page.evaluate(() => window.__marker)) === "same-document");
check("account panel shows the email", (await page.textContent("#acct")).includes(email));
check("free allowance is shown", (await page.textContent("#acct")).includes("free"));
check("playground appears once signed in", await page.isVisible("#play"));

// playground: a real metered call with only the cookie
await page.click("#play-run");
await page.waitForFunction(() => !document.querySelector("#play-out").hidden);
check("playground call succeeded", (await page.textContent("#play-status")).startsWith("200"), await page.textContent("#play-status"));
check("playground shows the response", (await page.textContent("#play-out")).includes("embedding"));

// API key
await page.click("#mk-key");
await page.waitForSelector("#key-val");
const key = await page.textContent("#key-val");
check("key has the mojojojo prefix", key.startsWith("mj_live_"));
const viaKey = await page.evaluate(async (k) => {
  const r = await fetch("/v1/embeddings", { method: "POST", headers: { Authorization: "Bearer " + k, "Content-Type": "application/json" }, body: JSON.stringify({ input: "x" }) });
  return r.status;
}, key);
check("the new key authenticates a call", viaKey === 200, String(viaKey));

// Stripe embedded checkout in a modal
await page.click('[data-buy="500"]');
await page.waitForSelector("#fake-stripe");
check("checkout modal is open", await page.isVisible("#stripe-modal"));
check("checkout asked for $5", checkoutBody && checkoutBody.cents === 500, JSON.stringify(checkoutBody));
check("stripe was initialised with the publishable key", (await page.evaluate(() => window.__stripe)).pk === "pk_test_fake");
await page.evaluate(() => window.__complete());
await page.waitForSelector("#stripe-modal", { state: "hidden" });
check("modal closes on completion and the checkout is destroyed", await page.evaluate(() => window.__destroyed === true));
check("the page says the payment arrived", (await page.textContent("#acct")).includes("Payment received"));
await page.keyboard.press("Escape");

// the session persists across a reload, and sign-out clears it
await page.reload();
await page.waitForSelector("#logout");
check("session survives reload", (await page.textContent("#acct")).includes(email));
await page.click("#logout");
await page.waitForSelector("#auth-form");
check("sign-out returns to the guest form", true);
await page.reload();
await page.waitForSelector("#auth-form");
check("sign-out persisted", !(await page.textContent("#acct")).includes(email));

// sign back in with the password
await page.click('[data-mode="login"]');
await page.fill('input[name=email]', email);
await page.fill('input[name=password]', "correct horse battery");
await page.click("button[type=submit]");
await page.waitForSelector("#logout");
check("sign-in works", (await page.textContent("#acct")).includes(email));

// agent discovery documents are reachable from the same page
for (const p of ["/openapi.json", "/llms.txt", "/.well-known/mcp.json"]) {
  const s = await page.evaluate(async (u) => (await fetch(u)).status, p);
  check("discovery " + p, s === 200, String(s));
}
check("no console errors", errors.length === 0, errors.join(" | "));
await browser.close();
console.log(failed ? `${failed} failed` : "all passed");
process.exit(failed ? 1 : 0);
