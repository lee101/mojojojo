/**
 * Publish Studio's default app and prove that two real browsers negotiate a
 * WebRTC data channel through its Python room-broker function.
 *
 *   node tests/multiplayer.mjs BASE_URL mojojojo_session=TOKEN
 */
import { createRequire } from "node:module";

const base = (process.argv[2] || "http://127.0.0.1:4341").replace(/\/$/, "");
const cookie = process.argv[3] || "";
let chromium;
for (const from of [import.meta.url, "file:///nvme0n1-disk/code/app-site/package.json"]) {
  try {
    ({ chromium } = createRequire(from)("playwright"));
    break;
  } catch { /* try next */ }
}
if (!chromium) {
  console.log("multiplayer e2e: playwright unavailable, skipping");
  process.exit(0);
}
if (!cookie.includes("=")) throw new Error("session cookie is required");

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1000, height: 800 } });
await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin: base });
const [cookieName, ...cookieValue] = cookie.split("=");
const hostname = new URL(base).hostname;
await context.addCookies([{ name: cookieName, value: cookieValue.join("="), domain: hostname, path: "/" }]);

const studio = await context.newPage();
let projectID = "";
try {
  await studio.goto(base + "/studio", { waitUntil: "networkidle" });
  await studio.click("#save-project");
  await studio.waitForFunction(() => ["saved new version", "already saved"].includes(document.getElementById("save-state").textContent));
  projectID = await studio.inputValue("#project-picker");
  if (!projectID) throw new Error("Studio did not create a project");

  const downloadStarted = studio.waitForEvent("download");
  await studio.click("#download-project");
  const downloaded = await downloadStarted;
  if (!downloaded.suggestedFilename().endsWith(".zip")) {
    throw new Error(`Studio exported an unexpected filename: ${downloaded.suggestedFilename()}`);
  }

  await studio.click("#publish-project");
  await studio.waitForFunction(() => !document.getElementById("open-live").hidden);
  const livePath = await studio.getAttribute("#open-live", "href");
  if (!livePath?.includes(`/p/${projectID}/`)) throw new Error(`unexpected live URL: ${livePath}`);
  const live = new URL(livePath, base).href;

  const host = await context.newPage();
  const guest = await context.newPage();
  const errors = [];
  for (const page of [host, guest]) {
    page.on("pageerror", (error) => errors.push(String(error)));
    page.on("console", (message) => { if (message.type() === "error") errors.push(message.text()); });
    await page.goto(live, { waitUntil: "networkidle" });
    if (!await page.locator("#lobby").isVisible() || await page.locator("#game").isVisible()) {
      throw new Error("published app must show only the lobby before joining a room");
    }
  }

  await host.fill("#player-name", "Host snake");
  await host.click("#create-room");
  await host.waitForFunction(() => !document.getElementById("game").hidden);
  if (await host.locator("#lobby").isVisible() || !await host.locator("#game").isVisible()) {
    throw new Error("host must transition from the lobby to the game");
  }
  const room = (await host.textContent("#room-label")).trim();
  if (!/^[A-Z0-9]{6}$/.test(room)) throw new Error(`invalid room code ${room}`);

  await host.click("#copy-invite");
  await host.waitForFunction(() => document.getElementById("copy-invite").textContent === "Invite copied");
  const invite = await host.evaluate(() => navigator.clipboard.readText());
  const inviteURL = new URL(invite);
  if (inviteURL.searchParams.get("room") !== room || !inviteURL.pathname.includes(`/p/${projectID}/`)) {
    throw new Error(`copied invite is not the published room URL: ${invite}`);
  }

  await guest.goto(invite, { waitUntil: "networkidle" });
  await guest.fill("#player-name", "Guest snake");
  if (await guest.inputValue("#room-code") !== room) throw new Error("invite did not prefill the room code");
  await guest.click("#join-room");
  await guest.waitForFunction(() => !document.getElementById("game").hidden);
  if (await guest.locator("#lobby").isVisible() || !await guest.locator("#game").isVisible()) {
    throw new Error("guest must transition from the lobby to the game");
  }

  for (const page of [host, guest]) {
    await page.waitForFunction(() => document.getElementById("network").textContent === "P2P live",
      null, { timeout: 45000 });
  }
  const hostPlayers = await host.textContent("#players");
  const guestPlayers = await guest.textContent("#players");
  if (!/Guest snake.*live/.test(hostPlayers) || !/Host snake.*live/.test(guestPlayers)) {
    throw new Error(`rosters did not converge: host=${hostPlayers} guest=${guestPlayers}`);
  }
  await host.keyboard.press("ArrowDown");
  await host.waitForTimeout(500);
  if (errors.length) throw new Error(`page errors: ${errors.join(" | ")}`);
  console.log(`multiplayer e2e OK — room ${room}, WebRTC live in both browsers`);
} finally {
  if (projectID) {
    await studio.evaluate(async (id) => {
      await fetch(`/v1/projects/${id}`, { method: "DELETE" });
    }, projectID).catch(() => {});
  }
  await browser.close();
}
