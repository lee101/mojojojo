import { run, plan, imports } from "/mojojojo.js";
import { highlight, attachEditor } from "/hl.js?v=20260802b";
import { decodeShare, shareURL } from "/static/share.js";

const $ = (id) => document.getElementById(id);
const root = $("ide");
const code = $("code");
const workspace = $("workspace");
const CREDIT_USD = 0.001;
const MS_PER_CREDIT = Number(root.dataset.msPerCredit);
const MS_PER_CREDIT_GPU = Number(root.dataset.msPerCreditGpu);
const SIGNUP_URL = root.dataset.signupUrl;
const LOGIN_URL = root.dataset.loginUrl;
const TOPUP_URL = root.dataset.topupUrl;
const SIGNED_IN = root.dataset.signedIn === "true";
const STORAGE_KEY = "mojojojo.editor.snippets.v1";
const LAYOUT_KEY = "mojojojo.editor.layout.v1";
const INDENT = "    ";

const examples = {
  sharpe: {
    name: "rolling_sharpe.py",
    code: code.value,
  },
  monte: {
    name: "monte_carlo.py",
    code: `# Pure Python is free in your browser. Numeric loops can go native on the server.
def monte_carlo_pi(n: int) -> float:
    inside = 0
    x = 12345
    for _ in range(n):
        x = (1103515245 * x + 12345) % 2147483648
        a = x / 2147483648.0
        x = (1103515245 * x + 12345) % 2147483648
        b = x / 2147483648.0
        if a * a + b * b <= 1.0:
            inside += 1
    return 4.0 * inside / n

print(monte_carlo_pi(500_000))`,
  },
  hello: {
    name: "hello_browser.py",
    code: `from statistics import mean

values = [12, 18, 21, 19, 25]
print("mean:", mean(values))
print("This ran privately in your browser.")`,
  },
};

let runCount = 0;
let totalMs = 0;
let totalCredits = 0;
let accruedMs = 0;
let activeName = examples.sharpe.name;
let activeSavedID = null;
let cleanValue = code.value;
let activeResultTab = "output";
let syntaxDiagnostic = null;
let toastTimer = 0;

const costViews = {
  output: {
    title: "This execution",
    value: "$0",
    credits: "free in browser",
    badge: "Browser",
    badgeClass: "badge browser",
    detail: "No credits used",
  },
  agent: {
    title: "This agent run",
    value: "Ready",
    credits: "0 tokens",
    badge: "Agent",
    badgeClass: "badge server",
    detail: "Token usage is metered by the agent service",
  },
};

function renderCostView(name) {
  const view = costViews[name];
  $("cost-title").textContent = view.title;
  $("cost-value").textContent = view.value;
  $("cost-credits").textContent = view.credits;
  $("route-badge").className = view.badgeClass;
  $("route-badge").textContent = view.badge;
  $("cost-detail").textContent = view.detail;
}

function updateCostView(name, patch) {
  Object.assign(costViews[name], patch);
  if (activeResultTab === name) renderCostView(name);
}

const repaint = attachEditor(code, "python");
const highlightLayer = document.querySelector(".hl-layer");
if (highlightLayer) highlightLayer.style.background = "transparent";
const currentLine = document.createElement("div");
currentLine.className = "current-line";
currentLine.setAttribute("aria-hidden", "true");
document.querySelector(".hl-wrap")?.prepend(currentLine);
const syntaxErrorLine = document.createElement("div");
syntaxErrorLine.className = "syntax-error-line";
syntaxErrorLine.setAttribute("aria-hidden", "true");
syntaxErrorLine.hidden = true;
document.querySelector(".hl-wrap")?.prepend(syntaxErrorLine);

function lineHeight() {
  return parseFloat(getComputedStyle(code).lineHeight) || 22.275;
}

function lineIndexAt(position) {
  return code.value.slice(0, position).split("\n").length - 1;
}

function renderGutter() {
  const count = Math.max(1, code.value.split("\n").length);
  const active = lineIndexAt(code.selectionStart);
  const fragment = document.createDocumentFragment();
  for (let i = 0; i < count; i += 1) {
    const span = document.createElement("span");
    span.textContent = String(i + 1);
    if (i === active) span.classList.add("active");
    if (syntaxDiagnostic?.line === i + 1) {
      span.classList.add("error");
      span.title = syntaxDiagnostic.message;
    }
    fragment.appendChild(span);
  }
  $("line-numbers").replaceChildren(fragment);
  $("line-numbers").scrollTop = code.scrollTop;
}

function updateCursor() {
  const before = code.value.slice(0, code.selectionStart);
  const line = before.split("\n");
  $("cursor-position").textContent = `Ln ${line.length}, Col ${line.at(-1).length + 1}`;
  const paddingTop = parseFloat(getComputedStyle(code).paddingTop) || 0;
  const top = paddingTop + lineIndexAt(code.selectionStart) * lineHeight() - code.scrollTop;
  currentLine.style.top = `${top}px`;
  currentLine.style.height = `${lineHeight()}px`;
  if (syntaxDiagnostic) {
    const errorTop = paddingTop + (syntaxDiagnostic.line - 1) * lineHeight() - code.scrollTop;
    syntaxErrorLine.style.top = `${errorTop}px`;
    syntaxErrorLine.style.height = `${lineHeight()}px`;
  }
  renderGutter();
}

function setDocument(name, value, { clean = true, savedID = null } = {}) {
  activeName = name || "untitled.py";
  activeSavedID = savedID;
  code.value = value;
  $("document-name").textContent = activeName;
  $("tab-name").textContent = activeName;
  cleanValue = clean ? value : null;
  clearDiagnostic();
  markDirty();
  repaint();
  refresh();
  code.focus();
  code.selectionStart = code.selectionEnd = 0;
  code.scrollTop = 0;
  updateCursor();
}

function markDirty() {
  const dirty = cleanValue === null || code.value !== cleanValue;
  $("dirty-mark").classList.toggle("clean", !dirty);
  $("save-state").textContent = dirty
    ? (activeSavedID ? "changes not saved" : "unsaved changes")
    : (activeSavedID ? "saved locally" : "example · not saved");
  $("save-label").textContent = activeSavedID ? "Update saved snippet" : "Save current snippet";
}

