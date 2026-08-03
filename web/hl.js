/*!
 * hl.js — the syntax highlighter mojojojo's pages and editor use.
 *
 * Dracula colours, no dependencies, no build step, ~4KB. It exists instead of
 * a CDN highlighter because every byte of this site is served from one box and
 * a third-party script tag on the page that runs your code is a bad trade.
 *
 *   import { highlight, highlightAll, attachEditor } from "/hl.js"
 *   highlightAll()                      // every <pre data-lang="python">
 *   attachEditor(textarea, "python")    // live highlight behind a textarea
 *
 * The tokenizer is deliberately shallow: one pass of alternation per language,
 * then a small fix-up pass that only needs the previous and next token (is this
 * name a def, a call, an attribute?). It is a highlighter, not a parser, so it
 * is allowed to be wrong about `x = "#"` inside three levels of nesting — it is
 * not allowed to be slow, and it is not allowed to emit unescaped input.
 */

const KEYWORDS = {
  python: new Set(("False None True and as assert async await break class continue def del " +
    "elif else except finally for from global if import in is lambda nonlocal not or pass " +
    "raise return try while with yield match case").split(" ")),
  js: new Set(("await async break case catch class const continue debugger default delete do " +
    "else export extends finally for from function get if import in instanceof let new of " +
    "return set static super switch this throw try typeof var void while with yield").split(" ")),
  bash: new Set(("if then else elif fi for while do done case esac in function return export " +
    "local source set unset trap exit").split(" ")),
  // Mojo is Python plus a declaration vocabulary. Everything in the Python set
  // is legal here too, so the two are unioned at the bottom of this file rather
  // than duplicated.
  mojo: new Set(("alias comptime fn var struct trait ref owned inout borrowed " +
    "raises capturing abi out self Self implements requires parameter").split(" ")),
};

// Names that read as values rather than variables — Dracula paints these the
// same purple as numbers, which is what makes `None` and `0` scan alike.
const CONSTANTS = {
  python: new Set(["True", "False", "None", "NotImplemented", "Ellipsis", "__name__", "__main__"]),
  js: new Set(["true", "false", "null", "undefined", "NaN", "Infinity", "this"]),
  bash: new Set([]),
  mojo: new Set(["True", "False", "None"]),
};

const BUILTINS = {
  python: new Set(("abs all any bin bool bytes callable chr dict dir divmod enumerate eval exec " +
    "filter float format frozenset getattr hasattr hash hex id input int isinstance issubclass " +
    "iter len list map max min next object oct open ord pow print range repr reversed round set " +
    "setattr slice sorted str sum super tuple type vars zip self cls").split(" ")),
  js: new Set(("console document window fetch Promise Array Object String Number Boolean Math " +
    "JSON Map Set Date RegExp Error performance globalThis").split(" ")),
  bash: new Set(("curl echo cd ls cat grep rg python python3 pip uv go node npm bun git sudo " +
    "systemctl journalctl make docker gunicorn uvicorn pytest mojojojo app").split(" ")),
  // The types and intrinsics a Mojo snippet on this site actually mentions.
  mojo: new Set(("Int Int8 Int16 Int32 Int64 UInt UInt8 UInt64 Float16 Float32 " +
    "Float64 Bool String SIMD DType UnsafePointer AnyOrigin List Tuple " +
    "Optional len range abs min max print alloc free store load reduce_add " +
    "parallelize simd_width_of").split(" ")),
};

