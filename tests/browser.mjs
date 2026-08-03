/**
 * Browser end-to-end: load every page in a real browser, prove the things that
 * only exist once JavaScript has run — syntax highlighting, the editor's colour
 * layer, and the sign-up / top-up buttons a page hands you when a run is
 * refused. Console errors fail the run: a page that throws on load is broken
 * even if it returned 200.
 *
 *   node tests/browser.mjs http://127.0.0.1:4398 [--shots DIR] [--cookie NAME=VALUE]
 *
 * Playwright is not a dependency of this repo; it is borrowed from ../app-site
 * when it is there, and the whole file skips (exit 0) when it is not, so CI on
 * a machine without a browser is not a red build.
 */
import { createRequire } from "node:module";
import { mkdirSync } from "node:fs";
import path from "node:path";

const base = (process.argv[2] || "http://127.0.0.1:4341").replace(/\/$/, "");
const shotsAt = process.argv.indexOf("--shots");
const shots = shotsAt > 0 ? process.argv[shotsAt + 1] : "";
const cookieAt = process.argv.indexOf("--cookie");
const cookie = cookieAt > 0 ? process.argv[cookieAt + 1] : "";

let chromium;
for (const from of [import.meta.url, "file:///nvme0n1-disk/code/app-site/package.json"]) {
  try {
    ({ chromium } = createRequire(from)("playwright"));
    break;
  } catch { /* try the next place */ }
}
if (!chromium) {
  console.log("browser e2e: playwright unavailable, skipping");
  process.exit(0);
}

const PAGES = ["/", "/editor", "/studio", "/apps", "/tools", "/pricing", "/docs", "/keys", "/usage", "/credits",
  "/cloud", "/data", "/vector", "/examples", "/agent", "/research"];
const failures = [];
const check = (ok, what) => { if (!ok) failures.push(what); };

// Research posts are discovered from the index rather than listed here, so a
// post added in Go without a link, or linked without a template, fails.
async function researchRoutes(context) {
  const page = await context.newPage();
  await page.goto(base + "/research", { waitUntil: "domcontentloaded" });
  const routes = await page.evaluate(() =>
    [...document.querySelectorAll("a.post")].map((a) => new URL(a.href).pathname));
  await page.close();
  return routes;
}

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin: base });
if (cookie) {
  const [name, ...rest] = cookie.split("=");
  const url = new URL(base);
  await context.addCookies([{ name, value: rest.join("="), domain: url.hostname, path: "/" }]);
}
if (shots) mkdirSync(shots, { recursive: true });

const found = await researchRoutes(context);
check(found.length > 0, "/research lists no posts");
const routes = [...PAGES, ...found];