function options() {
  const opts = {
    gpu: $("gpu").checked,
    timeoutMs: Math.max(1, Number($("timeout").value || 30)) * 1000,
    source: "editor",
    endpoint: location.origin,
  };
  if ($("where").value !== "auto") opts.where = $("where").value;
  return opts;
}

function snippet(source) {
  const opts = options();
  const extras = [
    opts.gpu ? "\n    gpu=True," : "",
    opts.timeoutMs !== 30000 ? `\n    timeout_ms=${opts.timeoutMs},` : "",
  ].join("");
  const body = source.length > 520 ? `${source.slice(0, 500)}\n# …` : source;
  return `from mojojojo import run

result = run(
    """${body}""",${extras}
)
print(result.stdout)
print(result.charge.billed_ms, "ms billed")`;
}

function refresh() {
  const decision = plan(code.value, options());
  const chosen = decision.where === "browser" ? "browser" : "server";
  $("planline").innerHTML = `<span class="status-dot" aria-hidden="true"></span>${chosen}: ${decision.why}`;
  const modules = imports(code.value);
  $("pkgline").textContent = modules.length
    ? `Python · imports ${modules.join(", ")}`
    : "Python · no imports";
  $("snippet").innerHTML = highlight(snippet(code.value), "python");
  updateCostView("output", {
    badgeClass: `badge ${chosen}`,
    badge: chosen === "browser" ? "Browser" : ($("gpu").checked ? "GPU" : "Server"),
  });
  if (chosen === "browser") {
    setCost(0, 0, "Free local execution");
  } else if (!runCount) {
    updateCostView("output", {
      value: "Metered",
      credits: "billed by actual runtime",
      detail: "Compile and queue time stay free",
    });
  }
  markDirty();
  updateCursor();
}

function usd(billedMs, gpu) {
  return (billedMs / (gpu ? MS_PER_CREDIT_GPU : MS_PER_CREDIT)) * CREDIT_USD;
}

function money(value) {
  if (!value) return "$0";
  if (value < 0.01) return `$${value.toFixed(6).replace(/0+$/, "")}`;
  return `$${value.toFixed(4).replace(/0+$/, "").replace(/\.$/, "")}`;
}

function setCost(billedMs, credits, detail, local = billedMs === 0) {
  updateCostView("output", {
    value: local ? "$0" : money(usd(billedMs, $("gpu").checked)),
    credits: local
      ? "0 credits · browser runtime"
      : `${credits} credit${credits === 1 ? "" : "s"} · ${Math.round(billedMs)} ms billed`,
    detail,
  });
}

function cta(...actions) {
  const box = $("cta");
  box.replaceChildren();
  box.hidden = actions.length === 0;
  for (const [label, href, ghost] of actions) {
    const link = document.createElement("a");
    link.className = ghost ? "ghost" : "";
    link.href = href;
    link.textContent = label;
    box.appendChild(link);
  }
}

function setOutput(stdout, stderr) {
  const out = $("out");
  const stdoutEl = out.querySelector(".stdout");
  const stderrEl = out.querySelector(".stderr");
  stdoutEl.textContent = stdout || (!stderr ? "(no output)" : "");
  stderrEl.textContent = stderr || "";
  stderrEl.hidden = !stderr;
  out.scrollTop = out.scrollHeight;
}

function parseSyntaxDiagnostic(text) {
  const report = String(text || "");
  const problem = [...report.matchAll(/\b(SyntaxError|IndentationError|TabError):\s*([^\n]*)/g)].at(-1);
  if (!problem) return null;
  const locations = [...report.matchAll(/File\s+["'][^"']+["'],\s+line\s+(\d+)/g)];
  const fallback = [...report.matchAll(/\bline\s+(\d+)\b/g)];
  const line = Number(locations.at(-1)?.[1] || fallback.at(-1)?.[1] || 0);
  if (!line || line > code.value.split("\n").length) return null;
  return {
    kind: problem[1],
    message: problem[2].trim() || "Invalid Python syntax",
    line,
  };
}

function clearDiagnostic() {
  syntaxDiagnostic = null;
  syntaxErrorLine.hidden = true;
  $("editor-diagnostic").hidden = true;
  renderGutter();
}

function showDiagnostic(text) {
  const diagnostic = parseSyntaxDiagnostic(text);
  if (!diagnostic) {
    clearDiagnostic();
    return false;
  }
  syntaxDiagnostic = diagnostic;
  syntaxErrorLine.hidden = false;
  $("diagnostic-kind").textContent = diagnostic.kind;
  $("diagnostic-message").textContent = diagnostic.message;
  $("diagnostic-location").textContent = `Ln ${diagnostic.line}`;
  $("editor-diagnostic").hidden = false;
  updateCursor();
  return true;
}

function jumpToDiagnostic() {
  if (!syntaxDiagnostic) return;
  const lines = code.value.split("\n");
  const position = lines.slice(0, syntaxDiagnostic.line - 1)
    .reduce((total, line) => total + line.length + 1, 0);
  code.focus();
  code.setSelectionRange(position, position + (lines[syntaxDiagnostic.line - 1]?.length || 0));
  code.scrollTop = Math.max(0, (syntaxDiagnostic.line - 1) * lineHeight() - code.clientHeight / 3);
  updateCursor();
}

function nativeLabel(result) {
  if (result.accel?.native_calls) return [`Native · ${result.accel.native_calls} calls`, "native"];
  if (result.accel?.queued) return ["Compiling next run", "server"];
  return ["Interpreted", "interpreted"];
}

function addHistory(result, gpu) {
  const history = $("history");
  history.querySelector(".empty-row")?.remove();
  const billed = result.charge?.billed_ms || 0;
  const row = document.createElement("tr");
  const route = result.where === "browser" ? "browser" : (gpu ? "GPU" : result.where);
  const cost = result.where === "browser" ? "free" : money(usd(billed, gpu));
  for (const value of [
    `#${runCount}`,
    route,
    `${Math.round(result.wall_ms || 0)}ms`,
    `${Math.round(billed)}ms`,
    cost,
  ]) {
    const cell = document.createElement("td");
    cell.textContent = value;
    row.appendChild(cell);
  }
  history.prepend(row);
}