// Per-language token patterns, tried in order at each position. The first that
// matches at the cursor wins; anything that matches nothing is emitted raw.
const RULES = {
  python: [
    ["com", /#[^\n]*/y],
    ["str", /(?:[rRbBuUfF]{0,2})(?:"""[\s\S]*?"""|'''[\s\S]*?'''|"(?:\\[\s\S]|[^"\\\n])*"|'(?:\\[\s\S]|[^'\\\n])*')/y],
    ["dec", /@[A-Za-z_][\w.]*/y],
    ["num", /(?:0[xXbBoO][0-9a-fA-F_]+|(?:\d[\d_]*(?:\.\d[\d_]*)?|\.\d[\d_]*)(?:[eE][+-]?\d+)?[jJ]?)/y],
    ["nam", /[A-Za-z_]\w*/y],
    ["op", /(?:\*\*|\/\/|<<|>>|[<>=!+\-*/%&|^~]=?|->|:=)/y],
    ["pun", /[()[\]{},.;:]/y],
  ],
  js: [
    ["com", /\/\/[^\n]*|\/\*[\s\S]*?\*\//y],
    ["str", /`(?:\\[\s\S]|[^`\\])*`|"(?:\\[\s\S]|[^"\\\n])*"|'(?:\\[\s\S]|[^'\\\n])*'/y],
    ["num", /(?:0[xX][0-9a-fA-F_]+|(?:\d[\d_]*(?:\.\d[\d_]*)?|\.\d[\d_]*)(?:[eE][+-]?\d+)?n?)/y],
    ["nam", /[A-Za-z_$][\w$]*/y],
    ["op", /(?:=>|===|!==|\*\*|\?\?|\|\||&&|[<>=!+\-*/%&|^~?]=?)/y],
    ["pun", /[()[\]{},.;:]/y],
  ],
  bash: [
    ["com", /#[^\n]*/y],
    ["str", /"(?:\\[\s\S]|[^"\\])*"|'[^']*'/y],
    ["var", /\$(?:\{[^}]*\}|[A-Za-z_]\w*|[@?#!$*0-9])/y],
    ["flg", /--?[A-Za-z][\w-]*/y],
    ["num", /\b\d+\b/y],
    ["nam", /[A-Za-z_][\w.\/-]*/y],
    ["op", /[|&<>=!]+/y],
    ["pun", /[()[\]{},;:\\]/y],
  ],
  json: [
    ["str", /"(?:\\[\s\S]|[^"\\])*"/y],
    ["num", /-?(?:\d+(?:\.\d+)?)(?:[eE][+-]?\d+)?/y],
    ["nam", /\b(?:true|false|null)\b/y],
    ["pun", /[()[\]{},.;:]/y],
  ],
  html: [
    ["com", /<!--[\s\S]*?-->/y],
    ["kw", /<\/?[A-Za-z][\w:-]*/y],
    ["str", /"(?:\\[\s\S]|[^"\\])*"|'(?:\\[\s\S]|[^'\\])*'/y],
    ["prop", /[A-Za-z_:][\w:.-]*(?=\s*=)/y],
    ["op", /\/?>|=/y],
  ],
};

// File trees and protocol transcripts are intentionally plain, but still get
// the same completed marker as tokenised blocks. This keeps highlightAll's
// contract true for every language label the docs advertise.
RULES.text = RULES.plain = [];
RULES.xml = RULES.html;

RULES.python3 = RULES.py = RULES.mojo = RULES.python;
RULES.javascript = RULES.mjs = RULES.js;
RULES.sh = RULES.shell = RULES.console = RULES.bash;
RULES.ps1 = RULES.powershell = RULES.bash;
RULES.http = RULES.bash;

// Mojo accepts Python's keywords as well as its own.
for (const word of KEYWORDS.python) KEYWORDS.mojo.add(word);
for (const word of BUILTINS.python) BUILTINS.mojo.add(word);

export const LANGS = Object.keys(RULES);

const ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" };
function esc(s) {
  return s.replace(/[&<>"]/g, (c) => ESC[c]);
}

/** Split `code` into {kind, text} tokens. Exported for the tests. */
export function tokenize(code, lang) {
  const rules = RULES[lang];
  if (!rules) return [{ kind: "", text: code }];
  if (!rules.length) return [{ kind: "", text: code }];
  const kw = KEYWORDS[lang] || KEYWORDS[baseLang(lang)] || new Set();
  const con = CONSTANTS[lang] || CONSTANTS[baseLang(lang)] || new Set();
  const bui = BUILTINS[lang] || BUILTINS[baseLang(lang)] || new Set();

  const out = [];
  let i = 0;
  while (i < code.length) {
    let matched = false;
    for (const [kind, re] of rules) {
      re.lastIndex = i;
      const m = re.exec(code);
      if (m && m.index === i && m[0].length) {
        out.push({ kind, text: m[0] });
        i += m[0].length;
        matched = true;
        break;
      }
    }
    if (!matched) {
      // Whitespace and anything unrecognised: take it verbatim, one run at a
      // time, so a stray character can never stall the loop.
      const ws = /\s+/y;
      ws.lastIndex = i;
      const m = ws.exec(code);
      const text = m && m.index === i ? m[0] : code[i];
      out.push({ kind: "", text });
      i += text.length;
    }
  }
  return interpolate(resolve(out, { kw, con, bui, lang: baseLang(lang) }), baseLang(lang));
}

// `curl -H "Bearer $MOJOJOJO_KEY"` is the first thing on the front page, and a
// $VAR that vanishes into a yellow string is exactly the character the reader
// has to replace. Double-quoted shell strings interpolate; single-quoted ones
// do not, and neither does anything else here.
const SHELL_VAR = /\$(?:\{[^}]*\}|[A-Za-z_]\w*)/g;

function interpolate(tokens, lang) {
  if (lang !== "bash") return tokens;
  const out = [];
  for (const t of tokens) {
    if (t.kind !== "str" || !t.text.startsWith('"') || !SHELL_VAR.test(t.text)) {
      out.push(t);
      continue;
    }
    SHELL_VAR.lastIndex = 0;
    let at = 0;
    for (const m of t.text.matchAll(SHELL_VAR)) {
      if (m.index > at) out.push({ kind: "str", text: t.text.slice(at, m.index) });
      out.push({ kind: "var", text: m[0] });
      at = m.index + m[0].length;
    }
    if (at < t.text.length) out.push({ kind: "str", text: t.text.slice(at) });
  }
  return out;
}

function baseLang(lang) {
  // `mojo` shares Python's token patterns but has its own keyword sets, so it
  // must not collapse into "python" the way `py` does.
  if (lang === "mojo") return "mojo";
  if (RULES[lang] === RULES.python) return "python";
  if (RULES[lang] === RULES.js) return "js";
  if (RULES[lang] === RULES.bash) return "bash";
  return lang;
}

// Names are ambiguous until you look one token either side: `def foo` is a
// definition, `foo(` is a call, `.foo` is an attribute, `foo` alone is a
// variable and gets no colour at all — colouring everything is how a
// highlighter turns into a ransom note.
function resolve(tokens, { kw, con, bui, lang }) {
  const meaningful = (j, step) => {
    for (let k = j + step; k >= 0 && k < tokens.length; k += step) {
      if (tokens[k].kind !== "" || tokens[k].text.trim()) return tokens[k];
    }
    return null;
  };
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (lang === "json") {
      // A string in key position is the only thing JSON needs told apart.
      if (t.kind === "str") {
        const after = meaningful(i, +1);
        if (after && after.text === ":") t.kind = "key";
      } else if (t.kind === "nam") {
        t.kind = "con";
      }
      continue;
    }
    if (t.kind !== "nam") continue;
    const prev = meaningful(i, -1);
    const next = meaningful(i, +1);
    const raw = tokens[i - 1];
    const startsLine = i === 0 || (raw && raw.kind === "" && raw.text.includes("\n"));
    if (con.has(t.text)) t.kind = "con";
    else if (kw.has(t.text)) t.kind = "kw";
    else if (prev && prev.kind === "kw" && (prev.text === "def" || prev.text === "function")) t.kind = "fn";
    else if (prev && prev.kind === "kw" && prev.text === "class") t.kind = "cls";
    else if (prev && prev.text === ".") t.kind = next && next.text === "(" ? "fn" : "prop";
    else if (next && next.text === "(") t.kind = bui.has(t.text) ? "bui" : "fn";
    else if (bui.has(t.text)) t.kind = "bui";
    else if (lang === "bash" && (startsLine || !prev || prev.text === "|" || prev.text === ";")) t.kind = "bui";
    else t.kind = "";
    // A JS/Python type-ish name (Capitalised, not called) reads as a class.
    if (t.kind === "" && /^[A-Z][A-Za-z0-9_]*$/.test(t.text) && lang !== "bash") t.kind = "cls";
  }
  return tokens;
}

/** Highlight `code` as `lang`, returning HTML. Input is always escaped. */
export function highlight(code, lang) {
  let out = "";
  for (const t of tokenize(code, lang)) {
    out += t.kind ? `<span class="t-${t.kind}">${esc(t.text)}</span>` : esc(t.text);
  }
  return out;
}

/**
 * Highlight every `<pre data-lang="...">` under `root`, once. A block is
 * marked done so a second call (or an element re-inserted by a page script)
 * cannot double-escape its own output.
 */
export function highlightAll(root) {
  const scope = root || (typeof document !== "undefined" ? document : null);
  if (!scope) return 0;
  let n = 0;
  for (const el of scope.querySelectorAll("pre[data-lang], code[data-lang]")) {
    if (el.dataset.hl === "done") continue;
    const lang = el.dataset.lang;
    if (!RULES[lang]) continue;
    el.innerHTML = highlight(el.textContent, lang);
    el.dataset.hl = "done";
    n += 1;
  }
  return n;
}

// Styles that have to be identical on the textarea and the layer behind it, or
// the caret drifts away from the text it is supposed to be sitting in.
const MIRRORED = [
  "fontFamily", "fontSize", "fontWeight", "fontStyle", "lineHeight", "letterSpacing",
  "wordSpacing", "textIndent", "textTransform", "paddingTop", "paddingRight",
  "paddingBottom", "paddingLeft", "borderTopWidth", "borderRightWidth",
  "borderBottomWidth", "borderLeftWidth", "borderStyle", "borderRadius", "tabSize",
  "boxSizing", "whiteSpace", "overflowWrap", "wordBreak",
];

/**
 * Put a highlighted layer behind `textarea` and keep the two in step.
 *
 * The textarea stays the real control — every keystroke, selection, undo and
 * accessibility affordance is the browser's, not ours — with transparent text
 * and a visible caret over a <pre> that carries the colour. Returns a repaint
 * function for callers that change the value programmatically.
 */
export function attachEditor(textarea, lang) {
  if (!textarea || textarea.dataset.hlEditor === "on") return () => {};
  if (!RULES[lang]) return () => {};
  const cs = getComputedStyle(textarea);

  const wrap = document.createElement("div");
  wrap.className = "hl-wrap";
  wrap.style.position = "relative";
  textarea.parentNode.insertBefore(wrap, textarea);

  const layer = document.createElement("pre");
  layer.className = "hl-layer";
  layer.setAttribute("aria-hidden", "true");
  layer.style.position = "absolute";
  layer.style.inset = "0";
  layer.style.margin = "0";
  layer.style.overflow = "hidden";
  layer.style.pointerEvents = "none";
  layer.style.background = cs.backgroundColor;
  layer.style.borderColor = "transparent";
  layer.style.color = "var(--ink)";
  for (const prop of MIRRORED) layer.style[prop] = cs[prop];
  // A textarea wraps like this whatever the author asked for; the layer has to
  // agree or long lines will sit on different rows in the two elements.
  layer.style.whiteSpace = "pre-wrap";
  layer.style.overflowWrap = "break-word";

  wrap.appendChild(layer);
  wrap.appendChild(textarea);
  textarea.style.position = "relative";
  textarea.style.background = "transparent";
  textarea.style.color = "transparent";
  textarea.style.caretColor = cs.color === "rgba(0, 0, 0, 0)" ? "#f8f8f2" : cs.color;
  textarea.dataset.hlEditor = "on";

  let dead = false;
  const paint = () => {
    if (dead) return;
    try {
      // The trailing newline keeps the last (empty) line rendered, so scrolling
      // to the bottom of the textarea does not scroll past the end of the layer.
      layer.innerHTML = highlight(textarea.value + "\n", lang);
    } catch (err) {
      // Colour is a nicety; a code box whose text you cannot see is not. If the
      // tokenizer ever throws, give the textarea its own ink back and stop.
      dead = true;
      textarea.style.color = cs.color;
      layer.remove();
      console.error("hl: highlighting disabled —", err);
      return;
    }
    layer.scrollTop = textarea.scrollTop;
    layer.scrollLeft = textarea.scrollLeft;
  };
  const sync = () => {
    layer.scrollTop = textarea.scrollTop;
    layer.scrollLeft = textarea.scrollLeft;
  };
  textarea.addEventListener("input", paint);
  textarea.addEventListener("scroll", sync);
  // The textarea is resizable and in normal flow, so the wrapper follows its
  // height on its own; what does not follow is the scroll offset after a
  // resize, and a layer one line out of step is worse than no colour at all.
  if (typeof ResizeObserver !== "undefined") {
    new ResizeObserver(sync).observe(textarea);
  }
  paint();
  return paint;
}

export default { highlight, highlightAll, attachEditor, tokenize, LANGS };

// Plain <script src> users get the same entry points without an import.
if (typeof window !== "undefined") {
  window.hl = { highlight, highlightAll, attachEditor, tokenize, LANGS };
}