for (const route of routes) {
  const page = await context.newPage();
  const errors = [];
  let studioAgentRequest = null;
  page.on("pageerror", (e) => errors.push(String(e)));
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });

  if (route === "/studio" && cookie) {
    await page.route("**/v1/agent/runs", async (request) => {
      studioAgentRequest = JSON.parse(request.request().postData() || "{}");
      const step = (event, data) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
      await request.fulfill({
        status: 200,
        contentType: "text/event-stream; charset=utf-8",
        headers: { "X-Run-ID": "studio-test-run" },
        body: step("run", { id: "studio-test-run", session: "studio-session", status: "running" }) +
          step("text", { kind: "text", text: '<file path="index.html">\n<!doctype html>\n<h1>Agent worked</h1>\n</file>' }) +
          step("usage", { kind: "usage", text: "1 req · in 80 · out 20", meta: {
            final: true, cost: { credits: 1, balance: 999 },
          } }),
      });
    });
  }

  const resp = await page.goto(base + route, { waitUntil: "networkidle" });
  check(resp && resp.status() === 200, `${route}: HTTP ${resp && resp.status()}`);
  const viewport = await page.evaluate(() => ({ width: innerWidth, scroll: document.documentElement.scrollWidth }));
  check(viewport.scroll <= viewport.width + 1,
    `${route}: horizontal overflow ${viewport.scroll}px in ${viewport.width}px viewport`);

  // Highlighting: every tagged block must have been tokenised, and the colour
  // must have actually been resolved by the stylesheet rather than inherited.
  const hl = await page.evaluate(() => {
    const blocks = [...document.querySelectorAll("pre[data-lang]")];
    const spans = blocks.flatMap((b) => [...b.querySelectorAll("span[class^=t-]")]);
    const colours = new Set(spans.map((s) => getComputedStyle(s).color));
    return { blocks: blocks.length, done: blocks.filter((b) => b.dataset.hl === "done").length,
      spans: spans.length, colours: colours.size, ink: getComputedStyle(document.body).color };
  });
  check(hl.blocks === hl.done, `${route}: ${hl.blocks - hl.done} code blocks not highlighted`);
  if (hl.blocks) {
    check(hl.spans > 0, `${route}: highlighted blocks produced no tokens`);
    check(hl.colours >= 3, `${route}: only ${hl.colours} token colours — the theme did not load`);
  }

  if (route === "/") {
    const contrast = await page.evaluate(() => {
      const styles = getComputedStyle(document.documentElement);
      const colour = (name) => styles.getPropertyValue(name).trim();
      const rgb = (value) => {
        if (value.startsWith("#")) {
          const raw = value.slice(1);
          return [0, 2, 4].map((at) => parseInt(raw.slice(at, at + 2), 16));
        }
        return (value.match(/[\d.]+/g) || []).slice(0, 3).map(Number);
      };
      const luminance = (value) => {
        const channels = rgb(value).map((part) => part / 255)
          .map((part) => part <= .04045 ? part / 12.92 : ((part + .055) / 1.055) ** 2.4);
        return .2126 * channels[0] + .7152 * channels[1] + .0722 * channels[2];
      };
      const ratio = (front, back) => {
        const a = luminance(front), b = luminance(back);
        return (Math.max(a, b) + .05) / (Math.min(a, b) + .05);
      };
      const bg = colour("--bg");
      return {
        ink: ratio(colour("--ink"), bg),
        dim: ratio(colour("--dim"), bg),
        accent: ratio(colour("--acid"), bg),
        button: ratio(colour("--acid-ink"), colour("--acid")),
        bg,
      };
    });
    check(contrast.ink >= 7, `/ theme: body contrast is ${contrast.ink.toFixed(2)}, want 7`);
    check(contrast.dim >= 4.5, `/ theme: secondary contrast is ${contrast.dim.toFixed(2)}, want 4.5`);
    check(contrast.accent >= 4.5, `/ theme: accent contrast is ${contrast.accent.toFixed(2)}, want 4.5`);
    check(contrast.button >= 4.5, `/ theme: button contrast is ${contrast.button.toFixed(2)}, want 4.5`);

    await page.hover("nav a[href='/studio']");
    const hover = await page.evaluate(() => {
      const link = document.querySelector("nav a[href='/studio']");
      return getComputedStyle(link).color;
    });
    check(hover !== "rgb(255, 255, 255)", "/ theme: navigation hover still turns white");
  }

  // Signing up and paying must be reachable from every page, whoever you are.
  const links = await page.evaluate(() =>
    [...document.querySelectorAll("a")].map((a) => a.getAttribute("href") || ""));
  const signedIn = await page.evaluate(() => !!document.querySelector("a.pill[href='/credits']"));
  check(links.some((h) => /\/auth\/signup/.test(h) || h === "/credits"),
    `${route}: no way to sign up or manage credits`);
  check(links.some((h) => h === "/credits"), `${route}: no link to credit management`);
  if (signedIn) check(links.some((h) => h === "/credits"), `${route}: signed in with no credit-management link`);

  if (route === "/editor") {
    const ed = await page.evaluate(() => {
      const ta = document.getElementById("code");
      const layer = document.querySelector(".hl-layer");
      const cs = ta && getComputedStyle(ta);
      const ls = layer && getComputedStyle(layer);
      return {
        layer: !!layer,
        tokens: layer ? layer.querySelectorAll("span[class^=t-]").length : 0,
        transparent: cs && cs.color === "rgba(0, 0, 0, 0)",
        caret: cs && cs.caretColor,
        // The two boxes have to lay text out identically or the caret drifts.
        aligned: !!(cs && ls && cs.fontSize === ls.fontSize && cs.lineHeight === ls.lineHeight &&
          cs.paddingLeft === ls.paddingLeft && cs.paddingTop === ls.paddingTop &&
          cs.fontFamily === ls.fontFamily),
      };
    });
    check(ed.layer, "/editor: no highlight layer behind the textarea");
    check(ed.tokens > 10, `/editor: layer has ${ed.tokens} tokens`);
    check(ed.transparent, "/editor: textarea text is painted over its own highlight");
    check(ed.aligned, "/editor: the highlight layer does not line up with the textarea");
    check(ed.caret && ed.caret !== "rgba(0, 0, 0, 0)", "/editor: no visible caret");

    // Typing has to keep the layer in step with the text.
    await page.fill("#code", "");
    await page.type("#code", "def f(x):\n    return x * 2  # note\n");
    const typed = await page.evaluate(() => {
      const layer = document.querySelector(".hl-layer");
      return { text: layer.textContent, kw: layer.querySelectorAll(".t-kw").length,
        com: layer.querySelectorAll(".t-com").length };
    });
    check(typed.text.includes("return x * 2"), "/editor: layer did not follow typing");
    check(typed.kw >= 2 && typed.com >= 1, "/editor: typed code was not re-highlighted");

    // Keyboard editing stays on the native textarea, but must cover the
    // essentials people expect from an IDE.
    await page.fill("#code", "alpha = 1\nbeta = 2");
    await page.evaluate(() => {
      const ta = document.getElementById("code");
      ta.setSelectionRange(0, ta.value.length);
      ta.focus();
    });
    await page.keyboard.press("Control+/");
    const commented = await page.inputValue("#code");
    check(commented === "# alpha = 1\n# beta = 2",
      `/editor: Ctrl-/ produced ${JSON.stringify(commented)}`);
    await page.keyboard.press("Tab");
    const indented = await page.inputValue("#code");
    check(indented === "    # alpha = 1\n    # beta = 2",
      "/editor: Tab did not indent every selected line");
    await page.keyboard.press("Shift+Tab");
    check((await page.inputValue("#code")) === commented,
      "/editor: Shift-Tab did not unindent the selection");
    await page.fill("#code", "");
    await page.focus("#code");
    await page.keyboard.type("(");
    const paired = await page.evaluate(() => {
      const ta = document.getElementById("code");
      return {
        value: ta.value,
        caret: ta.selectionStart,
        lines: document.querySelectorAll("#line-numbers span").length,
        current: document.querySelectorAll("#line-numbers span.active").length,
      };
    });
    check(paired.value === "()" && paired.caret === 1,
      "/editor: bracket auto-close did not leave the caret inside the pair");
    check(paired.lines === 1 && paired.current === 1,
      "/editor: line-number gutter lost its active line");

    // Saving is an update operation once a local document exists, and the
    // browser's normal save shortcut must never open a download dialog.
    await page.fill("#code", "value = 1\n");
    await page.click("#save-snippet");
    await page.fill("#code", "value = 2\n");
    await page.keyboard.press("Control+s");
    const saved = await page.evaluate(() => {
      const items = JSON.parse(localStorage.getItem("mojojojo.editor.snippets.v1") || "[]");
      return {
        count: items.length,
        code: items[0]?.code,
        state: document.getElementById("save-state").textContent,
        label: document.getElementById("save-label").textContent,
        selected: document.querySelectorAll(".file-item.active").length,
      };
    });
    check(saved.count === 1 && saved.code === "value = 2\n",
      `/editor: Ctrl-S created a duplicate or saved stale code (${JSON.stringify(saved)})`);
    check(saved.state === "updated locally" && /Update saved/.test(saved.label) && saved.selected === 1,
      `/editor: saved-document state is unclear (${JSON.stringify(saved)})`);

    // Pyodide normally arrives only when Run is clicked. A tiny module stub
    // makes the error deterministic while exercising the real run pipeline.
    await page.route("https://cdn.jsdelivr.net/pyodide/v0.28.3/full/pyodide.mjs", (request) =>
      request.fulfill({
        contentType: "application/javascript",
        headers: { "Access-Control-Allow-Origin": "*" },
        body: `export async function loadPyodide() { return {
          setStdout() {}, setStderr() {}, async loadPackage() {},
          async runPythonAsync() { throw new Error(
            'Traceback (most recent call last):\\n  File "<exec>", line 2\\n    if True print("x")\\n            ^\\nSyntaxError: invalid syntax'); }
        }; }`,
      }));
    await page.selectOption("#where", "browser");
    await page.fill("#code", "value = 1\nif True print('x')\n");
    await page.click("#run");
    await page.waitForFunction(() => document.getElementById("run-status").textContent === "Exited 1");
    const diagnostic = await page.evaluate(() => ({
      visible: !document.getElementById("editor-diagnostic").hidden,
      kind: document.getElementById("diagnostic-kind").textContent,
      message: document.getElementById("diagnostic-message").textContent,
      location: document.getElementById("diagnostic-location").textContent,
      errorLines: [...document.querySelectorAll("#line-numbers span.error")]
        .map((line) => line.textContent),
    }));
    check(diagnostic.visible && diagnostic.kind === "SyntaxError" &&
      diagnostic.location === "Ln 2" && diagnostic.errorLines.includes("2"),
    `/editor: syntax error did not mark line 2 (${JSON.stringify(diagnostic)})`);
    await page.click("#editor-diagnostic");
    check((await page.evaluate(() => document.getElementById("code").value.slice(
      document.getElementById("code").selectionStart,
      document.getElementById("code").selectionEnd))) === "if True print('x')",
    "/editor: diagnostic did not jump to the faulty source line");

    // A share link is a compressed URL fragment: opening it reproduces the
    // file and run settings without creating a server-side document.
    await page.click("#share-code");
    await page.waitForFunction(() => document.getElementById("share-code").textContent === "Copied");
    const sharedURL = await page.evaluate(() => navigator.clipboard.readText());
    const sharedTarget = new URL(sharedURL);
    check(sharedTarget.origin === new URL(base).origin && sharedTarget.pathname === "/editor" &&
      /^#[zr]=/.test(sharedTarget.hash), `/editor: invalid share URL ${sharedURL.slice(0, 80)}`);
    const sharedPage = await context.newPage();
    await sharedPage.goto(sharedURL, { waitUntil: "networkidle" });
    const shared = await sharedPage.evaluate(() => ({
      name: document.getElementById("document-name").textContent,
      code: document.getElementById("code").value,
      where: document.getElementById("where").value,
      toast: document.getElementById("editor-toast").textContent,
    }));
    check(shared.name === "rolling_sharpe.py" && shared.code.includes("if True print") &&
      shared.where === "browser" && /Shared snippet opened/.test(shared.toast),
    `/editor: shared file did not round-trip (${JSON.stringify(shared)})`);
    await sharedPage.close();

    const download = page.waitForEvent("download");
    await page.click("#download-code");
    check((await download).suggestedFilename() === "rolling_sharpe.py",
      "/editor: Python download did not preserve the filename");
  }

  if (route === "/studio") {
    const studio = await page.evaluate(() => ({
      files: document.querySelectorAll("#file-tree button").length,
      index: document.getElementById("source").value.includes("Streaming Snake"),
      indexLines: document.getElementById("source").value.split("\n").length,
      longestIndexLine: Math.max(...document.getElementById("source").value.split("\n").map((line) => line.length)),
      preview: !!document.getElementById("preview").srcdoc,
      functions: document.querySelectorAll("#function-name option").length,
      codemirror: !!document.querySelector(".CodeMirror"),
      lines: document.querySelectorAll(".CodeMirror-code>div").length,
      tokens: document.querySelectorAll(".CodeMirror-code span[class^='cm-']").length,
      gutters: document.querySelectorAll(".CodeMirror-linenumber").length,
      fileLeft: Math.round(document.querySelector("#file-tree button").getBoundingClientRect().left -
        document.getElementById("file-tree").getBoundingClientRect().left),
      zip: !!document.getElementById("download-project"),
      agentModel: document.getElementById("agent-model")?.value,
      previewClipboard: document.getElementById("preview").allow.includes("clipboard-write"),
      inviteFallback: document.getElementById("preview").srcdoc.includes("execCommand('copy')"),
      inviteHostURL: document.getElementById("preview").srcdoc.includes("hostURL:"),
    }));
    check(studio.files >= 5 && studio.index && studio.preview && studio.codemirror,
      `/studio: starter workspace is incomplete (${JSON.stringify(studio)})`);
    check(studio.lines >= 10 && studio.tokens >= 10 && studio.gutters >= 5,
      `/studio: syntax highlighting or line numbers missing (${JSON.stringify(studio)})`);
    check(studio.indexLines >= 35 && studio.longestIndexLine < 180,
      `/studio: index.html starter is unreadable (${JSON.stringify(studio)})`);
    check(studio.zip && studio.agentModel === "gpt-5.6-luna",
      `/studio: ZIP export or default agent model is missing (${JSON.stringify(studio)})`);
    check(studio.previewClipboard && studio.inviteFallback && studio.inviteHostURL,
      `/studio: invite copy cannot escape the preview sandbox (${JSON.stringify(studio)})`);
    const previewFrame = await (await page.$("#preview")).contentFrame();
    await previewFrame.evaluate(() => {
      eval("room='COPY42'");
      document.getElementById("game").hidden = false;
    });
    await previewFrame.click("#copy-invite");
    await previewFrame.waitForFunction(() => document.getElementById("copy-invite").textContent === "Invite copied");
    const snakeInvite = new URL(await page.evaluate(() => navigator.clipboard.readText()));
    check(snakeInvite.pathname === "/studio" && snakeInvite.searchParams.get("room") === "COPY42",
      `/studio: snake invite copied the wrong URL (${snakeInvite})`);
    check(studio.fileLeft <= 10,
      `/studio: file rows are not left aligned (${studio.fileLeft}px inset)`);
    await page.click("#file-tree button[title='app.css']");
    const scrolls = await page.evaluate(() => {
      const scroller = document.querySelector(".CodeMirror-scroll");
      return { vertical: scroller.scrollHeight > scroller.clientHeight,
        overflowY: getComputedStyle(scroller).overflowY };
    });
    check(scrolls.vertical && /auto|scroll/.test(scrolls.overflowY),
      `/studio: editor cannot scroll vertically (${JSON.stringify(scrolls)})`);
    await page.click("#toggle-agent");
    check(await page.isVisible("#agent-sidebar") &&
      await page.getAttribute("#toggle-agent", "aria-expanded") === "true",
    "/studio: agent sidebar did not open");
    if (cookie) {
      await page.fill("#agent-prompt", "Replace the page with a heading.");
      await page.click("#ask-agent");
      await page.waitForFunction(() => document.getElementById("agent-status").textContent.startsWith("Complete"));
      const agent = await page.evaluate(() => ({
        output: document.getElementById("agent-output").textContent,
        applyDisabled: document.getElementById("apply-agent").disabled,
      }));
      check(studioAgentRequest?.model === "gpt-5.6-luna" && studioAgentRequest?.effort === "low",
        `/studio: wrong default agent request (${JSON.stringify(studioAgentRequest)})`);
      check(/Agent worked/.test(agent.output) && /1 credit/.test(agent.output) && !agent.applyDisabled,
        `/studio: streamed agent answer or usage missing (${JSON.stringify(agent)})`);
      await page.click("#apply-agent");
      check((await page.locator(".CodeMirror").textContent()).includes("Agent worked"),
        "/studio: agent replacement was not applied to CodeMirror");
    }
    await page.click("#close-agent");
    check(!await page.isVisible("#agent-sidebar") &&
      await page.getAttribute("#toggle-agent", "aria-expanded") === "false",
    "/studio: agent sidebar did not close");
  }

  if (route === "/apps" || route === "/tools") {
    await page.waitForFunction(() => document.querySelectorAll(".app-card").length > 0);
    const cards = await page.locator(".app-card").count();
    check(cards >= 2, `${route}: only ${cards} featured examples`);
  }

  if (route === "/") {
    const hero = await page.locator(".hero h1").textContent();
    check(/Serverless Python/.test(hero), "/: value proposition does not lead with serverless Python");
    check(await page.isVisible("#open-editor"), "/: playground cannot be carried into the editor");
    const layout = await page.evaluate(() => ({
      mascot: document.querySelector(".hero-mascot")?.getBoundingClientRect().width || 0,
      code: document.querySelector("#code")?.getBoundingClientRect().height || 0,
      heroImages: document.querySelectorAll(".hero img").length,
    }));
    check(layout.mascot > 0 && layout.mascot <= 180,
      `/: desktop mascot is not compact (${layout.mascot}px)`);
    check(layout.code >= 300, `/: homepage code area is only ${layout.code}px tall`);
    check(layout.heroImages === 0, "/: hero still ships a large inline mascot image");
  }

  if (route === "/credits" && !signedIn) {
    const cta = await page.evaluate(() =>
      [...document.querySelectorAll("a.btn")].map((a) => a.textContent.trim()));
    check(cta.some((t) => /sign up/i.test(t)), "/credits: signed out with no sign-up button");
  }
  if (route === "/credits" && signedIn) {
    const body = await page.textContent("body");
    check(/Ledger/.test(body), "/credits: signed in without a ledger");
    check(!!(await page.$(".meter")), "/credits: no accrual meter");
  }

  if (route.startsWith("/research/")) {
    // A post has to actually be a post: a masthead, prose, and a way back.
    const post = await page.evaluate(() => ({
      body: !!document.querySelector("article.post-body"),
      h1: (document.querySelector("h1") || {}).textContent || "",
      words: (document.body.innerText || "").split(/\s+/).length,
      back: [...document.querySelectorAll("a")].some((a) => a.getAttribute("href") === "/research"),
      tags: document.querySelectorAll(".tag").length,
    }));
    check(post.body, `${route}: no article body`);
    check(post.h1.length > 8, `${route}: no title`);
    check(post.words > 400, `${route}: only ${post.words} words — is it rendering?`);
    check(post.back, `${route}: no link back to the index`);
    check(post.tags > 0, `${route}: untagged`);
  }

  check(errors.length === 0, `${route}: console errors — ${errors.slice(0, 2).join(" | ")}`);
  if (shots) {
    const stem = route === "/" ? "home" : route.slice(1).replace(/\//g, "-");
    await page.screenshot({
      path: path.join(shots, stem + (signedIn ? "-in" : "") + ".png"),
      fullPage: true,
    });
  }
  await page.close();
}

// The gallery is itself a review surface: comparisons should be linkable,
// filterable, and point at the current host rather than the capture machine.
{
  const page = await context.newPage();
  const response = await page.goto(base + "/visualbench", { waitUntil: "networkidle" });
  check(response?.status() === 200, `/visualbench: HTTP ${response?.status()}`);
  const gallery = await page.evaluate(() => ({
    pages: document.querySelectorAll(".page").length,
    options: document.querySelectorAll("#page-filter option").length,
    editorLive: document.querySelector("#editor .live")?.getAttribute("href"),
    editorLabel: document.querySelector("#editor figcaption b")?.textContent,
  }));
  check(gallery.pages >= 15 && gallery.options === gallery.pages + 1,
    `/visualbench: incomplete page filter (${JSON.stringify(gallery)})`);
  check(gallery.editorLive === "/editor" && /desktop/i.test(gallery.editorLabel || ""),
    `/visualbench: comparison links or labels are not portable (${JSON.stringify(gallery)})`);
  await page.selectOption("#page-filter", "editor");
  check((await page.locator(".page:visible").count()) === 1,
    "/visualbench: page filter did not reduce the gallery");
  await page.close();
}

// Conversion pages must fit an actual phone viewport. A crowded header once
// widened the document to 823px and made the content look half-size.
{
  const mobile = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true });
  for (const route of ["/", "/studio", "/apps", "/tools", "/pricing", "/cloud", "/data", "/vector", "/examples"]) {
    const page = await mobile.newPage();
    await page.goto(base + route, { waitUntil: "networkidle" });
    const size = await page.evaluate(() => ({ viewport: innerWidth,
      document: document.documentElement.scrollWidth }));
    check(size.document <= size.viewport + 1,
      `${route} mobile: horizontal overflow ${size.document}px in ${size.viewport}px viewport`);
    if (route === "/") {
      const layout = await page.evaluate(() => ({
        mascot: getComputedStyle(document.querySelector(".hero-mascot")).display,
        code: document.querySelector("#code").getBoundingClientRect().height,
      }));
      check(layout.mascot === "none", `/: mobile mascot is ${layout.mascot}, want hidden`);
      check(layout.code >= 320, `/: mobile code area is only ${layout.code}px tall`);
    }
    if (route === "/studio") {
      const toolbar = await page.locator(".studio-bar").evaluate((node) => ({
        client: node.clientWidth,
        scroll: node.scrollWidth,
        overflow: getComputedStyle(node).overflowX,
      }));
      check(toolbar.scroll <= toolbar.client + 1 && toolbar.overflow !== "auto" && toolbar.overflow !== "scroll",
        `/studio mobile: toolbar is horizontally scrollable (${JSON.stringify(toolbar)})`);
      check(!await page.locator("#save-state").isVisible(),
        "/studio mobile: internal save status should not appear as a subtitle");
    }
    await page.close();
  }
  await mobile.close();
}