async function execute() {
  const button = $("run");
  const label = button.querySelector("span:nth-of-type(2)");
  button.disabled = true;
  label.textContent = "Running…";
  $("run-status").className = "badge running";
  $("run-status").textContent = "Running";
  $("wall-time").textContent = "measuring wall time…";
  $("billed-time").textContent = "billing starts with your code";
  $("native-badge").className = "badge interpreted";
  $("native-badge").textContent = "Planning";
  clearDiagnostic();
  setOutput("Starting runtime…", "");
  cta();

  const opts = options();
  try {
    const result = await run(code.value, {
      ...opts,
      onPlan: (decision) => {
        const route = decision.where === "browser" ? "browser" : decision.where;
        $("run-status").textContent = `Running · ${route}`;
        updateCostView("output", {
          badgeClass: `badge ${route === "browser" ? "browser" : "server"}`,
          badge: route === "browser" ? "Browser" : (opts.gpu ? "GPU" : "Server"),
        });
        if (route === "browser") {
          setCost(0, 0, "Local execution is always free", true);
        } else if (decision.estimate_ms) {
          const estimate = Number(decision.estimate_ms);
          const rate = opts.gpu ? MS_PER_CREDIT_GPU : MS_PER_CREDIT;
          updateCostView("output", {
            value: `≈${money(decision.price_usd || usd(estimate, opts.gpu))}`,
            credits: `≈${(estimate / rate).toFixed(3)} credits · ${Math.round(estimate)} ms`,
            detail: "Live estimate · final charge uses measured time",
          });
        } else {
          updateCostView("output", {
            value: "Metering",
            credits: "measuring execution time",
            detail: "Queue and compile time remain free",
          });
        }
      },
      onFailover: ({ from }) => {
        setOutput(`The ${from} runtime declined this run; trying the next safe route…`, "");
      },
    });

    runCount += 1;
    const billed = result.charge?.billed_ms || 0;
    const credits = result.charge?.credits || 0;
    totalMs += billed;
    totalCredits += credits;
    accruedMs = result.charge?.owed_ms ?? accruedMs;
    setOutput(result.stdout || "", result.stderr || "");
    if (result.ok === false) showDiagnostic(result.stderr || result.error || "");

    const [native, nativeClass] = nativeLabel(result);
    $("run-status").className = result.ok === false ? "badge error" : "badge ok";
    $("run-status").textContent = result.ok === false ? "Exited 1" : "Complete";
    $("wall-time").textContent = `${Math.round(result.wall_ms || 0)} ms wall`;
    $("billed-time").textContent = `${Math.round(billed)} ms billed`;
    $("native-badge").className = `badge ${nativeClass}`;
    $("native-badge").textContent = native;
    updateCostView("output", {
      badgeClass: `badge ${result.where === "browser" ? "browser" : "server"}`,
      badge: result.where === "browser" ? "Browser" : (opts.gpu ? "GPU" : "Server"),
    });

    const detail = result.where === "browser"
      ? "Ran locally · always free"
      : (result.warm_ms > 50
        ? `${Math.round(result.warm_ms)} ms setup/compile excluded`
        : "Only execution time was billed");
    setCost(billed, credits, detail, result.where === "browser");
    addHistory(result, opts.gpu);
    $("session").textContent =
      `${runCount} run${runCount === 1 ? "" : "s"} · ${Math.round(totalMs)} ms · ${totalCredits} credits` +
      (accruedMs ? ` · ${Math.round(accruedMs)} ms accrued` : "");

    const balance = result.charge?.balance;
    if (balance !== undefined && result.where !== "browser" && balance < 100) {
      cta(["Add credits", TOPUP_URL], ["View balance", "/credits", true]);
    }
  } catch (error) {
    const report = error.body?.stderr || error.body?.error || error.message;
    setOutput("", `error: ${report}`);
    showDiagnostic(report);
    $("run-status").className = "badge error";
    $("run-status").textContent = "Error";
    $("wall-time").textContent = "run did not complete";
    $("billed-time").textContent = "no completed charge";
    $("native-badge").className = "badge interpreted";
    $("native-badge").textContent = "Not measured";
    if (error.status === 401) {
      setCost(0, 0, "Server execution needs an account");
      cta(["Sign up · $1 free", SIGNUP_URL], ["Sign in", LOGIN_URL, true],
        ["Run in browser", "#browser", true]);
      $("cta").lastElementChild.addEventListener("click", (event) => {
        event.preventDefault();
        $("where").value = "browser";
        refresh();
        execute();
      });
    } else if (error.status === 402) {
      updateCostView("output", {
        value: "No credit",
        credits: "balance is empty",
        detail: "No completed execution charge",
      });
      cta(["Add credits", TOPUP_URL], ["View balance", "/credits", true]);
    } else {
      setCost(0, 0, "This attempt was not billed");
    }
  } finally {
    button.disabled = false;
    label.textContent = "Run";
  }
}

const agentRun = {
  controller: null,
  runID: "",
  running: false,
  stopping: false,
  pendingCalls: [],
  currentKind: "",
  currentElement: null,
};

function selectResultTab(name, focus = true) {
  activeResultTab = name;
  for (const tabName of ["output", "agent"]) {
    const selected = tabName === name;
    const tab = $(`${tabName}-tab`);
    const panel = $(`${tabName}-panel`);
    tab.classList.toggle("active", selected);
    tab.setAttribute("aria-selected", String(selected));
    tab.tabIndex = selected ? 0 : -1;
    panel.hidden = !selected;
  }
  renderCostView(name);
  if (focus) $(`${name}-tab`).focus();
}

function setAgentStatus(label, style = "idle") {
  $("agent-status").className = `badge ${style}`;
  $("agent-status").textContent = label;
}

function finishAgentBlock() {
  if (agentRun.currentKind === "reasoning" && agentRun.currentElement) {
    agentRun.currentElement.open = false;
  }
  agentRun.currentKind = "";
  agentRun.currentElement = null;
}

