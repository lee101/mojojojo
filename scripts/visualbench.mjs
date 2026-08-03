/**
 * VisualBench: every page mojojojo serves, captured at two viewports, written
 * as webp with a thumbnail and an index that shows them side by side.
 *
 *   node scripts/visualbench.mjs [baseURL]
 *   VISUALBENCH_URL=https://mojojojo.cc node scripts/visualbench.mjs
 *   VISUALBENCH_TARGETS=home-desktop,editor-desktop node scripts/visualbench.mjs
 *   VISUALBENCH_COOKIE=mojojojo_session=... node scripts/visualbench.mjs
 *
 * Playwright and sharp are not dependencies of this repo; they are borrowed
 * from ../app-site the same way tests/browser.mjs borrows them, and the whole
 * script skips (exit 0) when they are absent so a machine with no browser is
 * not a red build.
 *
 * The output lands in visualbench/ and is copied to static/visualbench/, which
 * is what the /visualbench route serves.
 */
import { createRequire } from "node:module";
import { mkdir, writeFile, rm, copyFile, readdir } from "node:fs/promises";
import { resolve, join } from "node:path";

const base = (process.argv[2] || process.env.VISUALBENCH_URL || "http://127.0.0.1:4341").replace(/\/$/, "");
const rootDir = resolve(process.env.VISUALBENCH_OUT || "visualbench");
const publicDir = resolve(process.env.VISUALBENCH_PUBLIC || "static/visualbench");
const only = new Set((process.env.VISUALBENCH_TARGETS || "").split(",").map((s) => s.trim()).filter(Boolean));
const cookie = process.env.VISUALBENCH_COOKIE || "";

// webp at 16k+ px is where sharp starts squashing a tall page into a blurry
// mess, so a long page is clipped rather than scaled.
const webpQuality = 85;
const thumbQuality = 75;
const thumbWidth = 720;
const fullMaxHeightCssPx = 4200;

let chromium, sharp;
{
  const roots = [import.meta.url, "file:///nvme0n1-disk/code/app-site/package.json"];
  for (const from of roots) {
    try {
      const require = createRequire(from);
      chromium = require("playwright").chromium;
      sharp = require("sharp");
      break;
    } catch {
      // try the next root
    }
  }
  if (!chromium || !sharp) {
    console.log("visualbench: playwright/sharp unavailable, skipping");
    process.exit(0);
  }
}

const desktop = { width: 1440, deviceScaleFactor: 1, isMobile: false };
const mobile = { width: 390, height: 844, deviceScaleFactor: 2, isMobile: true };

// Every page the router serves that a person can actually look at. The research
// posts are the long ones; they are what fullMaxHeightCssPx exists for.
const pages = [
  { key: "home", label: "Home", path: "/", height: 1200 },
  { key: "editor", label: "Editor", path: "/editor", height: 900 },
  { key: "studio", label: "Studio", path: "/studio", height: 900 },
  { key: "studio-agent", label: "Studio Agent", path: "/studio", height: 900, setup: "agent", viewportOnly: true },
  { key: "apps", label: "Apps showcase", path: "/apps", height: 1200 },
  { key: "tools", label: "Tools showcase", path: "/tools", height: 1200 },
  { key: "docs", label: "Docs", path: "/docs", height: 1400 },
  { key: "agent", label: "Coding agent", path: "/agent", height: 1400 },
  { key: "pricing", label: "Pricing", path: "/pricing", height: 1400 },
  { key: "cloud", label: "Cloud", path: "/cloud", height: 1400 },
  { key: "vector", label: "Vector search", path: "/vector", height: 1400 },
  { key: "examples", label: "Examples", path: "/examples", height: 1600 },
  { key: "credits", label: "Credits", path: "/credits", height: 1000 },
  { key: "keys", label: "API keys", path: "/keys", height: 900 },
  { key: "usage", label: "Usage", path: "/usage", height: 900 },
  { key: "research", label: "Research index", path: "/research", height: 1200 },
  { key: "notfound", label: "404", path: "/no-such-page", height: 700 },
];