// Agent panel: stub the same-origin SSE API in the browser so this exercises
// parsing, rendering, interruption and editor patch mirroring without spending
// model tokens. The form is present but normally hidden for this signed-out
// test context; the real Go proxy remains the authority on authentication.
{
  await context.addInitScript(() => {
    const nativeFetch = window.fetch.bind(window);
    const encoder = new TextEncoder();
    const event = (name, data) =>
      `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;
    window.__agentInterrupts = 0;
    window.__agentRuns = 0;
    window.fetch = (input, init = {}) => {
      const url = typeof input === "string" ? input : input.url;
      const method = String(init.method || "GET").toUpperCase();
      if (/\/v1\/agent\/runs\/[^/]+\/interrupt$/.test(url) && method === "POST") {
        window.__agentInterrupts += 1;
        return Promise.resolve(new Response('{"ok":true,"status":"interrupted"}', {
          status: 202,
          headers: { "Content-Type": "application/json" },
        }));
      }
      if (url.endsWith("/v1/agent/runs") && method === "POST") {
        window.__agentRuns += 1;
        const run = window.__agentRuns;
        const stream = new ReadableStream({
          start(controller) {
            controller.enqueue(encoder.encode(event("run", {
              id: `stub-run-${run}`, session: "stub-session", status: "running",
            })));
            if (run === 2) {
              init.signal?.addEventListener("abort", () => {
                controller.error(new DOMException("stopped", "AbortError"));
              }, { once: true });
              return;
            }
            const patch = [
              "*** Begin Patch",
              "*** Update File: rolling_sharpe.py",
              "@@",
              "-value = 1",
              "+value = 2",
              "*** End Patch",
            ].join("\n");
            for (const [name, data] of [
              ["reasoning", { kind: "reasoning", text: "I will inspect the value.", name: "", meta: {} }],
              ["tool_call", { kind: "tool_call", text: '{"query":"value"}', name: "search", meta: {} }],
              ["tool_result", { kind: "tool_result", text: "rolling_sharpe.py:1:value = 1",
                name: "search", meta: { ok: true, hits: 1 } }],
              ["tool_call", { kind: "tool_call", text: JSON.stringify({ input: patch }),
                name: "apply_patch", meta: {} }],
              ["tool_result", { kind: "tool_result", text: "rolling_sharpe.py: +1 -1",
                name: "apply_patch", meta: { ok: true, files: ["rolling_sharpe.py"] } }],
              ["text", { kind: "text", text: "Updated the value.", name: "", meta: {} }],
              ["usage", { kind: "usage", text: "1 req · in 80 · out 40 (reasoning 8)",
                name: "", meta: { final: true, tokens: 120,
                  usage: { input_tokens: 80, output_tokens: 40 },
                  cost: { credits: 1, owed_tokens: 20, balance: 9, tokens_per_credit: 100 } } }],
            ]) controller.enqueue(encoder.encode(event(name, data)));
            controller.close();
          },
        });
        return Promise.resolve(new Response(stream, {
          status: 200,
          headers: { "Content-Type": "text/event-stream", "X-Run-ID": `stub-run-${run}` },
        }));
      }
      return nativeFetch(input, init);
    };
  });

  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  await page.goto(base + "/editor", { waitUntil: "networkidle" });
  await page.click("#agent-tab");
  if (!cookie) {
    check(await page.isVisible("#agent-auth"),
      "/editor agent: signed-out account panel is not visible");
  }
  await page.evaluate(() => {
    document.getElementById("agent-auth").hidden = true;
    document.getElementById("agent-workspace").hidden = false;
  });
  await page.fill("#code", "value = 1\n");
  await page.fill("#agent-instruction", "Change the value to two.");
  await page.click("#agent-start");
  await page.waitForFunction(() =>
    document.getElementById("agent-status").textContent === "Complete");

  const agent = await page.evaluate(() => ({
    code: document.getElementById("code").value,
    file: document.getElementById("agent-file-status").textContent,
    usage: document.getElementById("agent-usage").textContent,
    cost: document.getElementById("cost-credits").textContent,
    reasoning: document.querySelector(".agent-reasoning")?.textContent || "",
    reasoningOpen: document.querySelector(".agent-reasoning")?.open,
    tools: [...document.querySelectorAll(".agent-tool-line")].map((item) => item.textContent),
    resultOpen: document.querySelector(".agent-tool-result")?.open,
    answer: document.querySelector(".agent-answer")?.textContent || "",
  }));
  check(agent.code === "value = 2\n",
    `/editor agent: open file was not patched, got ${JSON.stringify(agent.code)}`);
  check(/Updated rolling_sharpe\.py/.test(agent.file),
    `/editor agent: editor update was not announced (${agent.file})`);
  check(/120 tokens/.test(agent.usage) && /1 credit/.test(agent.cost),
    `/editor agent: usage/cost missing (${agent.usage}; ${agent.cost})`);
  check(/inspect the value/.test(agent.reasoning) && agent.reasoningOpen === false,
    "/editor agent: reasoning is not rendered as collapsed text");
  check(agent.tools.some((line) => line === 'search "value"') &&
    agent.tools.some((line) => /apply_patch 1 file \+1 -1/.test(line)),
  `/editor agent: compact tool lines missing (${JSON.stringify(agent.tools)})`);
  check(agent.resultOpen === false, "/editor agent: tool result is expanded by default");
  check(agent.answer === "Updated the value.", "/editor agent: final prose was not rendered");
  if (shots) {
    await page.screenshot({ path: path.join(shots, "editor-agent.png"), fullPage: true });
  }

  await page.fill("#agent-instruction", "Keep running until stopped.");
  await page.click("#agent-start");
  await page.waitForFunction(() =>
    document.getElementById("agent-status").textContent === "Running");
  await page.click("#agent-stop");
  await page.waitForFunction(() =>
    document.getElementById("agent-status").textContent === "Stopped");
  check((await page.evaluate(() => window.__agentInterrupts)) === 1,
    "/editor agent: Stop did not call the interrupt endpoint");
  check(errors.length === 0, `/editor agent: page errors — ${errors.join(" | ")}`);
  await page.close();
}

// The one flow that has to work for a stranger with no account: run something
// in the browser for free, without signing in, and be offered the account when
// the answer needs our machines.
{
  const page = await context.newPage();
  await page.goto(base + "/editor", { waitUntil: "networkidle" });
  await page.selectOption("#where", "server");
  await page.fill("#code", "print(1)");
  await page.click("#run");
  await page.waitForSelector("#cta a", { timeout: 20000 }).catch(() => {});
  const cta = await page.evaluate(() =>
    [...document.querySelectorAll("#cta a")].map((a) => `${a.textContent.trim()}|${a.getAttribute("href")}`));
  if (!cookie) {
    check(cta.some((t) => /sign up/i.test(t) && /signup/.test(t)),
      `/editor: a refused server run offered ${JSON.stringify(cta)} instead of a sign-up button`);
  }
  if (shots) await page.screenshot({ path: path.join(shots, "editor-refused.png"), fullPage: true });
  await page.close();
}

// The editor is deliberately a different page shell. Pin both ends of its
// responsive contract and capture an image in each: widescreen must use the
// monitor rather than inherit the site's narrow reading column, while mobile
// must become a real vertical stack with no draggable gutters left onscreen.
// Results follow the editor so running code never requires scrolling through
// the entire snippet library first; secondary workspace navigation comes last.
for (const view of [
  { name: "wide", width: 1920, height: 1080 },
  { name: "mobile", width: 390, height: 844 },
]) {
  const visualContext = await browser.newContext({
    viewport: { width: view.width, height: view.height },
    reducedMotion: "reduce",
  });
  if (cookie) {
    const [name, ...rest] = cookie.split("=");
    const url = new URL(base);
    await visualContext.addCookies([
      { name, value: rest.join("="), domain: url.hostname, path: "/" },
    ]);
  }
  const page = await visualContext.newPage();
  await page.goto(base + "/editor", { waitUntil: "networkidle" });
  const layout = await page.evaluate(() => {
    const box = (selector) => document.querySelector(selector)?.getBoundingClientRect();
    const body = getComputedStyle(document.body);
    const workspace = getComputedStyle(document.querySelector(".workspace"));
    const splitter = getComputedStyle(document.querySelector(".splitter"));
    return {
      bodyOverflow: body.overflow,
      workspaceDisplay: workspace.display,
      splitterDisplay: splitter.display,
      rail: box(".rail"),
      editor: box(".editor-pane"),
      results: box(".results"),
      workspace: box(".workspace"),
    };
  });
  if (view.name === "wide") {
    check(layout.workspace.width > 1800,
      `/editor wide: workspace is only ${Math.round(layout.workspace.width)}px wide`);
    check(layout.workspaceDisplay === "grid", "/editor wide: panes are not a grid");
    check(layout.splitterDisplay !== "none", "/editor wide: resize dividers are hidden");
    check(layout.rail.width >= 170 && layout.editor.width >= 600 && layout.results.width >= 300,
      `/editor wide: unusable pane widths ${Math.round(layout.rail.width)}/` +
      `${Math.round(layout.editor.width)}/${Math.round(layout.results.width)}`);
    check(layout.workspace.bottom <= view.height + 1,
      `/editor wide: workspace exceeds viewport by ${Math.round(layout.workspace.bottom - view.height)}px`);
  } else {
    check(layout.workspaceDisplay === "flex", "/editor mobile: workspace is not a stack");
    check(layout.splitterDisplay === "none", "/editor mobile: resize divider is still visible");
    check(Math.abs(layout.editor.width - view.width) < 2 &&
      Math.abs(layout.results.width - view.width) < 2,
    `/editor mobile: panes do not fill ${view.width}px viewport`);
    check(layout.editor.top < layout.results.top && layout.results.top < layout.rail.top,
      "/editor mobile: editor, results and account rail are not in task order");
    check(layout.bodyOverflow !== "hidden", "/editor mobile: body cannot scroll");
  }
  const screenshotOptions = shots
    ? { path: path.join(shots, `editor-${view.name}.png`), fullPage: view.name === "mobile" }
    : { fullPage: view.name === "mobile" };
  const image = await page.screenshot(screenshotOptions);
  check(image.length > 10_000, `/editor ${view.name}: screenshot is unexpectedly empty`);
  await visualContext.close();
}

await browser.close();

if (failures.length) {
  console.error("browser e2e FAILED:\n  " + failures.join("\n  "));
  process.exit(1);
}
console.log(`browser e2e OK — ${routes.length} pages (${found.length} research)`+ `${cookie ? " signed in" : ""}`);