function appendAgentEvent(kind, text, name = "", meta = {}) {
  const stream = $("agent-stream");
  stream.querySelector(".agent-empty")?.remove();

  if (kind === "reasoning") {
    if (agentRun.currentKind !== "reasoning") {
      finishAgentBlock();
      const details = document.createElement("details");
      details.className = "agent-reasoning";
      details.open = true;
      const summary = document.createElement("summary");
      summary.textContent = "Reasoning";
      const body = document.createElement("p");
      details.append(summary, body);
      stream.appendChild(details);
      agentRun.currentKind = "reasoning";
      agentRun.currentElement = details;
    }
    agentRun.currentElement.querySelector("p").textContent += text;
  } else if (kind === "text") {
    if (agentRun.currentKind !== "text") {
      finishAgentBlock();
      const answer = document.createElement("p");
      answer.className = "agent-answer";
      stream.appendChild(answer);
      agentRun.currentKind = "text";
      agentRun.currentElement = answer;
    }
    agentRun.currentElement.textContent += text;
  } else if (kind === "tool_call") {
    finishAgentBlock();
    const parsed = parseToolArguments(text);
    const line = document.createElement("p");
    line.className = "agent-tool-line";
    line.textContent = compactToolCall(name, parsed);
    stream.appendChild(line);
    agentRun.pendingCalls.push({ name, arguments: parsed, line });
  } else if (kind === "tool_result") {
    finishAgentBlock();
    const callIndex = agentRun.pendingCalls.findIndex((item) => item.name === name);
    const call = callIndex >= 0 ? agentRun.pendingCalls.splice(callIndex, 1)[0] : null;
    if (call && meta.ok === false) call.line.classList.add("failed");
    if (call && name === "apply_patch") {
      call.line.textContent = compactPatchResult(text, meta);
      if (meta.ok !== false) mirrorAgentPatch(call.arguments?.input, meta.files);
    }
    const details = document.createElement("details");
    details.className = "agent-tool-result";
    const summary = document.createElement("summary");
    summary.textContent = `${name || "Tool"} result${meta.ok === false ? " · failed" : ""}`;
    const body = document.createElement("pre");
    body.textContent = text || "(no output)";
    details.append(summary, body);
    stream.appendChild(details);
  } else if (kind === "error") {
    finishAgentBlock();
    const error = document.createElement("p");
    error.className = "agent-error";
    error.textContent = text;
    stream.appendChild(error);
    setAgentStatus("Error", "error");
  }
  stream.scrollTop = stream.scrollHeight;
}

function parseToolArguments(text) {
  try {
    const value = JSON.parse(text || "{}");
    return value && typeof value === "object" ? value : {};
  } catch {
    return {};
  }
}

function compactToolCall(name, args) {
  if (name === "search") return `search ${JSON.stringify(args.query || "")}`;
  if (name === "apply_patch") return "apply_patch…";
  if (name === "read") return `read ${args.path || ""}`.trim();
  if (name === "ls") return `ls ${args.path || "."}`;
  if (name === "py") return "py";
  if (name === "shell") {
    const command = Array.isArray(args.command) ? args.command.join(" ") : (args.command || "");
    return `shell ${command}`.trim();
  }
  const first = Object.values(args).find((value) => typeof value === "string");
  return `${name || "tool"}${first ? ` ${String(first).slice(0, 80)}` : ""}`;
}

function compactPatchResult(text, meta) {
  const counts = [...String(text || "").matchAll(/:\s*\+(\d+)\s+-(\d+)/g)];
  const additions = counts.reduce((sum, match) => sum + Number(match[1]), 0);
  const deletions = counts.reduce((sum, match) => sum + Number(match[2]), 0);
  const files = Array.isArray(meta.files) ? meta.files.length : counts.length;
  return `apply_patch ${files} file${files === 1 ? "" : "s"} +${additions} -${deletions}`;
}