const targets = [];
for (const page of pages) {
  targets.push({ ...page, ...desktop, name: `${page.key}-desktop`, label: `${page.label} — desktop` });
  targets.push({ ...page, ...mobile,
    name: `${page.key}-mobile`, label: `${page.label} — mobile` });
}

await mkdir(rootDir, { recursive: true });
await mkdir(publicDir, { recursive: true });

const browser = await chromium.launch();
const shots = [];
const failures = [];

for (const target of targets) {
  if (only.size && !only.has(target.name)) continue;
  const context = await browser.newContext({
    viewport: { width: target.width, height: target.height },
    deviceScaleFactor: target.deviceScaleFactor,
    isMobile: target.isMobile,
    hasTouch: target.isMobile,
  });
  if (cookie) {
    const [name, ...rest] = cookie.split("=");
    const url = new URL(base);
    await context.addCookies([
      { name, value: rest.join("="), domain: url.hostname, path: "/" },
    ]);
  }
  const page = await context.newPage();
  const consoleErrors = [];
  page.on("pageerror", (err) => consoleErrors.push(String(err)));

  try {
    const response = await page.goto(base + target.path, { waitUntil: "networkidle", timeout: 45000 });
    const status = response ? response.status() : 0;
    // /no-such-page is meant to be a 404; everything else is not.
    const wantStatus = target.key === "notfound" ? 404 : 200;
    if (status !== wantStatus) {
      failures.push(`${target.name}: HTTP ${status}, want ${wantStatus}`);
    }
    await page.evaluate(() => document.fonts && document.fonts.ready);
    if (target.setup === "agent") {
      await page.click("#toggle-agent");
      await page.waitForFunction(() => !document.getElementById("agent-sidebar").hidden);
      await page.evaluate(() => document.activeElement?.blur());
    }
    // Freeze anything that would make two runs of the same page differ.
    await page.addStyleTag({
      content: `*,*::before,*::after{animation:none!important;transition:none!important;
                caret-color:transparent!important}`,
    });
    await page.waitForTimeout(250);

    const overflow = await page.evaluate(() => ({
      document: document.documentElement.scrollWidth - innerWidth,
      studioToolbar: document.querySelector(".studio-bar")
        ? document.querySelector(".studio-bar").scrollWidth - document.querySelector(".studio-bar").clientWidth
        : 0,
    }));
    if (overflow.document > 1) failures.push(`${target.name}: horizontal page overflow by ${overflow.document}px`);
    if (overflow.studioToolbar > 1) failures.push(`${target.name}: Studio toolbar overflow by ${overflow.studioToolbar}px`);

    const raw = await page.screenshot({
      fullPage: !target.viewportOnly,
      animations: "disabled",
      clip: undefined,
    });

    const image = sharp(raw);
    const meta = await image.metadata();
    const maxPx = fullMaxHeightCssPx * target.deviceScaleFactor;
    const pipeline = meta.height > maxPx
      ? sharp(raw).extract({ left: 0, top: 0, width: meta.width, height: maxPx })
      : sharp(raw);

    const file = `${target.name}.webp`;
    const thumb = `${target.name}.thumb.webp`;
    const buf = await pipeline.webp({ quality: webpQuality }).toBuffer();
    await writeFile(join(rootDir, file), buf);
    await sharp(buf).resize({ width: thumbWidth, withoutEnlargement: true })
      .webp({ quality: thumbQuality }).toBuffer()
      .then((t) => writeFile(join(rootDir, thumb), t));

    shots.push({
      ...target,
      file,
      thumb,
      bytes: buf.length,
      clipped: meta.height > maxPx,
      errors: consoleErrors,
    });
    if (consoleErrors.length) {
      failures.push(`${target.name}: ${consoleErrors.length} page error(s): ${consoleErrors[0]}`);
    }
    console.log(`  ${target.name} ${(buf.length / 1024).toFixed(0)}kb${meta.height > maxPx ? " (clipped)" : ""}`);
  } catch (err) {
    failures.push(`${target.name}: ${err.message}`);
    console.log(`  ${target.name} FAILED: ${err.message}`);
  } finally {
    await context.close();
  }
}
await browser.close();