function sameOpenFile(path) {
  const normal = String(path || "").replaceAll("\\", "/").replace(/^\.\//, "");
  const open = activeName.replaceAll("\\", "/").replace(/^\.\//, "");
  return normal === open || normal.split("/").at(-1) === open.split("/").at(-1);
}

function parsePatchEnvelope(patch) {
  const lines = String(patch || "").trim().split(/\r?\n/);
  if (lines[0] !== "*** Begin Patch" || lines.at(-1) !== "*** End Patch") {
    throw new Error("unrecognised patch envelope");
  }
  const operations = [];
  let index = 1;
  const isHeader = (line) => /^(?:\*\*\* Add File: |\*\*\* Update File: |\*\*\* Delete File: )/.test(line);
  while (index < lines.length - 1) {
    const header = lines[index++];
    const match = header.match(/^\*\*\* (Add|Update|Delete) File: (.+)$/);
    if (!match) throw new Error("unrecognised file operation");
    const operation = { kind: match[1].toLowerCase(), path: match[2], added: [], chunks: [] };
    if (operation.kind === "add") {
      while (index < lines.length - 1 && !isHeader(lines[index])) {
        if (!lines[index].startsWith("+")) throw new Error("invalid added line");
        operation.added.push(lines[index++].slice(1));
      }
    } else if (operation.kind === "update") {
      while (index < lines.length - 1 && !isHeader(lines[index])) {
        let section = null;
        if (lines[index] === "@@" || lines[index].startsWith("@@ ")) {
          section = lines[index] === "@@" ? null : lines[index].slice(3);
          index += 1;
        }
        const chunk = { section, old: [], next: [], end: false };
        while (index < lines.length - 1 && !isHeader(lines[index]) &&
          lines[index] !== "@@" && !lines[index].startsWith("@@ ")) {
          if (lines[index] === "*** End of File") {
            chunk.end = true;
            index += 1;
            break;
          }
          const prefix = lines[index][0];
          if (![" ", "+", "-"].includes(prefix)) throw new Error("invalid patch hunk");
          const content = lines[index++].slice(1);
          if (prefix !== "+") chunk.old.push(content);
          if (prefix !== "-") chunk.next.push(content);
        }
        operation.chunks.push(chunk);
      }
    }
    operations.push(operation);
  }
  return operations;
}

function findPatchLines(lines, wanted, start, atEnd = false) {
  const last = lines.length - wanted.length;
  const starts = atEnd ? [last] : Array.from({ length: Math.max(0, last - start + 1) },
    (_, index) => start + index);
  for (const normalise of [(value) => value, (value) => value.trimEnd(), (value) => value.trim()]) {
    const needle = wanted.map(normalise);
    for (const at of starts) {
      if (at >= 0 && lines.slice(at, at + wanted.length).map(normalise)
        .every((line, index) => line === needle[index])) return at;
    }
  }
  return -1;
}

function applyUpdateChunks(source, chunks) {
  const newline = source.includes("\r\n") ? "\r\n" : "\n";
  const finalNewline = /[\r\n]$/.test(source);
  const lines = source.split(/\r?\n/);
  if (finalNewline) lines.pop();
  let cursor = 0;
  for (const chunk of chunks) {
    if (chunk.section !== null) {
      const needle = chunk.section.trim();
      const sectionAt = lines.findIndex((line, index) =>
        index >= cursor && (line.trim() === needle || line.trim().includes(needle)));
      if (sectionAt < 0) throw new Error(`section not found: ${needle}`);
      cursor = sectionAt + 1;
    }
    let at = chunk.old.length
      ? findPatchLines(lines, chunk.old, cursor, chunk.end)
      : (chunk.end ? lines.length : cursor);
    if (at < 0) {
      // A hosted workspace may begin behind the browser buffer. A first patch
      // can reconcile that copy to content already present here; treat the
      // desired block as synchronized instead of reporting false divergence.
      const alreadyAt = findPatchLines(lines, chunk.next, cursor, chunk.end);
      if (alreadyAt >= 0) {
        cursor = alreadyAt + chunk.next.length;
        continue;
      }
      throw new Error(`patch context not found: ${chunk.old.find((line) => line.trim()) || "blank"}`);
    }
    lines.splice(at, chunk.old.length, ...chunk.next);
    cursor = at + chunk.next.length;
  }
  const result = lines.join(newline);
  return lines.length && (finalNewline || !source) ? result + newline : result;
}

function mirrorAgentPatch(patch, reportedFiles) {
  if (typeof patch !== "string") return;
  let operations;
  try {
    operations = parsePatchEnvelope(patch).filter((operation) => sameOpenFile(operation.path));
  } catch (error) {
    $("agent-file-status").textContent = `Editor sync needs review: ${error.message}`;
    $("agent-file-status").classList.remove("changed");
    return;
  }
  if (!operations.length) return;
  if (Array.isArray(reportedFiles) &&
    !reportedFiles.some((path) => sameOpenFile(path))) return;

  let updated = code.value;
  try {
    for (const operation of operations) {
      if (operation.kind === "add") updated = `${operation.added.join("\n")}\n`;
      else if (operation.kind === "delete") updated = "";
      else updated = applyUpdateChunks(updated, operation.chunks);
    }
  } catch (error) {
    $("agent-file-status").textContent = `Agent changed ${activeName}; editor sync needs review`;
    $("agent-file-status").classList.remove("changed");
    appendAgentEvent("error", `The agent changed the open file, but its patch could not be mirrored: ${error.message}`);
    return;
  }

  code.value = updated;
  code.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText" }));
  const message = `Updated ${activeName} in the editor`;
  $("agent-file-status").textContent = message;
  $("agent-file-status").classList.add("changed");
  const notice = document.createElement("p");
  notice.className = "agent-tool-line";
  notice.textContent = message;
  $("agent-stream").appendChild(notice);
}

function agentRequestPrompt(instruction) {
  const filename = activeName.split(/[\\/]/).at(-1) || "untitled.py";
  const context = [
    instruction,
    "",
    `The browser editor has ${filename} open. Its content below is authoritative.`,
    `If you edit it, use apply_patch on exactly ${filename}. First reconcile any stale workspace copy.`,
    `<open_file name=${JSON.stringify(filename)}>`,
    code.value,
    "</open_file>",
  ].join("\n");
  if (context.length > 250000) {
    throw new Error("The open file and instruction are too large for one agent request.");
  }
  return context;
}

function updateAgentUsage(step) {
  const meta = step.meta || {};
  let tokens = Number(meta.tokens || 0);
  if (!tokens) {
    const input = step.text.match(/\bin\s+(\d+)/)?.[1] || 0;
    const output = step.text.match(/\bout\s+(\d+)/)?.[1] || 0;
    tokens = Number(input) + Number(output);
  }
  $("agent-usage").textContent = tokens
    ? `${tokens.toLocaleString()} tokens · ${step.text}`
    : step.text;
  updateCostView("agent", {
    value: meta.final ? money(Number(meta.cost?.credits || 0) * CREDIT_USD) : "Metering",
    credits: `${tokens.toLocaleString()} token${tokens === 1 ? "" : "s"}` +
      (meta.final ? ` · ${meta.cost?.credits || 0} credit${meta.cost?.credits === 1 ? "" : "s"}` : ""),
    detail: meta.final
      ? `${Number(meta.cost?.owed_tokens || 0).toLocaleString()} tokens accrued` +
        (meta.cost?.balance === undefined ? "" : ` · ${meta.cost.balance} credits left`)
      : step.text,
  });
  if (meta.final) {
    finishAgentBlock();
    setAgentStatus("Complete", "ok");
  }
}

function handleAgentStep(event, step) {
  if (event === "run") {
    agentRun.runID = step.id || agentRun.runID;
    return;
  }
  if (event === "usage") {
    updateAgentUsage(step);
    return;
  }
  appendAgentEvent(step.kind || event, step.text || "", step.name || "", step.meta || {});
}

async function consumeAgentStream(response) {
  const reader = response.body?.getReader();
  if (!reader) throw new Error("The agent returned no event stream.");
  const decoder = new TextDecoder();
  let buffer = "";
  while (true) {
    const { value, done } = await reader.read();
    buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
    buffer = buffer.replace(/\r\n/g, "\n");
    let boundary;
    while ((boundary = buffer.indexOf("\n\n")) >= 0) {
      const block = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      if (!block || block.startsWith(":")) continue;
      let event = "message";
      const data = [];
      for (const line of block.split("\n")) {
        if (line.startsWith("event:")) event = line.slice(6).trim();
        else if (line.startsWith("data:")) data.push(line.slice(5).trimStart());
      }
      if (!data.length) continue;
      try {
        handleAgentStep(event, JSON.parse(data.join("\n")));
      } catch {
        appendAgentEvent("error", "The agent sent an unreadable stream event.");
      }
    }
    if (done) break;
  }
}

async function runAgent(event) {
  event?.preventDefault();
  const instruction = $("agent-instruction").value.trim();
  if (!instruction || agentRun.running) return;

  let prompt;
  try {
    prompt = agentRequestPrompt(instruction);
  } catch (error) {
    appendAgentEvent("error", error.message);
    return;
  }

  selectResultTab("agent", false);
  $("agent-stream").replaceChildren();
  $("agent-file-status").textContent = "Watching the open file for edits";
  $("agent-file-status").classList.remove("changed");
  $("agent-usage").textContent = "0 tokens · starting";
  agentRun.pendingCalls = [];
  agentRun.currentKind = "";
  agentRun.currentElement = null;
  agentRun.runID = "";
  agentRun.running = true;
  agentRun.stopping = false;
  agentRun.controller = new AbortController();
  $("agent-start").disabled = true;
  $("agent-stop").disabled = false;
  $("agent-instruction").disabled = true;
  $("agent-stream").setAttribute("aria-busy", "true");
  setAgentStatus("Running", "running");
  updateCostView("agent", {
    value: "Metering",
    credits: "0 tokens · starting",
    detail: "The agent service bills exact model usage",
  });

  try {
    const response = await fetch("/v1/agent/runs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt }),
      signal: agentRun.controller.signal,
    });
    agentRun.runID = response.headers.get("X-Run-ID") || agentRun.runID;
    if (!response.ok) {
      let message = `Agent request failed (${response.status})`;
      try {
        const problem = await response.json();
        message = problem.error || message;
      } catch { /* keep the status-based message */ }
      const error = new Error(message);
      error.status = response.status;
      throw error;
    }
    await consumeAgentStream(response);
    if (!agentRun.stopping && $("agent-status").textContent === "Running") {
      setAgentStatus("Complete", "ok");
    }
  } catch (error) {
    if (error.name === "AbortError" || agentRun.stopping) {
      setAgentStatus("Stopped", "idle");
      updateCostView("agent", { detail: "Run interrupted" });
    } else {
      appendAgentEvent("error", error.message);
      updateCostView("agent", {
        value: error.status === 402 ? "No credit" : "Not billed",
        detail: error.status === 401 ? "Sign in to use the agent" : "The run did not complete",
      });
    }
  } finally {
    finishAgentBlock();
    agentRun.running = false;
    agentRun.controller = null;
    $("agent-start").disabled = false;
    $("agent-stop").disabled = true;
    $("agent-instruction").disabled = false;
    $("agent-stream").setAttribute("aria-busy", "false");
  }
}

async function stopAgent() {
  if (!agentRun.running || agentRun.stopping) return;
  agentRun.stopping = true;
  setAgentStatus("Stopping", "running");
  try {
    if (agentRun.runID) {
      await fetch(`/v1/agent/runs/${encodeURIComponent(agentRun.runID)}/interrupt`, {
        method: "POST",
      });
    }
  } catch {
    // Aborting the controlling stream is also an interrupt at the agent
    // service, so a failed acknowledgement must not leave the run attached.
  } finally {
    agentRun.controller?.abort();
  }
}

function replaceSelection(value, start, end) {
  code.setRangeText(value, start, end, "preserve");
  code.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText", data: value }));
}

function indentSelection(unindent) {
  const value = code.value;
  const start = code.selectionStart;
  const end = code.selectionEnd;
  const blockStart = value.lastIndexOf("\n", start - 1) + 1;
  let blockEnd = value.indexOf("\n", end);
  if (blockEnd < 0) blockEnd = value.length;
  const selected = value.slice(blockStart, blockEnd);

  if (!unindent && start === end) {
    replaceSelection(INDENT, start, end);
    code.selectionStart = code.selectionEnd = start + INDENT.length;
    return;
  }

  const lines = selected.split("\n");
  let removedFirst = 0;
  let totalDelta = 0;
  const changed = lines.map((line, index) => {
    if (!unindent) {
      totalDelta += INDENT.length;
      return INDENT + line;
    }
    const match = line.match(/^( {1,4}|\t)/);
    const amount = match ? match[0].length : 0;
    if (index === 0) removedFirst = amount;
    totalDelta -= amount;
    return line.slice(amount);
  }).join("\n");

  code.setRangeText(changed, blockStart, blockEnd, "select");
  code.selectionStart = unindent ? Math.max(blockStart, start - removedFirst) : start + INDENT.length;
  code.selectionEnd = Math.max(code.selectionStart, end + totalDelta);
  code.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText" }));
}

function toggleComment() {
  const value = code.value;
  const start = code.selectionStart;
  const end = code.selectionEnd;
  const blockStart = value.lastIndexOf("\n", start - 1) + 1;
  let blockEnd = value.indexOf("\n", end);
  if (blockEnd < 0) blockEnd = value.length;
  const lines = value.slice(blockStart, blockEnd).split("\n");
  const nonBlank = lines.filter((line) => line.trim());
  const remove = nonBlank.length > 0 && nonBlank.every((line) => /^\s*# ?/.test(line));
  const changed = lines.map((line) => {
    if (!line.trim()) return line;
    if (remove) return line.replace(/^(\s*)# ?/, "$1");
    return line.replace(/^(\s*)/, "$1# ");
  }).join("\n");
  code.setRangeText(changed, blockStart, blockEnd, "select");
  code.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText" }));
}

function handlePairs(event) {
  const pairs = { "(": ")", "[": "]", "{": "}", '"': '"', "'": "'" };
  const closer = pairs[event.key];
  if (!closer || event.ctrlKey || event.metaKey || event.altKey) return false;
  event.preventDefault();
  const start = code.selectionStart;
  const end = code.selectionEnd;
  const selected = code.value.slice(start, end);
  replaceSelection(`${event.key}${selected}${closer}`, start, end);
  code.selectionStart = start + 1;
  code.selectionEnd = selected ? end + 1 : start + 1;
  return true;
}

function handleClosing(event) {
  if (![")", "]", "}", '"', "'"].includes(event.key)) return false;
  if (code.selectionStart !== code.selectionEnd) return false;
  if (code.value[code.selectionStart] !== event.key) return false;
  event.preventDefault();
  code.selectionStart += 1;
  code.selectionEnd = code.selectionStart;
  updateCursor();
  return true;
}

function loadSaved() {
  try {
    const data = JSON.parse(localStorage.getItem(STORAGE_KEY) || "[]");
    return Array.isArray(data) ? data : [];
  } catch {
    return [];
  }
}

function savedKey(item) {
  return item.id || `name:${item.name}`;
}

function renderSaved() {
  const list = $("saved-list");
  const saved = loadSaved();
  $("saved-count").textContent = String(saved.length);
  list.replaceChildren();
  if (!saved.length) {
    const empty = document.createElement("p");
    empty.className = "rail-empty";
    empty.textContent = "Save a draft to keep it in this browser.";
    list.appendChild(empty);
    return;
  }
  saved.forEach((item) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "file-item";
    button.classList.toggle("active", savedKey(item) === activeSavedID);
    button.innerHTML = `<span class="py-icon" aria-hidden="true">Py</span><span><b></b><small>local browser storage</small></span>`;
    button.querySelector("b").textContent = item.name;
    button.addEventListener("click", () => {
      document.querySelectorAll("[data-example]").forEach((example) => example.classList.remove("active"));
      setDocument(item.name, item.code, { savedID: savedKey(item) });
      renderSaved();
    });
    list.appendChild(button);
  });
}

function saveCurrent() {
  const saved = loadSaved();
  let index = activeSavedID
    ? saved.findIndex((item) => savedKey(item) === activeSavedID)
    : saved.findIndex((item) => item.name === activeName);
  const existing = index >= 0 ? saved[index] : null;
  const id = existing?.id || globalThis.crypto?.randomUUID?.() || `saved-${Date.now()}`;
  const item = { id, name: activeName, code: code.value, savedAt: Date.now() };
  if (index >= 0) saved[index] = item;
  else saved.unshift(item);
  localStorage.setItem(STORAGE_KEY, JSON.stringify(saved.slice(0, 20)));
  activeSavedID = id;
  document.querySelectorAll("[data-example]").forEach((example) => example.classList.remove("active"));
  cleanValue = code.value;
  markDirty();
  renderSaved();
  $("save-state").textContent = existing ? "updated locally" : "saved locally";
}

function copyText(text, button) {
  navigator.clipboard.writeText(text).then(() => {
    const before = button.textContent;
    button.textContent = "Copied";
    setTimeout(() => { button.textContent = before; }, 1200);
  }).catch(() => {
    const before = button.textContent;
    button.textContent = "Copy failed";
    setTimeout(() => { button.textContent = before; }, 1200);
  });
}

function showToast(message) {
  const toast = $("editor-toast");
  clearTimeout(toastTimer);
  toast.textContent = message;
  toast.hidden = false;
  toastTimer = setTimeout(() => { toast.hidden = true; }, 2600);
}

function sharePayload() {
  return {
    name: activeName,
    code: code.value,
    where: $("where").value,
    gpu: $("gpu").checked,
    timeout: Number($("timeout").value || 30),
  };
}

async function shareCurrent() {
  const button = $("share-code");
  const before = button.textContent;
  button.disabled = true;
  button.textContent = "Creating…";
  try {
    const url = await shareURL(sharePayload(), `${location.origin}/editor`);
    if (url.length > 24000) {
      throw new Error("This file is too large for a reliable URL. Download it instead.");
    }
    await navigator.clipboard.writeText(url);
    button.textContent = "Copied";
    showToast("Private share link copied — code stays in the URL until it is run.");
  } catch (error) {
    button.textContent = "Try again";
    showToast(error.message || "Could not copy a share link.");
  } finally {
    setTimeout(() => {
      button.textContent = before;
      button.disabled = false;
    }, 1400);
  }
}

function downloadCurrent() {
  const safeName = (activeName || "snippet.py").replace(/[^A-Za-z0-9._-]+/g, "_");
  const filename = safeName.endsWith(".py") ? safeName : `${safeName}.py`;
  const url = URL.createObjectURL(new Blob([code.value], { type: "text/x-python;charset=utf-8" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  link.click();
  URL.revokeObjectURL(url);
  showToast(`Downloaded ${filename}`);
}

async function loadSharedSnippet() {
  const shared = await decodeShare();
  if (!shared) return false;
  if (["auto", "browser", "server"].includes(shared.where)) $("where").value = shared.where;
  $("gpu").checked = shared.gpu === true;
  if (Number.isFinite(Number(shared.timeout))) {
    $("timeout").value = String(Math.max(1, Math.min(300, Number(shared.timeout))));
  }
  document.querySelectorAll(".file-item.active").forEach((item) => item.classList.remove("active"));
  setDocument(String(shared.name || "shared.py").slice(0, 120), shared.code, { clean: false });
  renderSaved();
  showToast("Shared snippet opened. Review it, then run or save your own copy.");
  return true;
}

function initSplitters() {
  try {
    const saved = JSON.parse(localStorage.getItem(LAYOUT_KEY) || "{}");
    if (saved.rail) workspace.style.setProperty("--rail-size", `${saved.rail}px`);
    if (saved.results) workspace.style.setProperty("--results-size", `${saved.results}px`);
  } catch { /* defaults are intentionally usable */ }

  for (const splitter of document.querySelectorAll("[data-splitter]")) {
    const reportValue = () => {
      const styles = getComputedStyle(workspace);
      const property = splitter.dataset.splitter === "left" ? "--rail-size" : "--results-size";
      splitter.setAttribute("aria-valuenow", String(Math.round(parseFloat(styles.getPropertyValue(property)))));
    };
    reportValue();
    splitter.addEventListener("pointerdown", (event) => {
      if (matchMedia("(max-width: 899px)").matches) return;
      splitter.setPointerCapture(event.pointerId);
      splitter.classList.add("dragging");
      const move = (moveEvent) => {
        const rect = workspace.getBoundingClientRect();
        if (splitter.dataset.splitter === "left") {
          const width = Math.max(170, Math.min(380, moveEvent.clientX - rect.left));
          workspace.style.setProperty("--rail-size", `${width}px`);
        } else {
          const width = Math.max(300, Math.min(620, rect.right - moveEvent.clientX));
          workspace.style.setProperty("--results-size", `${width}px`);
        }
        reportValue();
      };
      const stop = () => {
        splitter.classList.remove("dragging");
        splitter.removeEventListener("pointermove", move);
        splitter.removeEventListener("pointerup", stop);
        splitter.removeEventListener("pointercancel", stop);
        const styles = getComputedStyle(workspace);
        localStorage.setItem(LAYOUT_KEY, JSON.stringify({
          rail: parseFloat(styles.getPropertyValue("--rail-size")),
          results: parseFloat(styles.getPropertyValue("--results-size")),
        }));
      };
      splitter.addEventListener("pointermove", move);
      splitter.addEventListener("pointerup", stop);
      splitter.addEventListener("pointercancel", stop);
    });

    splitter.addEventListener("keydown", (event) => {
      if (!["ArrowLeft", "ArrowRight"].includes(event.key)) return;
      event.preventDefault();
      const delta = event.key === "ArrowRight" ? 12 : -12;
      const styles = getComputedStyle(workspace);
      if (splitter.dataset.splitter === "left") {
        const current = parseFloat(styles.getPropertyValue("--rail-size"));
        workspace.style.setProperty("--rail-size", `${Math.max(170, Math.min(380, current + delta))}px`);
      } else {
        const current = parseFloat(styles.getPropertyValue("--results-size"));
        workspace.style.setProperty("--results-size", `${Math.max(300, Math.min(620, current - delta))}px`);
      }
      reportValue();
    });
  }
}

$("run").addEventListener("click", execute);
$("clear").addEventListener("click", () => {
  clearDiagnostic();
  setOutput("Ready.", "");
  $("run-status").className = "badge idle";
  $("run-status").textContent = "Ready";
});
$("copy-code").addEventListener("click", (event) => copyText(code.value, event.currentTarget));
$("share-code").addEventListener("click", shareCurrent);
$("download-code").addEventListener("click", downloadCurrent);
$("copy-output").addEventListener("click", (event) => {
  const stdout = $("out").querySelector(".stdout").textContent;
  const stderr = $("out").querySelector(".stderr").textContent;
  copyText([stdout, stderr].filter(Boolean).join("\n"), event.currentTarget);
});
$("copy-snippet").addEventListener("click", (event) => copyText(snippet(code.value), event.currentTarget));
$("save-snippet").addEventListener("click", saveCurrent);
$("new-snippet").addEventListener("click", () => {
  document.querySelectorAll(".file-item.active").forEach((item) => item.classList.remove("active"));
  setDocument("untitled.py", "", { clean: false });
  renderSaved();
});
$("editor-diagnostic").addEventListener("click", jumpToDiagnostic);
$("agent-form").addEventListener("submit", runAgent);
$("agent-stop").addEventListener("click", stopAgent);

for (const name of ["output", "agent"]) {
  $(`${name}-tab`).addEventListener("click", () => selectResultTab(name, false));
  $(`${name}-tab`).addEventListener("keydown", (event) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const next = event.key === "ArrowLeft" || event.key === "Home" ? "output" : "agent";
    selectResultTab(next);
  });
}

for (const control of [$("where"), $("gpu"), $("timeout")]) {
  control.addEventListener("change", refresh);
}

for (const toggle of document.querySelectorAll(".section-toggle")) {
  toggle.addEventListener("click", () => {
    toggle.setAttribute("aria-expanded", String(toggle.getAttribute("aria-expanded") !== "true"));
  });
}

for (const button of document.querySelectorAll("[data-example]")) {
  button.addEventListener("click", () => {
    const item = examples[button.dataset.example];
    setDocument(item.name, item.code);
    document.querySelectorAll(".file-item.active").forEach((entry) => entry.classList.remove("active"));
    button.classList.add("active");
    renderSaved();
  });
}

$("file-upload").addEventListener("change", async (event) => {
  const file = event.target.files?.[0];
  if (!file) return;
  if (file.size > 1024 * 1024) {
    setOutput("", "That file is larger than 1 MB. Open a smaller source file.");
    return;
  }
  document.querySelectorAll(".file-item.active").forEach((item) => item.classList.remove("active"));
  setDocument(file.name || "uploaded.py", await file.text(), { clean: false });
  renderSaved();
  event.target.value = "";
});

code.addEventListener("input", () => {
  clearDiagnostic();
  markDirty();
  repaint();
  refresh();
});
code.addEventListener("scroll", () => {
  $("line-numbers").scrollTop = code.scrollTop;
  updateCursor();
});
for (const eventName of ["click", "keyup", "select", "focus"]) {
  code.addEventListener(eventName, updateCursor);
}
code.addEventListener("keydown", (event) => {
  if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
    event.preventDefault();
    execute();
    return;
  }
  if ((event.metaKey || event.ctrlKey) && event.key === "/") {
    event.preventDefault();
    toggleComment();
    return;
  }
  if (event.key === "Tab") {
    event.preventDefault();
    indentSelection(event.shiftKey);
    return;
  }
  if (handleClosing(event)) return;
  handlePairs(event);
});

document.addEventListener("keydown", (event) => {
  if (!(event.metaKey || event.ctrlKey) || event.key.toLowerCase() !== "s") return;
  event.preventDefault();
  saveCurrent();
});

window.addEventListener("resize", updateCursor);
initSplitters();
renderSaved();
if (!await loadSharedSnippet()) {
  refresh();
  updateCursor();
}
$("agent-auth").hidden = SIGNED_IN;
$("agent-workspace").hidden = !SIGNED_IN;
renderCostView("output");

// Keep the anonymous path explicit in the UI even if future planning rules
// evolve: a signed-out visitor always starts on free local compute.
if (!SIGNED_IN && $("where").value === "auto") {
  updateCostView("output", { detail: "No account required" });
}