const capturedAt = new Date().toISOString();
const cacheKey = capturedAt.replace(/\D/g, "").slice(0, 14);
await writeFile(join(rootDir, "index.html"), gallery(shots, base, failures, cacheKey));
await writeFile(join(rootDir, "manifest.json"), JSON.stringify({
  base, capturedAt, failures,
  shots: shots.map(({ name, label, path, file, thumb, bytes, clipped }) =>
    ({ name, label, path, file, thumb, bytes, clipped })),
}, null, 2));

// Publish: the /visualbench route serves static/visualbench straight off disk.
await rm(publicDir, { recursive: true, force: true });
await mkdir(publicDir, { recursive: true });
for (const entry of await readdir(rootDir)) {
  await copyFile(join(rootDir, entry), join(publicDir, entry));
}

console.log(`\nvisualbench: ${shots.length} shot(s) -> ${rootDir}`);
if (failures.length) {
  console.error(`\n${failures.length} problem(s):`);
  for (const f of failures) console.error("  " + f);
  process.exit(1);
}

function gallery(shots, base, failures, cacheKey) {
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const groups = new Map();
  for (const shot of shots) {
    if (!groups.has(shot.key)) groups.set(shot.key, []);
    groups.get(shot.key).push(shot);
  }
  const entries = [...groups.entries()];
  const options = entries.map(([key, items]) =>
    `<option value="${esc(key)}">${esc(items[0].label.split(" — ")[0])}</option>`).join("");
  const sections = entries.map(([key, items]) => `
  <article class="page" id="${esc(key)}" data-page="${esc(key)}">
    <div class="page-head">
      <h2><a class="permalink" href="#${esc(key)}" aria-label="Link to this comparison">#</a>
        ${esc(items[0].label.split(" — ")[0])}</h2>
      <a class="live" href="${esc(items[0].path)}">Open live ${esc(items[0].path)} ↗</a>
    </div>
    <div class="shots">
      ${items.map((s) => `
      <figure class="${s.name.endsWith("mobile") ? "mobile" : "desktop"}">
        <a class="frame" href="${esc(s.file)}?v=${cacheKey}" target="_blank">
          <img src="${esc(s.thumb)}?v=${cacheKey}" alt="${esc(s.label)}">
        </a>
        <figcaption><b>${esc(s.label.split(" — ")[1] || "capture")}</b>
          <span>${s.width}px viewport · ${(s.bytes / 1024).toFixed(0)}kb${s.clipped ? " · clipped" : ""}</span>
        </figcaption>
      </figure>`).join("")}
    </div>
  </article>`).join("");

  return `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<title>mojojojo visualbench</title>
<style>
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;padding:0 24px 64px;background:#121019;color:#d8d2df;
  font:15px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
.wrap{max-width:1380px;margin:0 auto}
.top{z-index:10;position:sticky;top:0;display:flex;align-items:center;justify-content:space-between;
  gap:24px;margin:0 -24px 28px;padding:18px max(24px,calc((100vw - 1380px)/2));
  border-bottom:1px solid #3a3448;background:rgba(18,16,25,.94);backdrop-filter:blur(12px)}
h1{font-size:21px;margin:0 0 2px;letter-spacing:-.01em}
p.meta{color:#9f97ad;font-size:12px;margin:0}
p.meta code{color:#98d8aa}.pass{color:#98d8aa}
.controls{display:flex;gap:9px}.controls label{display:grid;gap:2px;color:#91879f;font-size:9px;
  font-weight:700;letter-spacing:.07em;text-transform:uppercase}
select{min-width:150px;height:32px;padding:0 28px 0 9px;border:1px solid #493f55;
  border-radius:7px;color:#d8d2df;background:#211d2e;font:12px inherit}
.page{margin:0 0 32px;padding:14px;border:1px solid #3a3448;border-radius:13px;background:#17141f;
  scroll-margin-top:100px}
.page[hidden]{display:none}.page-head{display:flex;align-items:center;justify-content:space-between;
  gap:14px;margin:0 0 12px}
h2{font-size:15px;font-weight:700;margin:0}.permalink{margin-right:5px;color:#62586f;text-decoration:none}
.page:hover .permalink{color:#98d8aa}.live{color:#98d8aa;font-size:12px;text-decoration:none}
.live:hover{text-decoration:underline}
.shots{display:flex;gap:14px;align-items:flex-start}.shots figure{min-width:0;margin:0}
figure.desktop{flex:3 1 0}figure.mobile{flex:1 1 0;max-width:390px}
.frame{display:block;height:520px;overflow:auto;border:1px solid #3a3448;border-radius:9px;
  background:#0e0d15}.frame img{display:block;width:100%;height:auto}
figcaption{display:flex;justify-content:space-between;gap:8px;padding:7px 2px 0;color:#9f97ad;
  font-size:11px;text-transform:capitalize}figcaption b{color:#c9c1d2}figcaption span{color:#91879f;
  font-weight:400;text-transform:none}
body[data-layout="compact"] main{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:16px}
body[data-layout="compact"] .page{margin:0}body[data-layout="compact"] .frame{height:300px}
body[data-layout="compact"] figcaption span{display:none}
.fail{background:#2a1416;border:1px solid #5b2126;border-radius:10px;padding:14px 16px;margin:0 0 28px}
.fail h3{margin:0 0 8px;font-size:14px;color:#fca5a5}
.fail ul{margin:0;padding-left:18px;color:#e6a4a4;font-size:13px}
@media(max-width:760px){body{padding-inline:12px}.top{position:static;align-items:flex-start;flex-direction:column;
  margin-inline:-12px;padding:14px 12px}.controls{width:100%}.controls label{flex:1}.controls select{width:100%;min-width:0}
  .shots{flex-direction:column}.shots figure,.shots figure.mobile{width:100%;max-width:none}.frame{height:430px}
  body[data-layout="compact"] main{grid-template-columns:1fr}.page-head{align-items:flex-start}}
</style></head><body data-layout="compact"><div class="wrap">
<header class="top"><div><h1>mojojojo visualbench</h1>
<p class="meta">${shots.length} captures · <code>${esc(new URL(base).host)}</code> ·
  ${failures.length ? `${failures.length} problem(s)` : '<span class="pass">all pages clean</span>'}</p></div>
<div class="controls"><label>Page<select id="page-filter"><option value="all">All pages</option>${options}</select></label>
<label>Layout<select id="layout"><option value="compact">Compact</option><option value="compare">Compare</option></select></label></div>
</header>
${failures.length ? `<div class="fail"><h3>${failures.length} problem(s)</h3><ul>${
  failures.map((f) => `<li>${esc(f)}</li>`).join("")}</ul></div>` : ""}
<main>${sections}</main>
</div><script>
const filter=document.getElementById("page-filter"),layout=document.getElementById("layout");
function apply(){for(const page of document.querySelectorAll(".page"))page.hidden=filter.value!=="all"&&page.dataset.page!==filter.value;
  document.body.dataset.layout=layout.value;if(filter.value!=="all")history.replaceState(null,"","#"+filter.value);}
filter.addEventListener("change",()=>{if(filter.value!=="all")layout.value="compare";apply()});layout.addEventListener("change",apply);
const linked=location.hash.slice(1);if([...filter.options].some(option=>option.value===linked)){
  filter.value=linked;layout.value="compare";apply();}
</script></body></html>`;
}
