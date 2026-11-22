/* airadminer client: plain JavaScript, no build step, no dependencies. */
"use strict";
(() => {
const E2E = window.__AA_E2E;

// ============================================================ utilities

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
const ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ESC[c]);
// qid quotes a PostgreSQL identifier unless it is plain lower case.
const qid = (s) => (/^[a-z_][a-z0-9_$]*$/.test(s) ? s : `"${s.replace(/"/g, `""`)}"`);
const icon = (id, cls = "") => `<svg class="i ${cls}" aria-hidden="true"><use href="#${id}"/></svg>`;
const isMac = /Mac|iPhone|iPad/.test(navigator.platform);
const MOD = isMac ? "⌘" : "Ctrl";
const fmtN = (n) => Number(n).toLocaleString();
const compact = (n) => {
  if (n == null || n < 0) return "";
  if (n < 1000) return String(n);
  if (n < 1e6) return (n / 1e3).toFixed(n < 1e4 ? 1 : 0).replace(/\.0$/, "") + "k";
  if (n < 1e9) return (n / 1e6).toFixed(n < 1e7 ? 1 : 0).replace(/\.0$/, "") + "M";
  return (n / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
};
const fmtBytes = (b) => {
  if (b == null) return "";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (i ? b.toFixed(b < 10 ? 1 : 0) : b) + " " + u[i];
};
const plural = (n, one, many) => `${fmtN(n)} ${n === 1 ? one : many || one + "s"}`;
const debounce = (fn, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; };
const html = (s) => { const t = document.createElement("template"); t.innerHTML = s.trim(); return t.content.firstElementChild; };
const store = {
  get(k, d) { try { const v = localStorage.getItem(k); return v == null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch {} },
};
const hue = (s) => { let h = 0; for (const ch of String(s)) h = (h * 31 + ch.charCodeAt(0)) >>> 0; return h % 360; };

async function copyText(text) {
  try { await navigator.clipboard.writeText(text); return; } catch {}
  const ta = document.createElement("textarea");
  ta.value = text; ta.style.position = "fixed"; ta.style.opacity = "0";
  document.body.appendChild(ta); ta.select();
  try { document.execCommand("copy"); } finally { ta.remove(); }
}

// ============================================================ API

class ApiError extends Error {
  constructor(j, status) { super(j.error || "Request failed"); Object.assign(this, j); this.status = status; }
}

// Every request names the database the page was built for, so the server
// can refuse an edit after another tab switched this session elsewhere.
function apiHeaders(path) {
  const h = { "Content-Type": "application/json", "X-Airadminer": "1" };
  if (S.meta && path !== "login" && path !== "meta") h["X-Airadminer-Db"] = encodeURIComponent(S.meta.database);
  return h;
}

let reloadingDB = false;
async function dbChanged() {
  if (reloadingDB) return;
  reloadingDB = true;
  try {
    S.meta = await api("meta");
    if (S.view && S.view.destroy) S.view.destroy();
    S.view = null;
    location.hash = "";
    await renderShell();
    toast(`This session was switched to database ${S.meta.database} in another tab`, { err: true });
  } catch (e) { toastErr(e); } finally { reloadingDB = false; }
}

// A row whose long values the server cut ends with {cut: {index: md5}}.
// It becomes row.cut, so every index of the row still names a column.
function unpackRow(r) {
  if (!Array.isArray(r) || !r.length) return;
  const l = r[r.length - 1];
  if (l && typeof l === "object" && !Array.isArray(l)) { r.pop(); r.cut = l.cut; }
}
const unpackRows = (rows) => { if (Array.isArray(rows)) rows.forEach(unpackRow); };
const isCut = (row, i) => !!(row && row.cut && row.cut[i] != null);

async function api(path, body, { signal, raw } = {}) {
  let r;
  try {
    r = await fetch("api/" + path, {
      method: "POST", credentials: "same-origin", signal,
      headers: apiHeaders(path),
      body: JSON.stringify(body || {}),
    });
  } catch (e) {
    if (e.name === "AbortError") throw e;
    throw new ApiError({ error: "Cannot reach the airadminer server. Check that it is still running." }, 0);
  }
  if (raw && r.ok) return r;
  let j;
  try { j = await r.json(); } catch { j = { error: `The server answered with HTTP ${r.status}` }; }
  if (!r.ok) {
    const err = new ApiError(j, r.status);
    if (r.status === 401 && path !== "login") signedOut();
    if (r.status === 409 && j.code === "dbchanged") dbChanged();
    throw err;
  }
  if (path !== "query" && path !== "cells") { unpackRows(j.rows); unpackRow(j.row); unpackRows(j.updated); unpackRows(j.inserted); }
  return j;
}

// ============================================================ toasts, popovers, dialogs

function toast(msg, o = {}) {
  const t = html(`<div class="toast ${o.err ? "err" : ""}"><div>${esc(msg)}${o.detail ? `<small>${esc(o.detail)}</small>` : ""}</div></div>`);
  if (o.action) {
    const b = html(`<button>${esc(o.action.label)}</button>`);
    b.onclick = () => { t.remove(); o.action.fn(); };
    t.appendChild(b);
  }
  $("#toasts").appendChild(t);
  setTimeout(() => t.remove(), o.ms || (o.err ? 7000 : 3200));
}
function toastErr(e) {
  if (e && e.name === "AbortError") return;
  if (e && e.status === 401) return;
  toast(e.message || String(e), { err: true, detail: [e.detail, e.hint && "Hint: " + e.hint].filter(Boolean).join("  ") });
}

let curPop = null;
function closePop() {
  if (!curPop) return;
  const p = curPop; curPop = null;
  p.el.remove();
  document.removeEventListener("mousedown", p.out, true);
  document.removeEventListener("keydown", p.key, true);
  window.removeEventListener("resize", p.close);
  p.onClose && p.onClose();
}
function openPop(anchor, el, { align = "left", onClose, cls = "" } = {}) {
  closePop();
  el.classList.add("pop");
  if (cls) el.classList.add(...cls.split(" "));
  document.body.appendChild(el);
  const place = () => {
    const r = anchor.getBoundingClientRect();
    const w = el.offsetWidth, h = el.offsetHeight;
    let left = align === "right" ? r.right - w : r.left;
    left = Math.max(8, Math.min(left, innerWidth - w - 8));
    let top = r.bottom + 6;
    if (top + h > innerHeight - 8 && r.top - h - 6 > 8) top = r.top - h - 6;
    el.style.left = left + "px";
    el.style.top = Math.max(8, top) + "px";
  };
  place();
  const p = {
    el, onClose, place, close: closePop,
    out: (e) => { if (!el.contains(e.target) && !(anchor.contains && anchor.contains(e.target))) closePop(); },
    key: (e) => { if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closePop(); anchor.focus && anchor.focus(); } },
  };
  document.addEventListener("mousedown", p.out, true);
  document.addEventListener("keydown", p.key, true);
  window.addEventListener("resize", p.close);
  curPop = p;
  return p;
}

// Keyboard navigation for a list of .mi buttons inside a popover.
function menuKeys(root, input) {
  let hi = -1;
  const items = () => $$(".mi:not([disabled])", root);
  const paint = () => items().forEach((b, i) => b.classList.toggle("hi", i === hi));
  (input || root).addEventListener("keydown", (e) => {
    const list = items();
    if (e.key === "ArrowDown") { e.preventDefault(); hi = Math.min(list.length - 1, hi + 1); paint(); list[hi]?.scrollIntoView({ block: "nearest" }); }
    else if (e.key === "ArrowUp") { e.preventDefault(); hi = Math.max(0, hi - 1); paint(); list[hi]?.scrollIntoView({ block: "nearest" }); }
    else if (e.key === "Enter" && hi >= 0 && list[hi]) { e.preventDefault(); list[hi].click(); }
  });
  return { reset(i = -1) { hi = i; paint(); }, get index() { return hi; } };
}

function dialog({ title, body, ok = "Delete", cancel = "Cancel", danger = true }) {
  if (E2E && E2E.confirm) return Promise.resolve(true);
  return new Promise((resolve) => {
    const scrim = html(`<div class="scrim" style="z-index:60"></div>`);
    const d = html(`<div class="pop dialog" role="alertdialog" aria-modal="true">
      <h3>${esc(title)}</h3><p>${esc(body)}</p>
      <div class="acts"><button class="btn line" data-no>${esc(cancel)}</button><button class="btn ${danger ? "solid-danger" : "primary"}" data-ok>${esc(ok)}</button></div></div>`);
    const prev = document.activeElement;
    const done = (v) => { scrim.remove(); d.remove(); document.removeEventListener("keydown", key, true); prev && prev.focus && prev.focus(); resolve(v); };
    const key = (e) => { if (e.key === "Escape") { e.stopPropagation(); done(false); } };
    scrim.onclick = () => done(false);
    $("[data-no]", d).onclick = () => done(false);
    $("[data-ok]", d).onclick = () => done(true);
    document.addEventListener("keydown", key, true);
    document.body.append(scrim, d);
    $("[data-ok]", d).focus();
  });
}

// ============================================================ data types

const KIND_ICON = {
  text: "t-text", num: "t-num", date: "t-date", datetime: "t-date", time: "t-time", bool: "t-bool",
  json: "t-json", uuid: "t-id", enum: "t-enum", array: "t-array", blob: "t-blob", range: "t-range",
  net: "t-net", link: "i-link", other: "t-other",
};

// ---------- default column widths
//
// A column is as wide as its values, not its name: the header makes room for
// at most HEAD_NAME_MAX px of the name (the rest is cut, the full name is in
// the tooltip), so a flag called battery_powered stays narrow. Explicit
// "Fit width to content" still shows the whole name.

const HEAD_NAME_MAX = 72;
const COL_MIN = 56, COL_MAX = 340;
// Before any rows load: a guess per kind, replaced once the first page arrives.
const KIND_GUESS = {
  num: 72, bool: 40, date: 96, datetime: 190, time: 90, uuid: 290, json: 240, text: 180,
  enum: 100, array: 180, link: 80, blob: 80, range: 240, net: 130, other: 160,
};

let measureCtx = null, measureFonts = null;
function textW(s, font) {
  measureCtx ||= document.createElement("canvas").getContext("2d");
  if (!measureFonts) {
    const cs = getComputedStyle(document.documentElement);
    measureFonts = { sans: cs.getPropertyValue("--font").trim() || "sans-serif", mono: cs.getPropertyValue("--mono").trim() || "monospace" };
  }
  measureCtx.font = font.replace("SANS", measureFonts.sans).replace("MONO", measureFonts.mono);
  return measureCtx.measureText(s).width;
}

// Width the header needs: type icon, name (capped unless full) and one slot
// for the key icon or sort arrow (they share it), plus padding and gaps.
function headerWidth(col, full) {
  const name = textW(col.label, "550 12.5px SANS");
  return 42 + (full ? name : Math.min(name, HEAD_NAME_MAX)) + 18;
}

// Drawn width of one value, without cell padding (mirrors renderValue).
function valueWidth(kind, v) {
  if (v === null || v === undefined) return textW("null", "italic 12px MONO");
  const s = String(v).slice(0, 120);
  switch (kind) {
    case "bool": return 16;
    case "num": return textW(s, "12.5px MONO");
    case "enum": return textW(s, "550 12px SANS") + 16;
    case "link": return textW(s, "550 12.5px SANS") + 34;
    case "datetime": {
      const m = /^(\S+) (\d\d:\d\d)(:\d\d)?(\.\d+)?(.*)$/.exec(s);
      if (!m) return textW(s, "13px SANS");
      const secs = m[3] && m[3] !== ":00" ? m[3] : "";
      return textW(m[1], "13px SANS") + 6 + textW(m[2] + secs + (m[5] ? " " + m[5] : ""), "13px SANS");
    }
    case "uuid": case "json": case "other": case "range": return textW(s, "12px MONO");
    case "blob": return 60;
    case "array": {
      const items = parseArray(String(v));
      if (!items) return textW(s, "12px MONO");
      if (!items.length) return textW("empty list", "13px SANS");
      return items.slice(0, 6).reduce((w, x) => w + 4 + 16 + textW(x === null ? "null" : String(x), "550 12px SANS"), -4);
    }
    // Single-line rows show line breaks as " ↵ " (see renderValue).
    default: return textW(s.replace(/\r?\n/g, " ↵ "), "13px SANS");
  }
}

// Default width of a column from a sample of rows (none: a guess by kind).
function columnWidth(col, rows) {
  const pad = 2 * (S.density === "compact" ? 6 : 10) + 4;
  let content = KIND_GUESS[col.kind] || 160;
  if (rows && rows.length) {
    content = 0;
    for (const row of rows) if (row) content = Math.max(content, valueWidth(col.kind, row[col.idx]));
  }
  return Math.round(Math.max(COL_MIN, Math.min(COL_MAX, Math.max(content + pad, headerWidth(col)))));
}

function kindOf(c) {
  if (c.fk) return "link";
  const udt = c.udt || "", cat = c.cat || "";
  if (c.enum || cat === "E") return "enum";
  if (cat === "B") return "bool";
  if (cat === "N" && udt !== "oid") return "num";
  if (udt === "date") return "date";
  if (udt === "time" || udt === "timetz" || udt === "interval") return "time";
  if (cat === "D") return "datetime";
  if (cat === "T") return "time";
  if (udt === "json" || udt === "jsonb") return "json";
  if (udt === "uuid") return "uuid";
  if (udt === "bytea") return "blob";
  if (cat === "A") return "array";
  if (cat === "R") return "range";
  if (cat === "I") return "net";
  if (cat === "S") return "text";
  return "other";
}
function kindFromTypeName(t) {
  if (!t) return "text";
  if (t.endsWith("[]")) return "array";
  if (/range$/.test(t)) return "range";
  if (/^(integer|bigint|smallint|numeric|real|double precision|money)/.test(t)) return "num";
  if (t === "boolean") return "bool";
  if (t === "date") return "date";
  if (t.startsWith("timestamp")) return "datetime";
  if (t.startsWith("time") || t === "interval") return "time";
  if (t === "json" || t === "jsonb") return "json";
  if (t === "uuid") return "uuid";
  if (t === "bytea") return "blob";
  if (/^(inet|cidr|macaddr)/.test(t)) return "net";
  if (/^(text|character|name|citext|"char")/.test(t)) return "text";
  return "other";
}
const isTrue = (v) => v === "t" || v === "true";

// Parses a one-dimensional PostgreSQL array literal. Returns null for anything fancier.
function parseArray(s) {
  if (!s || s[0] !== "{" || s[s.length - 1] !== "}" || s[1] === "{") return null;
  const out = []; let i = 1, cur = "", quoted = false, inq = false;
  for (; i < s.length - 1; i++) {
    const ch = s[i];
    if (inq) {
      if (ch === "\\") cur += s[++i];
      else if (ch === '"') inq = false;
      else cur += ch;
    } else if (ch === '"') { inq = true; quoted = true; }
    else if (ch === ",") { out.push(!quoted && cur === "NULL" ? null : cur); cur = ""; quoted = false; }
    else cur += ch;
  }
  if (s.length > 2) out.push(!quoted && cur === "NULL" ? null : cur);
  return out;
}

function renderValue(col, v, tall) {
  if (v === null || v === undefined) return `<span class="v v-null">null</span>`;
  switch (col.kind) {
    case "num": return `<span class="v v-num">${esc(v)}</span>`;
    case "bool": return `<span class="v v-bool"><span class="chk ${isTrue(v) ? "on" : ""}">${isTrue(v) ? icon("i-check") : ""}</span></span>`;
    case "enum": {
      const i = col.enum ? col.enum.indexOf(v) : -1;
      const h = i >= 0 ? (i * 67 + 205) % 360 : hue(v);
      return `<span class="v pills"><span class="pill" style="--h:${h}">${esc(v)}</span></span>`;
    }
    case "link": return `<span class="v"><button class="link" data-link tabindex="-1">${icon("i-link")}<span>${esc(v)}</span></button></span>`;
    case "date": return `<span class="v v-date">${esc(v)}</span>`;
    case "datetime": {
      const m = /^(\S+) (\d\d:\d\d)(:\d\d)?(\.\d+)?(.*)$/.exec(v);
      if (!m) return `<span class="v v-date">${esc(v)}</span>`;
      const secs = m[3] && m[3] !== ":00" ? m[3] : "";
      return `<span class="v v-date">${esc(m[1])}<span class="t">${esc(m[2] + secs + (m[5] ? " " + m[5] : ""))}</span></span>`;
    }
    case "json": {
      const s = esc(v.length > 600 ? v.slice(0, 600) + "…" : v);
      return `<span class="v v-json">${s.replace(/(&quot;(?:[^&\\]|\\.|&(?!quot;))*?&quot;)(\s*:)/g, '<span class="k">$1</span>$2')}</span>`;
    }
    case "uuid": return `<span class="v v-id">${esc(v)}</span>`;
    case "array": {
      const items = parseArray(v);
      if (!items) return `<span class="v v-mono">${esc(v)}</span>`;
      if (!items.length) return `<span class="v v-empty">empty list</span>`;
      return `<span class="v pills">${items.slice(0, 24).map((x) => x === null ? `<span class="pill flat v-null">null</span>` : `<span class="pill" style="--h:${hue(x)}">${esc(x)}</span>`).join("")}</span>`;
    }
    case "blob": {
      const n = v.startsWith("\\x") ? (v.length - 2) / 2 : Number(v);
      return `<span class="v v-blob">${icon("t-blob", "xs")}${esc(fmtBytes(n))}</span>`;
    }
    case "text": {
      if (v === "") return `<span class="v v-empty">empty</span>`;
      const s = esc(v.length > 1200 ? v.slice(0, 1200) + "…" : v);
      return `<span class="v">${tall ? s : s.replace(/\r?\n/g, '<span class="v-nl"> ↵ </span>')}</span>`;
    }
    default: return `<span class="v v-mono">${esc(v.length > 600 ? v.slice(0, 600) + "…" : v)}</span>`;
  }
}

// The last block copied from a grid, with its NULLs. TSV has no NULL, so a
// paste of exactly this text uses the matrix instead of the text.
let lastCopy = null;
function copyMatrix(matrix) {
  const text = toTSV(matrix);
  lastCopy = { text: text.replace(/\r\n?/g, "\n"), matrix };
  return copyText(text);
}
function pastedMatrix(text) {
  return lastCopy && text.replace(/\r\n?/g, "\n") === lastCopy.text ? lastCopy.matrix : parseTSV(text);
}

// Re-indents JSON by changing whitespace only: numbers, strings and key
// order stay exactly as written (JSON.parse would round big numbers).
function prettyJSON(src) {
  try { JSON.parse(src); } catch { return null; }
  let out = "", ind = 0;
  const nl = () => "\n" + "  ".repeat(ind);
  for (let i = 0; i < src.length; i++) {
    const ch = src[i];
    if (ch === '"') {
      let j = i + 1;
      while (j < src.length && src[j] !== '"') j += src[j] === "\\" ? 2 : 1;
      out += src.slice(i, j + 1); i = j;
    } else if (ch === " " || ch === "\t" || ch === "\n" || ch === "\r") {
      continue;
    } else if (ch === "{" || ch === "[") {
      let j = i + 1;
      while (" \t\n\r".includes(src[j])) j++;
      if (src[j] === (ch === "{" ? "}" : "]")) { out += ch + src[j]; i = j; continue; }
      ind++; out += ch + nl();
    } else if (ch === "}" || ch === "]") { ind--; out += nl() + ch; }
    else if (ch === ",") out += "," + nl();
    else if (ch === ":") out += ": ";
    else out += ch;
  }
  return out;
}

// TSV in the dialect spreadsheets use, so copy/paste round-trips with Sheets/Excel/Airtable.
function toTSV(matrix) {
  return matrix.map((row) => row.map((v) => {
    if (v == null) return "";
    return /[\t\n\r"]/.test(v) ? '"' + v.replace(/"/g, '""') + '"' : v;
  }).join("\t")).join("\n");
}
function parseTSV(text) {
  const rows = []; let row = [], cur = "", i = 0, q = false;
  text = text.replace(/\r\n?/g, "\n");
  if (text.endsWith("\n")) text = text.slice(0, -1);
  for (; i < text.length; i++) {
    const ch = text[i];
    if (q) {
      if (ch === '"' && text[i + 1] === '"') { cur += '"'; i++; }
      else if (ch === '"') q = false;
      else cur += ch;
    } else if (ch === '"' && cur === "") q = true;
    else if (ch === "\t") { row.push(cur); cur = ""; }
    else if (ch === "\n") { row.push(cur); rows.push(row); row = []; cur = ""; }
    else cur += ch;
  }
  row.push(cur); rows.push(row);
  return rows;
}
// With -csv-escape-formulas, a cell a spreadsheet would run as a formula
// gets a leading apostrophe, as server exports do.
const escapeFormula = (v) => v && /^[=+\-@\t\r]/.test(v) && !/^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$/.test(v) ? "'" + v : v;
function toCSV(header, rows) {
  const esc1 = (v) => S.meta && S.meta.csvEscape ? escapeFormula(v) : v;
  const q = (v) => { if (v == null) return ""; v = esc1(v); return v === "" || /[",\n\r]/.test(v) ? '"' + v.replace(/"/g, '""') + '"' : v; };
  return [header.map(q).join(","), ...rows.map((r) => r.map(q).join(","))].join("\n") + "\n";
}
function download(name, blob) {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob); a.download = name;
  document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 2000);
}

// ============================================================ grid

// Sizes per density (S.density). CSS mirrors HEAD_H in --head-h.
const HEAD_H = { comfortable: 34, compact: 28 };
const ROW_HEIGHTS = { comfortable: [33, 58, 96], compact: [26, 46, 78] };
// Narrowest a column gets when fitted to the screen; below this the grid scrolls.
const FIT_MIN = { comfortable: 56, compact: 44 };

// Scale widths so they sum to exactly `avail`, keeping proportions but never
// going below `min`. Returns null when even `min` per column does not fit.
function fitWidths(ws, avail, min) {
  const n = ws.length;
  if (!n || avail < n * min) return null;
  const out = new Array(n).fill(0), pinned = new Array(n).fill(false);
  for (;;) {
    let free = avail, sum = 0;
    for (let i = 0; i < n; i++) pinned[i] ? (free -= min) : (sum += ws[i]);
    const k = sum > 0 ? free / sum : 0;
    let changed = false;
    for (let i = 0; i < n; i++) {
      if (pinned[i]) { out[i] = min; continue; }
      out[i] = ws[i] * k;
      if (out[i] < min) { pinned[i] = true; changed = true; }
    }
    if (!changed) break;
  }
  // Whole pixels, with the rounding remainder on the last column, so the
  // total is exact and no 1px horizontal scrollbar appears.
  let used = 0;
  for (let i = 0; i < n - 1; i++) { out[i] = Math.floor(out[i]); used += out[i]; }
  out[n - 1] = avail - used;
  return out;
}

/*
 * Grid renders a virtual window of rows from a source:
 *   { count(), get(i) -> row array | undefined, ensure(first, last) }
 * Columns: { name, label, kind, idx, width, type, readonly, pk, enum, sortDir, filtered }
 */
class Grid {
  constructor(host, o) {
    this.o = o;
    this.cols = [];
    this.rhIndex = o.rh || 0;
    this.fit = !!o.fit;
    this.fitted = false;
    this.ws = [];
    this.sel = null;
    this.picked = new Set();
    this.drawn = null;
    this.flashes = new Map();
    host.innerHTML = `<div class="grid" tabindex="0" role="grid" aria-label="${esc(o.label || "Rows")}"><div class="g-scroll"><div class="g-head" role="row"></div><div class="g-body"></div></div></div><div class="grid-foot"></div>`;
    this.el = $(".grid", host);
    this.sc = $(".g-scroll", host);
    this.head = $(".g-head", host);
    this.body = $(".g-body", host);
    this.foot = $(".grid-foot", host);
    this.bind();
  }

  get rh() { return ROW_HEIGHTS[S.density][this.rhIndex]; }
  get headH() { return HEAD_H[S.density]; }

  // Columns (besides the frozen first one) that overlap the visible width,
  // plus a margin: only these get cells, so wide tables stay fast.
  colRange() {
    const cols = this.cols, sc = this.sc;
    const x0 = sc.scrollLeft - 240, x1 = sc.scrollLeft + sc.clientWidth + 240;
    let a = 1, b = cols.length - 1;
    while (a <= b && this.lefts[a] + this.ws[a] < x0) a++;
    while (b >= a && this.lefts[b] > x1) b--;
    return [a, b];
  }
  get rnw() { return this.o.pick ? Math.max(66, String(this.o.source.count()).length * 7 + 46) : Math.max(52, String(this.o.source.count()).length * 7 + 26); }

  setCols(cols) { this.cols = cols; this.renderHead(); this.layout(); this.draw(true); }
  setRowHeight(i) { this.rhIndex = i; this.layout(); this.draw(true); }
  setFit(on) { this.fit = !!on; this.layout(); this.draw(true); if (this.sel) this.scrollTo(this.sel.r2, this.sel.c2); }

  // Column widths as drawn: the saved widths, or in fit mode those widths
  // scaled to fill the visible width (saved widths act as proportions and
  // are never overwritten).
  layout() {
    const rnw = this.rnw, cols = this.cols;
    const n = this.o.source.count();
    // Body height first: it decides whether a vertical scrollbar takes width.
    this.body.style.height = n * this.rh + (this.o.onAdd ? 36 : 0) + 48 + "px";
    const fitted = this.fit && fitWidths(cols.map((c) => c.width), this.sc.clientWidth - rnw, FIT_MIN[S.density]);
    this.fitted = !!fitted;
    this.ws = fitted || cols.map((c) => c.width);
    this.lefts = [];
    let x = rnw;
    for (const w of this.ws) { this.lefts.push(x); x += w; }
    const st = this.sc.style;
    const fill = this.fitted ? "minmax(0px, 1fr)" : "minmax(60px, 1fr)";
    st.setProperty("--cols", `${rnw}px ${this.ws.map((w) => w + "px").join(" ")} ${fill}`);
    st.setProperty("--total", x + (this.fitted ? 0 : 60) + "px");
    st.setProperty("--rnw", rnw + "px");
    st.setProperty("--rh", this.rh + "px");
    this.el.classList.toggle("fitted", this.fitted);
  }

  renderHead() {
    const pickAll = this.o.pick ? `<button class="ck" data-pickall tabindex="-1" title="Select loaded rows"><span class="box"></span></button>` : "";
    this.head.innerHTML = `<div class="g-h g-rn">${pickAll}</div>` + this.cols.map((c, i) => `
      <div class="g-h${i === 0 ? " g-f0" : ""}${c.filtered ? " filtered" : ""}" data-c="${i}" role="columnheader" title="${esc(c.name + (c.type ? "  " + c.type : ""))}">
        ${icon(KIND_ICON[c.kind] || "t-other", "sm ty")}<span class="nm">${esc(c.label)}</span>
        ${c.sortDir ? icon(c.sortDir === "asc" ? "i-arrow-up" : "i-arrow-down", "xs sorted") : c.pk ? icon("i-key", "xs keyic") : ""}
        <span class="rs" data-rs></span></div>`).join("") + `<div class="g-h g-fill"></div>`;
  }

  range() {
    const n = this.o.source.count();
    const top = this.sc.scrollTop;
    const first = Math.max(0, Math.floor(top / this.rh) - 4);
    const last = Math.min(n - 1, Math.ceil((top + this.sc.clientHeight - this.headH) / this.rh) + 4);
    return [first, last];
  }

  draw(force) {
    const [first, last] = this.range();
    const [ca, cb] = this.colRange();
    const src = this.o.source;
    if (last >= first) src.ensure(Math.max(0, first - 20), last + 40);
    const d = this.drawn;
    if (!force && d && d[0] === first && d[1] === last && d[2] === ca && d[3] === cb) return;
    this.drawn = [first, last, ca, cb];
    const rh = this.rh, tall = this.rhIndex > 0, cols = this.cols;
    const now = performance.now();
    const cell = (r, c, row) => {
      const col = cols[c];
      const fl = this.flashes.get(r + ":" + col.name);
      const cls = (c === 0 ? " g-f0" : "") + (col.readonly ? " ro" : "") + (fl && fl > now ? " flash" : "");
      const v = row && row[col.idx];
      return `<div class="g-c${cls}" data-c="${c}" style="grid-column:${c + 2}" role="gridcell">${row ? renderValue(col, isCut(row, col.idx) ? v + "…" : v, tall) : `<span class="skel" style="width:${40 + ((r * 7 + c * 13) % 45)}%"></span>`}</div>`;
    };
    let h = "";
    for (let r = first; r <= last; r++) {
      const row = src.get(r);
      h += `<div class="g-row rh${this.rhIndex + 1}" data-r="${r}" style="top:${r * rh}px" role="row">`;
      h += `<div class="g-c g-rn" style="grid-column:1">${this.o.pick ? `<button class="ck" data-pick tabindex="-1" aria-label="Select row"><span class="box"></span></button>` : ""}<span class="num">${r + 1}</span>${this.o.onExpand ? `<button class="xp" data-exp tabindex="-1" title="Expand record (Space)">${icon("i-expand", "sm")}</button>` : ""}</div>`;
      if (cols.length) h += cell(r, 0, row);
      for (let c = ca; c <= cb; c++) h += cell(r, c, row);
      h += `<div class="g-c g-fill" style="grid-column:${cols.length + 2}"></div></div>`;
    }
    if (this.o.onAdd) h += `<div class="g-add" style="top:${src.count() * rh}px"><button data-add>${icon("i-plus", "sm")}Add record</button></div>`;
    this.body.innerHTML = h;
    this.paintSel();
  }

  refresh() { this.layout(); this.draw(true); }

  norm() {
    const s = this.sel;
    if (!s) return null;
    return { r0: Math.min(s.r, s.r2), r1: Math.max(s.r, s.r2), c0: Math.min(s.c, s.c2), c1: Math.max(s.c, s.c2), fr: s.r2, fc: s.c2 };
  }

  paintSel() {
    const s = this.norm();
    const multi = s && (s.r0 !== s.r1 || s.c0 !== s.c1);
    for (const rowEl of this.body.children) {
      if (!rowEl.dataset.r) continue;
      const r = +rowEl.dataset.r;
      const picked = this.picked.has(r);
      rowEl.classList.toggle("picked", picked);
      const box = rowEl.querySelector(".box");
      if (box && box.classList.contains("on") !== picked) { box.classList.toggle("on", picked); box.innerHTML = picked ? icon("i-check", "xs") : ""; }
      for (const cell of rowEl.children) {
        const c = cell.dataset.c;
        if (c === undefined) continue;
        const ci = +c;
        cell.classList.toggle("act", !!s && r === s.fr && ci === s.fc);
        cell.classList.toggle("insel", !!multi && r >= s.r0 && r <= s.r1 && ci >= s.c0 && ci <= s.c1);
      }
    }
    const all = $(".g-head .box", this.el);
    if (all) {
      all.className = "box" + (this.picked.size ? " some" : "");
    }
    this.updateFoot();
    this.o.onPick && this.o.onPick(this.picked);
  }

  // Large selections recompute the footer once the selection settles,
  // not on every mouse move of a drag.
  updateFoot() {
    clearTimeout(this.footT);
    const s = this.norm();
    if (s && (s.r1 - s.r0 + 1) * (s.c1 - s.c0 + 1) > 5000) this.footT = setTimeout(() => this.paintFoot(), 120);
    else this.paintFoot();
  }

  paintFoot() {
    let h = this.o.status ? this.o.status() : "";
    const s = this.norm();
    if (s && (s.r1 > s.r0 || s.c1 > s.c0)) {
      const cells = (s.r1 - s.r0 + 1) * (s.c1 - s.c0 + 1);
      // Whole numbers add up exactly with BigInt: bigint ids and amounts
      // beyond 2^53 would otherwise be rounded.
      let sum = 0, cnt = 0, min = Infinity, max = -Infinity, filled = 0;
      let isum = 0n, imin = null, imax = null, ints = true;
      const src = this.o.source;
      for (let r = s.r0; r <= Math.min(s.r1, s.r0 + 50000); r++) {
        const row = src.get(r);
        if (!row) continue;
        for (let c = s.c0; c <= s.c1; c++) {
          const v = row[this.cols[c].idx];
          if (v != null) filled++;
          if (this.cols[c].kind === "num" && v != null) {
            const x = parseFloat(v);
            if (isNaN(x)) continue;
            sum += x; cnt++; min = Math.min(min, x); max = Math.max(max, x);
            if (ints && /^-?\d+$/.test(v)) {
              const b = BigInt(v);
              isum += b;
              if (imin === null || b < imin) imin = b;
              if (imax === null || b > imax) imax = b;
            } else ints = false;
          }
        }
      }
      const f = (x) => x.toLocaleString(undefined, { maximumFractionDigits: 4 });
      h = `<span><b>${fmtN(cells)}</b> cells selected</span><span><b>${fmtN(filled)}</b> not null</span>`;
      if (cnt && ints) h += `<span>Sum <b>${isum.toLocaleString()}</b></span><span>Average <b>${f(Number(isum) / cnt)}</b></span><span>Min <b>${imin.toLocaleString()}</b></span><span>Max <b>${imax.toLocaleString()}</b></span>`;
      else if (cnt) h += `<span>Sum <b>${f(sum)}</b></span><span>Average <b>${f(sum / cnt)}</b></span><span>Min <b>${f(min)}</b></span><span>Max <b>${f(max)}</b></span>`;
    }
    this.foot.innerHTML = h;
  }

  cellAt(e) {
    const cell = e.target.closest(".g-c");
    if (!cell || cell.dataset.c === undefined) return null;
    const row = cell.parentElement;
    return { r: +row.dataset.r, c: +cell.dataset.c, el: cell };
  }
  cellEl(r, c) { return this.body.querySelector(`.g-row[data-r="${r}"] [data-c="${c}"]`); }

  select(r, c, extend) {
    const n = this.o.source.count();
    if (!n || !this.cols.length) { this.sel = null; this.paintSel(); return; }
    r = Math.max(0, Math.min(n - 1, r));
    c = Math.max(0, Math.min(this.cols.length - 1, c));
    if (extend && this.sel) { this.sel.r2 = r; this.sel.c2 = c; }
    else this.sel = { r, c, r2: r, c2: c };
    this.scrollTo(r, c);
    this.paintSel();
  }

  scrollTo(r, c) {
    const sc = this.sc, rh = this.rh;
    const top = r * rh;
    if (top < sc.scrollTop) sc.scrollTop = top;
    else if (this.headH + top + rh > sc.scrollTop + sc.clientHeight) sc.scrollTop = this.headH + top + rh - sc.clientHeight;
    if (c > 0) {
      const left = this.lefts[c], right = left + this.ws[c];
      const frozenEdge = sc.scrollLeft + this.rnw + this.ws[0];
      if (left < frozenEdge) sc.scrollLeft = left - this.rnw - this.ws[0];
      else if (right > sc.scrollLeft + sc.clientWidth) sc.scrollLeft = right - sc.clientWidth + 2;
    }
    this.draw();
  }

  flash(r, name) {
    this.flashes.set(r + ":" + name, performance.now() + 900);
    const c = this.cols.findIndex((x) => x.name === name);
    const el = c >= 0 && this.cellEl(r, c);
    if (el) { el.classList.remove("flash"); void el.offsetWidth; el.classList.add("flash"); }
  }
  markErr(r, name) {
    const c = this.cols.findIndex((x) => x.name === name);
    const el = c >= 0 && this.cellEl(r, c);
    if (el) { el.classList.add("err"); setTimeout(() => el.classList.remove("err"), 1600); }
  }

  bind() {
    const sc = this.sc;
    let raf = 0;
    sc.addEventListener("scroll", () => {
      sc.classList.toggle("scrolled-x", sc.scrollLeft > 0);
      if (this.editor) this.placeEditor();
      if (!raf) raf = requestAnimationFrame(() => { raf = 0; this.draw(); });
    }, { passive: true });
    let roRaf = 0, roW = -1;
    this.ro = new ResizeObserver(() => {
      if (roRaf) return;
      roRaf = requestAnimationFrame(() => {
        roRaf = 0;
        const w = sc.clientWidth;
        if (this.fit && w !== roW) { roW = w; this.layout(); if (this.editor) this.placeEditor(); }
        this.draw();
      });
    });
    this.ro.observe(sc);

    // header: sort/menu + column resize
    this.head.addEventListener("mousedown", (e) => {
      const rs = e.target.closest("[data-rs]");
      if (rs) {
        e.preventDefault();
        const c = +rs.parentElement.dataset.c, col = this.cols[c];
        const x0 = e.clientX, w0 = col.width, e0 = this.ws[c];
        rs.classList.add("drag");
        const mv = (ev) => {
          const dx = ev.clientX - x0;
          if (this.fitted && this.cols.length > 1) {
            // Saved widths are proportions here: pick the one that puts this
            // column's drawn edge under the pointer once the others rebalance.
            const min = FIT_MIN[S.density], avail = this.sc.clientWidth - this.rnw;
            const target = Math.max(min, Math.min(avail - (this.cols.length - 1) * min, e0 + dx));
            const others = this.cols.reduce((s, x, i) => (i === c ? s : s + x.width), 0);
            col.width = Math.round(Math.max(20, Math.min(4000, (target * others) / Math.max(1, avail - target))));
          } else col.width = Math.max(60, Math.min(900, w0 + dx));
          this.layout(); this.draw();
        };
        const up = () => { rs.classList.remove("drag"); removeEventListener("mousemove", mv); removeEventListener("mouseup", up); this.o.onResize && this.o.onResize(col); };
        addEventListener("mousemove", mv); addEventListener("mouseup", up);
      }
    });
    this.head.addEventListener("click", (e) => {
      if (e.target.closest("[data-rs]")) return;
      if (e.target.closest("[data-pickall]")) {
        if (this.picked.size) this.picked.clear();
        else for (let r = 0; r < this.o.source.count(); r++) if (this.o.source.get(r)) this.picked.add(r);
        this.lastPick = null;
        this.paintSel();
        return;
      }
      const h = e.target.closest(".g-h[data-c]");
      if (h && this.o.onHeader) this.o.onHeader(this.cols[+h.dataset.c], h);
    });
    this.head.addEventListener("dblclick", (e) => {
      const rs = e.target.closest("[data-rs]");
      if (rs) { const col = this.cols[+rs.parentElement.dataset.c]; this.autosize(col); }
    });

    // body: selection, drag-select, row picking, double-click to edit
    let dragging = false, lastDown = { t: 0, r: -1, c: -1 };
    this.body.addEventListener("mousedown", (e) => {
      if (e.button !== 0) return;
      const rowEl = e.target.closest(".g-row");
      if (e.target.closest("[data-add]")) { e.preventDefault(); this.o.onAdd(); return; }
      if (!rowEl) return;
      const r = +rowEl.dataset.r;
      if (e.target.closest("[data-pick]")) {
        e.preventDefault();
        if (e.shiftKey && this.lastPick != null) {
          const on = !this.picked.has(r);
          for (let i = Math.min(r, this.lastPick); i <= Math.max(r, this.lastPick); i++) on ? this.picked.add(i) : this.picked.delete(i);
        } else this.picked.has(r) ? this.picked.delete(r) : this.picked.add(r);
        this.lastPick = r;
        this.paintSel();
        return;
      }
      if (e.target.closest("[data-exp]")) { e.preventDefault(); this.o.onExpand(r); return; }
      const hit = this.cellAt(e);
      if (!hit) return;
      e.preventDefault();
      this.el.focus({ preventScroll: true });
      if (this.editor) this.commitEdit();
      const now = Date.now();
      const isDouble = now - lastDown.t < 420 && lastDown.r === hit.r && lastDown.c === hit.c;
      const wasActive = this.sel && this.sel.r2 === hit.r && this.sel.c2 === hit.c && this.sel.r === hit.r && this.sel.c === hit.c;
      lastDown = { t: now, r: hit.r, c: hit.c };
      this.select(hit.r, hit.c, e.shiftKey);
      const col = this.cols[hit.c];
      if (e.target.closest("[data-link]") && this.o.onLink) { this.o.onLink(hit.r, col); return; }
      if (col.kind === "bool" && e.target.closest(".chk") && !col.readonly && this.o.onEdit) {
        const row = this.o.source.get(hit.r);
        if (row) this.o.onEdit(hit.r, col, isTrue(row[col.idx]) ? "false" : "true", row);
        return;
      }
      if (isDouble || (wasActive && col.kind === "enum")) { this.startEdit(); return; }
      dragging = true;
    });
    this.body.addEventListener("mousemove", (e) => {
      if (!dragging) return;
      const hit = this.cellAt(e);
      if (hit && this.sel && (hit.r !== this.sel.r2 || hit.c !== this.sel.c2)) { this.sel.r2 = hit.r; this.sel.c2 = hit.c; this.paintSel(); }
    });
    this.body.addEventListener("contextmenu", (e) => {
      if (!this.o.onContext) return;
      const rowEl = e.target.closest(".g-row");
      // The row-number column has no cell index: treat it as the first field.
      const hit = this.cellAt(e) || (rowEl && { r: +rowEl.dataset.r, c: this.sel ? this.sel.c2 : 0 });
      if (!hit || !this.o.source.get(hit.r)) return;
      e.preventDefault();
      if (this.editor) this.commitEdit();
      // Keep the selection if the click is inside it; otherwise act on the clicked cell.
      if (this.picked.size && !this.picked.has(hit.r)) { this.picked.clear(); this.paintSel(); }
      const s = this.norm();
      const inside = s && hit.r >= s.r0 && hit.r <= s.r1 && hit.c >= s.c0 && hit.c <= s.c1;
      if (!inside && !this.picked.size) this.select(hit.r, hit.c);
      this.el.focus({ preventScroll: true });
      this.o.onContext(e, hit);
    });
    this.onUp = () => { dragging = false; };
    addEventListener("mouseup", this.onUp);

    this.el.addEventListener("keydown", (e) => this.onKey(e));
    this.onPasteEv = (e) => {
      if (document.activeElement !== this.el || !this.sel || !this.o.onPaste) return;
      const text = e.clipboardData.getData("text/plain");
      if (!text) return;
      e.preventDefault();
      this.o.onPaste(this.norm(), pastedMatrix(text));
    };
    document.addEventListener("paste", this.onPasteEv);
  }

  autosize(col) {
    let w = headerWidth(col, true);
    const [a, b] = this.drawn || [0, 0];
    const pad = 2 * (S.density === "compact" ? 6 : 10) + 4;
    for (let r = a; r <= b; r++) {
      const row = this.o.source.get(r);
      if (row) w = Math.max(w, valueWidth(col.kind, row[col.idx]) + pad);
    }
    col.width = Math.round(Math.min(560, Math.max(COL_MIN, w)));
    this.layout();
    this.o.onResize && this.o.onResize(col);
  }

  onKey(e) {
    if (this.editor) return;
    const mod = e.metaKey || e.ctrlKey;
    const n = this.o.source.count();
    const k = e.key;
    if (mod && e.shiftKey && k.toLowerCase() === "d") {
      if (this.o.onDuplicate && (this.sel || this.picked.size)) { e.preventDefault(); this.o.onDuplicate(); }
      return;
    }
    if (!this.sel && /^(Arrow|Tab|Enter|Home|End|Page)/.test(k) && n) { e.preventDefault(); this.select(0, 0); return; }
    if (!this.sel) {
      if (k === "Escape" && this.picked.size) { this.picked.clear(); this.paintSel(); }
      return;
    }
    const s = this.sel;
    const page = Math.max(1, Math.floor((this.sc.clientHeight - this.headH) / this.rh) - 1);
    const last = this.cols.length - 1;
    const mv = (r, c) => { e.preventDefault(); this.select(r, c, e.shiftKey); };
    switch (k) {
      case "ArrowDown": return mv(mod ? n - 1 : s.r2 + 1, s.c2);
      case "ArrowUp": return mv(mod ? 0 : s.r2 - 1, s.c2);
      case "ArrowRight": return mv(s.r2, mod ? last : s.c2 + 1);
      case "ArrowLeft": return mv(s.r2, mod ? 0 : s.c2 - 1);
      case "PageDown": return mv(s.r2 + page, s.c2);
      case "PageUp": return mv(s.r2 - page, s.c2);
      case "Home": return mv(mod ? 0 : s.r2, 0);
      case "End": return mv(mod ? n - 1 : s.r2, last);
      case "Tab": e.preventDefault(); return this.select(s.r2, s.c2 + (e.shiftKey ? -1 : 1));
      case "Enter": case "F2": e.preventDefault(); return this.startEdit();
      case " ":
        if (this.o.onExpand) { e.preventDefault(); this.o.onExpand(s.r2); }
        return;
      case "Escape":
        if (this.picked.size) { this.picked.clear(); this.paintSel(); }
        else { this.sel = { r: s.r2, c: s.c2, r2: s.r2, c2: s.c2 }; this.paintSel(); }
        return;
      case "Delete": case "Backspace":
        if (this.o.onClear) { e.preventDefault(); this.o.onClear(this.norm()); }
        return;
    }
    if (mod && k.toLowerCase() === "c") { e.preventDefault(); this.copy(); return; }
    if (mod && k.toLowerCase() === "a") { e.preventDefault(); this.sel = { r: 0, c: 0, r2: n - 1, c2: last }; this.paintSel(); return; }
    if (mod && k.toLowerCase() === "z" && this.o.onUndo) { e.preventDefault(); this.o.onUndo(); return; }
    if (k.length === 1 && !mod && !e.altKey) {
      const col = this.cols[s.c2];
      if (col && !["bool", "enum"].includes(col.kind)) { e.preventDefault(); this.startEdit(k); }
    }
  }

  async copy() {
    const s = this.norm();
    if (!s) return;
    const cols = this.cols.slice(s.c0, s.c1 + 1);
    let rows = [], missing = 0;
    if (this.o.copyRows) {
      // The view loads rows not on screen yet and the full text of cut cells.
      rows = await this.o.copyRows(s.r0, s.r1, cols);
      if (!rows) return;
    } else {
      for (let r = s.r0; r <= s.r1; r++) {
        const row = this.o.source.get(r);
        if (row) rows.push(row); else missing++;
      }
    }
    const out = rows.map((row) => cols.map((c) => row[c.idx]));
    copyMatrix(out);
    const cells = out.length * (s.c1 - s.c0 + 1);
    toast(cells === 1 ? "Copied" : `Copied ${plural(cells, "cell")}` + (missing ? ` (${fmtN(missing)} rows were not loaded yet)` : ""));
  }

  // ---------- in-cell editing

  startEdit(initial) {
    const s = this.sel;
    if (!s) return;
    const r = s.r2, c = s.c2, col = this.cols[c];
    const row = this.o.source.get(r);
    if (!row || !col) return;
    this.sel = { r, c, r2: r, c2: c };
    this.paintSel();
    const v = row[col.idx];
    const readonly = col.readonly || !this.o.onEdit;
    if (!readonly && col.kind === "bool") { this.o.onEdit(r, col, isTrue(v) ? "false" : "true", row); return; }
    if (!readonly && col.kind === "enum" && col.enum) { this.enumPicker(r, c, col, v); return; }
    if (readonly && col.kind === "blob") return;
    if (initial == null && isCut(row, col.idx) && this.o.full) {
      // The grid has only the start of this value: load all of it first,
      // so editing never saves a shortened value.
      this.o.full(row, col).then((full) => {
        const s2 = this.sel;
        if (!this.editor && s2 && s2.r2 === r && s2.c2 === c && this.o.source.get(r) === row) this.openEditor(r, c, col, full, null, readonly);
      }, toastErr);
      return;
    }
    this.openEditor(r, c, col, row, initial, readonly);
  }

  openEditor(r, c, col, row, initial, readonly) {
    const v = row[col.idx];
    let text = v ?? "";
    if (readonly && col.kind === "json" && v) text = prettyJSON(v) ?? v;
    const multi = col.kind === "json" || (col.kind === "text" && (/\n/.test(text) || text.length > 40)) || (readonly && text.length > 40);
    const ed = html(`<div class="g-editor ${["json", "uuid", "num", "array", "range", "other", "net"].includes(col.kind) ? "mono" : ""}"></div>`);
    const input = document.createElement(multi ? "textarea" : "input");
    input.spellcheck = false;
    if (!multi) input.type = "text";
    input.value = initial != null ? initial : text;
    if (v === null && initial == null) input.placeholder = "null";
    input.readOnly = readonly;
    ed.appendChild(input);
    if (multi || col.kind === "link") {
      ed.appendChild(html(`<div class="hint">${readonly ? "<span>Read-only</span><span>Esc closes</span>" : `<span>Enter saves</span>${multi ? "<span>Shift+Enter adds a line</span>" : ""}<span>Esc cancels</span>`}</div>`));
    }
    this.sc.appendChild(ed);
    this.editor = { r, c, col, row, el: ed, input, orig: v, multi, readonly, initial };
    this.placeEditor();
    input.focus();
    if (initial == null) input.select(); else input.setSelectionRange(input.value.length, input.value.length);
    if (col.kind === "link" && this.o.suggest && !readonly) this.attachSuggest();

    input.addEventListener("keydown", (e) => {
      if (this.editor?.sug?.handle(e)) return;
      if (e.key === "Escape") { e.preventDefault(); this.cancelEdit(); }
      else if (e.key === "Enter" && !e.shiftKey && !e.altKey) { e.preventDefault(); this.commitEdit(1, 0); }
      else if (e.key === "Tab") { e.preventDefault(); this.commitEdit(0, e.shiftKey ? -1 : 1); }
      e.stopPropagation();
    });
    input.addEventListener("blur", () => setTimeout(() => { if (this.editor && this.editor.input === input && !ed.contains(document.activeElement)) this.commitEdit(); }, 120));
  }

  placeEditor() {
    const ed = this.editor;
    if (!ed) return;
    const { r, c, col, el, multi } = ed;
    const left = c === 0 ? Math.max(this.lefts[0], this.sc.scrollLeft + this.rnw) : this.lefts[c];
    el.style.top = this.headH + r * this.rh + "px";
    el.style.left = left + "px";
    el.style.width = (multi ? Math.max(this.ws[c], 360) : Math.max(this.ws[c], 160)) + "px";
    if (!multi) el.style.height = this.rh + "px";
  }

  enumPicker(r, c, col, v) {
    const cell = this.cellEl(r, c);
    if (!cell) return;
    const row = this.o.source.get(r);
    const opts = [...col.enum.map((x) => ({ v: x, h: (col.enum.indexOf(x) * 67 + 205) % 360 })), ...(col.nullable ? [{ v: null }] : [])];
    const el = html(`<div class="scroll">${opts.map((o, i) => `<button class="mi ${o.v === v ? "on" : ""}" data-i="${i}">${o.v === null ? `<span class="v-null">null</span>` : `<span class="pill" style="--h:${o.h}">${esc(o.v)}</span>`}${o.v === v ? `<span class="r">${icon("i-check", "sm")}</span>` : ""}</button>`).join("")}</div>`);
    const wrap = html(`<div></div>`); wrap.appendChild(el);
    const p = openPop(cell, wrap, { onClose: () => this.el.focus({ preventScroll: true }) });
    const keys = menuKeys(wrap, wrap);
    wrap.tabIndex = -1; wrap.focus();
    keys.reset(Math.max(0, opts.findIndex((o) => o.v === v)));
    wrap.addEventListener("click", (e) => {
      const b = e.target.closest("[data-i]");
      if (!b) return;
      const val = opts[+b.dataset.i].v;
      closePop();
      if (val !== v && row) this.o.onEdit(r, col, val, row);
    });
    return p;
  }

  attachSuggest() {
    const ed = this.editor;
    const list = html(`<div class="scroll" style="border-top:1px solid var(--line-2);max-height:240px;padding:4px"></div>`);
    ed.el.insertBefore(list, ed.el.lastChild);
    let items = [], hi = -1, seq = 0;
    const paint = () => {
      list.innerHTML = items.length ? items.map((it, i) => `<button class="mi ${i === hi ? "hi" : ""}" data-i="${i}" tabindex="-1"><span class="link">${icon("i-link")}${esc(it.value)}</span><span class="muted" style="overflow:hidden;text-overflow:ellipsis">${esc(it.label || "")}</span></button>`).join("") : `<div class="muted" style="padding:6px 8px;font-size:12.5px">No matching records</div>`;
    };
    const load = debounce(async () => {
      const my = ++seq;
      try {
        const res = await this.o.suggest(ed.col, ed.input.value === String(ed.orig ?? "") ? "" : ed.input.value);
        if (my !== seq || this.editor !== ed) return;
        items = res; hi = -1; paint();
      } catch {}
    }, 160);
    ed.input.addEventListener("input", load);
    load();
    list.addEventListener("mousedown", (e) => {
      const b = e.target.closest("[data-i]");
      if (!b) return;
      e.preventDefault();
      ed.input.value = items[+b.dataset.i].value;
      this.commitEdit(1, 0);
    });
    ed.sug = {
      handle: (e) => {
        if (e.key === "ArrowDown" && items.length) { e.preventDefault(); hi = Math.min(items.length - 1, hi + 1); paint(); return true; }
        if (e.key === "ArrowUp" && items.length) { e.preventDefault(); hi = Math.max(-1, hi - 1); paint(); return true; }
        if (e.key === "Enter" && hi >= 0) { ed.input.value = items[hi].value; }
        return false;
      },
    };
  }

  cancelEdit() {
    if (!this.editor) return;
    this.editor.el.remove();
    this.editor = null;
    this.el.focus({ preventScroll: true });
  }

  commitEdit(dr = 0, dc = 0) {
    const ed = this.editor;
    if (!ed) return;
    this.editor = null;
    ed.el.remove();
    this.el.focus({ preventScroll: true });
    if (!ed.readonly) {
      let val = ed.input.value;
      const blankIsNull = !["text"].includes(ed.col.kind);
      if (val === "" && (ed.orig === null || blankIsNull)) val = ed.col.nullable === false && !blankIsNull ? "" : null;
      if (val !== ed.orig) this.o.onEdit(ed.r, ed.col, val, ed.row);
    }
    if (dr || dc) this.select(ed.r + dr, ed.c + dc);
  }

  destroy() {
    this.cancelEdit();
    this.ro.disconnect();
    removeEventListener("mouseup", this.onUp);
    document.removeEventListener("paste", this.onPasteEv);
  }
}

// ============================================================ app state

const S = {
  meta: null,
  tables: [],
  schemas: [],
  collapsed: new Set(store.get("aa.collapsed", [])),
  side: store.get("aa.side", true) !== false,
  density: store.get("aa.density", "comfortable") === "compact" ? "compact" : "comfortable",
  view: null,
  filterText: "",
  pendingFilter: null,
};
const app = $("#app");
document.documentElement.dataset.density = S.density;

// ---------- layout preferences: sidebar, density, fit to screen

// Below this width the sidebar is an overlay drawer (see app.css).
const NARROW = matchMedia("(max-width: 820px)");

function sideShown() {
  const shell = $(".shell");
  if (!shell) return false;
  return NARROW.matches ? shell.classList.contains("side-open") : S.side;
}

// One toggle for both layouts: on wide screens it collapses the sidebar
// (remembered), on narrow ones it opens the drawer.
function toggleSide(show) {
  const shell = $(".shell");
  if (!shell) return;
  const next = show ?? !sideShown();
  if (NARROW.matches) shell.classList.toggle("side-open", next);
  else { S.side = next; store.set("aa.side", next); }
  paintSide();
}

function paintSide() {
  const shell = $(".shell");
  if (!shell) return;
  shell.classList.toggle("side-collapsed", !S.side);
  const shown = sideShown();
  const side = $(".side", shell);
  // A hidden sidebar must not take focus: move it out before going inert.
  if (!shown && side.contains(document.activeElement)) $("[data-menu]")?.focus();
  side.inert = !shown;
  for (const b of $$("[data-menu]")) {
    b.setAttribute("aria-expanded", String(shown));
    b.setAttribute("aria-label", shown ? "Hide sidebar" : "Show sidebar");
    b.title = `${shown ? "Hide" : "Show"} sidebar (${MOD === "⌘" ? "⌘B" : "Ctrl+B"})`;
  }
}
NARROW.addEventListener("change", () => { $(".shell")?.classList.remove("side-open"); paintSide(); });

function setDensity(d) {
  S.density = d === "compact" ? "compact" : "comfortable";
  store.set("aa.density", S.density);
  document.documentElement.dataset.density = S.density;
  const g = S.view && S.view.grid;
  if (g) g.refresh();
}

function toggleFit() {
  if (S.view && S.view.toggleFit) S.view.toggleFit();
}

const FIT_KEY = MOD === "⌘" ? "⌘⇧F" : "Ctrl+Shift+F";

// Row height, density and fit-to-screen for a view with a grid
// (TableView or SqlView: rowHeight/setRowHeight/fit/toggleFit).
function viewMenu(anchor, view) {
  const chk = (on) => (on ? `<span class="r">${icon("i-check", "sm")}</span>` : "");
  const el = html(`<div style="min-width:230px" role="menu" tabindex="-1">
    <h4>Row height</h4>
    ${["Short", "Medium", "Tall"].map((n, i) => `<button class="mi ${view.rowHeight === i ? "on" : ""}" data-rh="${i}" role="menuitemradio" aria-checked="${view.rowHeight === i}">${icon("i-rows")}${n}${chk(view.rowHeight === i)}</button>`).join("")}
    <hr><h4>Density</h4>
    ${[["comfortable", "Comfortable"], ["compact", "Compact"]].map(([d, n]) => `<button class="mi ${S.density === d ? "on" : ""}" data-density="${d}" role="menuitemradio" aria-checked="${S.density === d}">${icon("i-grid")}${n}${chk(S.density === d)}</button>`).join("")}
    <hr>
    <button class="mi ${view.fit ? "on" : ""}" data-fit role="menuitemcheckbox" aria-checked="${view.fit}">${icon("i-expand")}Fit columns to screen<span class="r">${view.fit ? icon("i-check", "sm") : `<span class="kbd">${FIT_KEY}</span>`}</span></button>
  </div>`);
  openPop(anchor, el);
  menuKeys(el, el);
  el.focus();
  el.addEventListener("click", (e) => {
    const b = e.target.closest("button");
    if (!b) return;
    closePop();
    if (b.dataset.rh != null) view.setRowHeight(+b.dataset.rh);
    else if (b.dataset.density) setDensity(b.dataset.density);
    else if (b.hasAttribute("data-fit")) view.toggleFit();
  });
}

function signedOut() {
  if (!S.meta && !S.view) return; // several requests can see the same 401
  closeDrawer(true);
  if (S.view && S.view.destroy) S.view.destroy();
  S.view = null; S.meta = null;
  closePop();
  renderLogin();
}

// Signing out on purpose also forgets SQL text and history kept in this
// browser, which may hold passwords or personal data.
async function signOut() {
  try { await api("logout"); } catch {}
  try {
    for (let i = localStorage.length - 1; i >= 0; i--) {
      const k = localStorage.key(i);
      if (k && k.startsWith("aa.sql.")) localStorage.removeItem(k);
    }
  } catch {}
  signedOut();
}

async function boot() {
  try {
    S.meta = await api("meta");
  } catch (e) {
    if (e.status !== 401) toastErr(e);
    renderLogin();
    return;
  }
  await renderShell();
}

// ============================================================ login

function renderLogin(err) {
  document.title = "Sign in · airadminer";
  const last = store.get("aa.login", { host: "localhost", port: "5432", user: "postgres", database: "postgres", sslmode: "prefer" });
  let useURL = store.get("aa.login.url", false);
  app.innerHTML = `
  <main class="login">
    <form class="login-card" novalidate>
      <span class="logo"><svg><use href="#logo"/></svg>airadminer</span>
      <h1>Connect to PostgreSQL</h1>
      <p class="lede">Your password stays in this server's memory for the session and is never written to disk.</p>
      <div data-fields></div>
      <div class="actions">
        <button type="button" class="linkish" data-mode></button>
        <button class="btn primary" type="submit">Connect</button>
      </div>
      <div data-err></div>
    </form>
  </main>`;
  const form = $("form", app);
  const paint = () => {
    $("[data-mode]", form).textContent = useURL ? "Use separate fields" : "Use a connection URL";
    $("[data-fields]", form).innerHTML = useURL ? `
      <label class="field"><span>Connection URL</span><input class="input mono" name="url" placeholder="postgres://user:password@host:5432/database" autocomplete="off" spellcheck="false" required></label>` : `
      <div class="form-row"><label class="field"><span>Host</span><input class="input" name="host" value="${esc(last.host)}" autocomplete="off" spellcheck="false"></label>
      <label class="field"><span>Port</span><input class="input" name="port" value="${esc(last.port)}" inputmode="numeric"></label></div>
      <div class="form-row even"><label class="field"><span>User</span><input class="input" name="user" value="${esc(last.user)}" autocomplete="username" spellcheck="false"></label>
      <label class="field"><span>Password</span><input class="input" type="password" name="password" autocomplete="current-password"></label></div>
      <div class="form-row even"><label class="field"><span>Database</span><input class="input" name="database" value="${esc(last.database)}" spellcheck="false"></label>
      <label class="field"><span>SSL</span><select class="select" name="sslmode">
        ${["prefer", "disable", "require", "verify-ca", "verify-full"].map((m) => `<option ${m === last.sslmode ? "selected" : ""}>${m}</option>`).join("")}
      </select></label></div>`;
    const first = useURL ? $("[name=url]", form) : (last.host && last.user ? $("[name=password]", form) : $("[name=host]", form));
    first.focus();
  };
  paint();
  $("[data-mode]", form).onclick = () => { useURL = !useURL; store.set("aa.login.url", useURL); paint(); };
  const showErr = (e) => {
    $("[data-err]", form).innerHTML = e ? `<div class="error-box" role="alert"><b>${esc(e.message)}</b>${e.detail ? `<div class="detail">${esc(e.detail)}</div>` : ""}</div>` : "";
  };
  if (err) showErr(err);
  form.onsubmit = async (e) => {
    e.preventDefault();
    const data = Object.fromEntries(new FormData(form));
    const btn = $("button[type=submit]", form);
    btn.disabled = true; btn.innerHTML = `<span class="spin"></span>Connecting`;
    showErr(null);
    try {
      await api("login", data);
      if (!useURL) store.set("aa.login", { host: data.host, port: data.port, user: data.user, database: data.database, sslmode: data.sslmode });
      await boot();
    } catch (er) {
      showErr(er);
      btn.disabled = false; btn.textContent = "Connect";
    }
  };
}

// ============================================================ shell + sidebar

async function loadTables() {
  const r = await api("tables");
  S.tables = r.tables;
  S.schemas = r.schemas;
}

async function renderShell() {
  try { await loadTables(); } catch (e) { toastErr(e); }
  app.innerHTML = `
  <div class="shell${S.side ? "" : " side-collapsed"}">
    <aside class="side" id="side" aria-label="Tables">
      <div class="side-top">
        <button class="db-switch" data-db title="Switch database">${icon("i-db")}<span class="name"></span>${icon("i-down", "sm")}</button>
        <div class="side-search">${icon("i-search", "sm")}<input class="input" data-find placeholder="Find a table" spellcheck="false" aria-label="Find a table"><span class="kbd">${MOD === "⌘" ? "⌘K" : "Ctrl K"}</span></div>
      </div>
      <nav class="side-list" data-list></nav>
      <div class="side-foot">
        ${S.meta.sql !== false ? `<a class="tbl" href="#/sql" data-sqllink>${icon("i-terminal")}<span class="nm">SQL editor</span></a>` : ""}
        <div class="who" data-who></div>
        <div class="ver" data-ver></div>
        <div class="row">
          <button class="btn" data-themebtn></button>
          <button class="btn icon" data-logout title="Sign out" ${S.meta.fixed ? "hidden" : ""}>${icon("i-logout")}</button>
        </div>
      </div>
    </aside>
    <main class="main" data-main></main>
  </div>`;
  paintSideMeta();
  paintTables();
  const find = $("[data-find]");
  find.addEventListener("input", () => { S.filterText = find.value; paintTables(); });
  find.addEventListener("keydown", (e) => {
    if (e.key === "Enter") { const a = $(".side-list .tbl"); if (a) { location.hash = a.getAttribute("href"); find.blur(); } }
    if (e.key === "Escape") { find.value = ""; S.filterText = ""; paintTables(); find.blur(); }
  });
  $("[data-db]").onclick = (e) => dbMenu(e.currentTarget);
  $("[data-themebtn]").onclick = cycleTheme;
  $("[data-logout]").onclick = signOut;
  $("[data-list]").addEventListener("click", (e) => {
    const h = e.target.closest("[data-schema]");
    if (h) {
      const s = h.dataset.schema;
      S.collapsed.has(s) ? S.collapsed.delete(s) : S.collapsed.add(s);
      store.set("aa.collapsed", [...S.collapsed]);
      paintTables();
    }
    if (e.target.closest(".tbl") && NARROW.matches) toggleSide(false);
  });
  paintThemeBtn();
  paintSide();
  route();
}

function paintSideMeta() {
  $("[data-db] .name").textContent = S.meta.database;
  const host = S.meta.host ? `${S.meta.host}${S.meta.port && S.meta.port !== 5432 ? ":" + S.meta.port : ""}` : "local socket";
  $("[data-who]").innerHTML = `<span title="${esc(S.meta.user + "@" + host)}">${esc(S.meta.user)} on ${esc(host)}</span><span style="flex:none">PG ${esc(String(S.meta.server).split(" ")[0])}</span>`;
  const built = [S.meta.commit, S.meta.date && String(S.meta.date).slice(0, 10)].filter(Boolean).join(", ");
  $("[data-ver]").textContent = `airadminer ${S.meta.version}`;
  $("[data-ver]").title = built ? `airadminer ${S.meta.version} (${built})` : "";
}

function paintTables() {
  const list = $("[data-list]");
  if (!list) return;
  const q = S.filterText.trim().toLowerCase();
  const cur = S.view && S.view.ref;
  let h = "";
  const bySchema = new Map(S.schemas.map((s) => [s, []]));
  for (const t of S.tables) {
    if (q && !(t.name.toLowerCase().includes(q) || (t.schema + "." + t.name).toLowerCase().includes(q))) continue;
    if (!bySchema.has(t.schema)) bySchema.set(t.schema, []);
    bySchema.get(t.schema).push(t);
  }
  const showHeads = bySchema.size > 1 || !bySchema.has("public");
  for (const [schema, tables] of bySchema) {
    if (q && !tables.length) continue;
    const closed = !q && S.collapsed.has(schema);
    if (showHeads) h += `<button class="schema-h ${closed ? "closed" : ""}" data-schema="${esc(schema)}" aria-expanded="${!closed}">${icon("i-down", "xs")}${esc(schema)}<span class="n">${tables.length}</span></button>`;
    if (closed) continue;
    for (const t of tables) {
      const active = cur && cur.schema === t.schema && cur.name === t.name;
      const kind = { r: "Table", v: "View", m: "Materialized view", f: "Foreign table", p: "Partitioned table" }[t.kind];
      h += `<a class="tbl ${active ? "active" : ""}" href="#/t/${encodeURIComponent(t.schema)}/${encodeURIComponent(t.name)}" title="${esc(kind + (t.comment ? ": " + t.comment : ""))}">${icon("k-" + t.kind)}<span class="nm">${esc(t.name)}</span><span class="est">${t.kind === "v" ? "" : compact(t.estimate)}</span></a>`;
    }
  }
  if (!h) h = `<div class="side-empty">${q ? "No table matches “" + esc(S.filterText) + "”." : "No tables yet. Create one in the SQL editor."}</div>`;
  list.innerHTML = h;
  $("[data-sqllink]")?.classList.toggle("active", S.view instanceof SqlView);
}

async function dbMenu(anchor) {
  const el = html(`<div><h4>Databases</h4><div class="scroll">${S.meta.databases.map((d) => `<button class="mi ${d === S.meta.database ? "on" : ""}" data-d="${esc(d)}">${icon("i-db")}${esc(d)}${d === S.meta.database ? `<span class="r">${icon("i-check", "sm")}</span>` : ""}</button>`).join("")}</div></div>`);
  el.style.minWidth = anchor.offsetWidth + "px";
  openPop(anchor, el);
  el.addEventListener("click", async (e) => {
    const b = e.target.closest("[data-d]");
    if (!b) return;
    closePop();
    if (b.dataset.d !== S.meta.database) switchDB(b.dataset.d);
  });
}

async function switchDB(db) {
  try {
    await api("use", { database: db });
    S.meta = await api("meta");
    if (S.view && S.view.destroy) S.view.destroy();
    S.view = null;
    location.hash = "";
    await renderShell();
    toast(`Switched to ${db}`);
  } catch (e) { toastErr(e); }
}

// Theme preference: "system" follows the OS; "light" and "dark" are pinned
// via <html data-theme>, which theme.js restores before first paint.
const THEMES = {
  system: { label: "System theme", icon: "i-monitor" },
  light: { label: "Light theme", icon: "i-sun" },
  dark: { label: "Dark theme", icon: "i-moon" },
};
const THEME_ORDER = ["system", "light", "dark"];
function themePref() {
  const t = document.documentElement.dataset.theme;
  return t === "light" || t === "dark" ? t : "system";
}
function setTheme(pref) {
  const root = document.documentElement;
  if (pref === "system") delete root.dataset.theme; else root.dataset.theme = pref;
  try { pref === "system" ? localStorage.removeItem("aa.theme") : localStorage.setItem("aa.theme", pref); } catch {}
  paintThemeBtn();
}
function cycleTheme() {
  setTheme(THEME_ORDER[(THEME_ORDER.indexOf(themePref()) + 1) % THEME_ORDER.length]);
}
function paintThemeBtn() {
  const b = $("[data-themebtn]");
  if (!b) return;
  const cur = themePref(), next = THEME_ORDER[(THEME_ORDER.indexOf(cur) + 1) % THEME_ORDER.length];
  b.innerHTML = `${icon(THEMES[cur].icon)}${THEMES[cur].label}`;
  b.title = `Switch to ${THEMES[next].label.toLowerCase()}`;
}

// ============================================================ routing

// ---------- view state in the URL
//
// A table's search, filters, sorts and hidden fields follow the path after a
// "?", e.g. #/t/public/orders?q=acme&sort=["-total"], so a link opens the same
// view in another browser. A query, when present, describes the whole view;
// without one the view saved in this browser is used. Column widths, row
// height and fit to screen stay per browser.

function viewParams(v) {
  const p = new URLSearchParams();
  if (v.search) p.set("q", v.search);
  if (v.filters.length) p.set("f", JSON.stringify(v.filters.map((f) => (f.op === "sql" ? { op: "sql", val: f.val ?? "" } : { col: f.col, op: f.op, val: f.val ?? "" }))));
  if (v.sort.length) p.set("sort", JSON.stringify(v.sort.map((s) => (s.desc ? "-" : "+") + s.col)));
  if (v.hidden.length) p.set("hide", JSON.stringify(v.hidden));
  return p.toString();
}

function parseViewParams(qs) {
  const p = new URLSearchParams(qs);
  const list = (k) => { try { const a = JSON.parse(p.get(k) || "[]"); return Array.isArray(a) ? a : []; } catch { return []; } };
  const str = (v) => (v == null ? "" : String(v));
  return {
    search: p.get("q") || "",
    filters: list("f")
      .filter((f) => f && typeof f === "object" && (f.op === "sql" || (Object.hasOwn(OP_LABEL, f.op) && typeof f.col === "string")))
      .map((f) => (f.op === "sql" ? { op: "sql", val: str(f.val) } : { col: f.col, op: f.op, val: str(f.val) })),
    sort: list("sort").filter((s) => typeof s === "string" && /^[+-]./s.test(s)).map((s) => ({ col: s.slice(1), desc: s[0] === "-" })),
    hidden: list("hide").filter((h) => typeof h === "string"),
  };
}

function route() {
  if (!S.meta || !$("[data-main]")) return;
  const raw = location.hash.replace(/^#\/?/, "");
  const qi = raw.indexOf("?"); // a "?" in a name is %-escaped, so the first one starts the query
  let parts;
  try { parts = (qi < 0 ? raw : raw.slice(0, qi)).split("/").map(decodeURIComponent); } catch { parts = []; } // a malformed %-escape
  let next;
  if (parts[0] === "sql" && S.meta.sql !== false) next = { type: "sql" }; // -no-sql: falls through to a table
  else if (parts[0] === "t" && parts[1] && parts[2]) next = { type: "table", schema: parts[1], name: parts[2], tab: parts[3] === "structure" ? "structure" : "grid", view: qi < 0 ? null : parseViewParams(raw.slice(qi + 1)) };
  else next = { type: "empty" }; // no table is opened until one is picked
  const v = S.view;
  if (v && next.type === "table" && v instanceof TableView && v.ref.schema === next.schema && v.ref.name === next.name) {
    v.setTab(next.tab);
    if (next.view) v.fromURL(next.view);
    else v.syncURL();
    return;
  }
  if (v && v.destroy) v.destroy();
  closePop();
  closeDrawer(true);
  const main = $("[data-main]");
  if (next.type === "sql") S.view = new SqlView(main);
  else if (next.type === "table") S.view = new TableView(main, next);
  else { S.view = null; renderEmpty(main); }
  paintTables();
}
addEventListener("hashchange", route);

function topbar(inner) {
  return `<header class="topbar"><button class="btn icon menu-btn" data-menu aria-controls="side">${icon("i-sidebar")}</button>${inner}</header>`;
}
function bindTopbar(root) {
  const m = $("[data-menu]", root);
  if (m) m.onclick = () => toggleSide();
  paintSide();
}

function renderEmpty(main) {
  document.title = `${S.meta.database} · airadminer`;
  main.innerHTML = topbar(`<div class="crumb"><h2>${esc(S.meta.database)}</h2></div>`) + `
    <div class="empty">${icon("k-r")}${S.tables.length ? `<h3>No table open</h3><p>Pick a table in the sidebar.</p>` : `<h3>This database has no tables yet</h3><p>Tables you create show up in the sidebar.</p>`}${S.meta.sql !== false ? `<a class="btn primary" href="#/sql">${icon("i-terminal")}Open SQL editor</a>` : ""}</div>`;
  bindTopbar(main);
}

// ============================================================ table view

const PAGE = 200;
const MAX_PAGES = 60; // pages kept in the browser; the farthest from view are dropped

class RowStore {
  constructor(view) { this.view = view; this.gen = 0; this.reset(); }
  reset() {
    this.gen++;
    // Requests for the previous query are canceled, which also frees their
    // database connections on the server.
    if (this.ctl) this.ctl.abort();
    this.ctl = new AbortController();
    this.pages = new Map();
    this.inflight = new Map(); // page -> promise
    this.holds = []; // row ranges an action is loading; never dropped meanwhile
    this.total = 0;
    this.exact = false;
    this.ready = false;
    this.want = null;
  }
  evict() {
    if (this.pages.size <= MAX_PAGES) return;
    const [a, b] = this.want || [0, 0];
    const mid = Math.floor((a + b) / 2 / PAGE);
    const held = (p) => this.holds.some(([x, y]) => p >= Math.floor(x / PAGE) && p <= Math.floor(y / PAGE));
    const far = [...this.pages.keys()].filter((p) => Math.abs(p - mid) > 2 && !held(p)).sort((x, y) => Math.abs(y - mid) - Math.abs(x - mid));
    for (const p of far) {
      if (this.pages.size <= MAX_PAGES) break;
      this.pages.delete(p);
    }
  }
  count() { return this.total; }
  get(i) { const p = this.pages.get(Math.floor(i / PAGE)); return p ? p[i % PAGE] : undefined; }
  set(i, row) { const p = this.pages.get(Math.floor(i / PAGE)); if (p) p[i % PAGE] = row; }
  ensure(a, b) {
    this.want = [a, b];
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.fill(), 40);
  }
  fill() {
    if (!this.want) return;
    const [a, b] = this.want;
    for (let p = Math.floor(a / PAGE); p <= Math.floor(Math.min(b, Math.max(0, this.total - 1)) / PAGE); p++) {
      if (!this.pages.has(p) && !this.inflight.has(p)) this.inflight.set(p, this.fetch(p));
    }
  }
  // Loads every page that covers rows a..b. Resolves false if the view was
  // reset meanwhile or a page failed to load.
  async loadRange(a, b) {
    const gen = this.gen;
    const hold = [a, b];
    this.holds.push(hold);
    try {
      for (let p = Math.floor(a / PAGE); p <= Math.floor(b / PAGE); p++) {
        if (this.exact && p * PAGE >= this.total) break;
        if (!this.pages.has(p)) {
          if (!this.inflight.has(p)) this.inflight.set(p, this.fetch(p));
          await this.inflight.get(p);
        }
        if (gen !== this.gen || !this.pages.has(p)) return false;
      }
      return true;
    } finally {
      this.holds = this.holds.filter((h) => h !== hold);
    }
  }
  // In the default order, a page next to a loaded one is fetched from that
  // page's edge key: deep in a big table this skips OFFSET's scan of every
  // earlier row. A jump far away still uses the offset.
  anchor(p) {
    const v = this.view;
    if (v.settings.sort.length || !v.meta.keyMode) return {};
    const prev = this.pages.get(p - 1), next = this.pages.get(p + 1);
    if (prev && prev.length === PAGE) return { after: v.resolveKey(v.keyOf(prev[PAGE - 1])) };
    if (next && next.length) return { before: v.resolveKey(v.keyOf(next[0])) };
    return {};
  }
  async fetch(p) {
    const gen = this.gen;
    this.view.loading(true);
    try {
      const res = await api("rows", { ...this.view.query(), ...this.anchor(p), offset: p * PAGE, limit: PAGE }, { signal: this.ctl.signal });
      if (gen !== this.gen) return;
      this.pages.set(p, res.rows);
      this.evict();
      this.view.lastMs = res.ms;
      if (res.rows.length < PAGE) {
        const t = p * PAGE + res.rows.length;
        if (t !== this.total || !this.exact) { this.total = t; this.exact = true; this.view.countChanged(); }
      } else if (this.total < (p + 1) * PAGE) {
        this.total = (p + 1) * PAGE + 1;
        this.view.countChanged();
      }
      this.ready = true;
      if (!this.view.autoW.size && res.rows.length) this.view.sizeColumns(res.rows);
      this.view.grid.draw(true);
    } catch (e) {
      if (gen === this.gen) toastErr(e);
    } finally {
      if (gen === this.gen) {
        this.inflight.delete(p);
        if (!this.inflight.size) this.view.loading(false);
      }
    }
  }
  async start() {
    const gen = this.gen;
    const q = this.view.query();
    const first = this.fetch(0);
    this.inflight.set(0, first);
    api("count", q, { signal: this.ctl.signal }).then((r) => {
      if (gen !== this.gen) return;
      if (this.exact && r.exact) { this.total = r.count; }
      else if (!this.exact || r.exact) { this.total = r.count; this.exact = r.exact; }
      this.view.countChanged();
    }).catch(() => {});
    await first;
  }
}

const LABEL_NAMES = ["name", "title", "full_name", "display_name", "label", "username", "email", "slug", "code", "sku"];
function labelColumn(meta) {
  for (const n of LABEL_NAMES) { const c = meta.columns.find((x) => x.name === n); if (c) return c; }
  return meta.columns.find((c) => c.cat === "S" && !c.pk) || null;
}

async function getMeta(schema, name) {
  return api("table", { schema, name });
}

function opsFor(kind) {
  switch (kind) {
    case "num": case "date": case "datetime": case "time": return ["eq", "neq", "lt", "gt", "lte", "gte", "null", "notnull"];
    case "bool": return ["true", "false", "null", "notnull"];
    case "enum": return ["eq", "neq", "null", "notnull"];
    case "link": case "uuid": return ["eq", "neq", "contains", "null", "notnull"];
    default: return ["contains", "ncontains", "eq", "neq", "starts", "null", "notnull"];
  }
}
const OP_LABEL = {
  contains: "contains", ncontains: "does not contain", eq: "is", neq: "is not", starts: "starts with",
  lt: "<", gt: ">", lte: "≤", gte: "≥", null: "is empty", notnull: "is not empty", true: "is checked", false: "is unchecked",
};
const NO_VALUE = new Set(["null", "notnull", "true", "false"]);

class TableView {
  constructor(main, ref) {
    this.main = main;
    this.ref = { schema: ref.schema, name: ref.name };
    this.tab = ref.tab;
    this.undo = [];
    this.renamed = new Map(); // JSON(old key) -> new key
    this.writeChain = Promise.resolve();
    this.lastMs = null;
    this.key = `aa.v:${S.meta.database}:${ref.schema}.${ref.name}`;
    this.settings = Object.assign({ hidden: [], widths: {}, sort: [], filters: [], rh: 0, search: "", fit: false }, store.get(this.key, {}));
    this.urlView = ref.view || null; // adopted once the columns are known
    if (S.pendingFilter && S.pendingFilter.schema === ref.schema && S.pendingFilter.name === ref.name) {
      this.settings.filters = [S.pendingFilter.filter];
      this.settings.search = "";
      S.pendingFilter = null;
    }
    document.title = `${ref.name} · airadminer`;
    main.innerHTML = topbar(`<div class="crumb"><span class="sch">${esc(ref.schema)} /</span><h2>${esc(ref.name)}</h2></div>`) + `<div class="grid-wrap"><div class="loadbar"></div></div>`;
    bindTopbar(main);
    this.load();
  }

  async load() {
    try {
      this.meta = await getMeta(this.ref.schema, this.ref.name);
    } catch (e) {
      this.main.innerHTML = topbar(`<div class="crumb"><h2>${esc(this.ref.name)}</h2></div>`) + `<div class="empty">${icon("t-other")}<h3>Cannot open ${esc(this.ref.schema)}.${esc(this.ref.name)}</h3><p>${esc(e.message)}</p></div>`;
      bindTopbar(this.main);
      return;
    }
    if (this.dead) return;
    const m = this.meta;
    if (this.urlView) {
      const view = this.urlView;
      this.urlView = null;
      await this.adopt(view);
      if (this.dead) return;
    }
    this.clean();
    this.editable = !!m.keyMode && !S.meta.readonly;
    this.offset = m.keyMode === "ctid" ? 1 : 0;
    this.renderFrame();
    this.syncURL();
  }

  // Drops settings that name fields the table no longer has.
  clean() {
    const cols = this.meta.columns;
    const names = new Set(cols.map((c) => c.name));
    this.settings.hidden = this.settings.hidden.filter((h) => names.has(h));
    if (cols.length && this.settings.hidden.length >= cols.length) this.settings.hidden = this.settings.hidden.filter((h) => h !== cols[0].name);
    this.settings.sort = this.settings.sort.filter((s) => names.has(s.col));
    this.settings.filters = this.settings.filters.filter((f) => f.op === "sql" ? S.meta.sql !== false : names.has(f.col));
  }

  // Takes the search, filters, sorts and hidden fields from a link. A raw SQL
  // filter runs with the viewer's privileges, so one the viewer does not
  // already have is applied only after they confirm it.
  async adopt(view) {
    const own = new Set(this.settings.filters.filter((f) => f.op === "sql").map((f) => (f.val || "").trim()));
    const sqls = view.filters.filter((f) => f.op === "sql" && f.val.trim() && !own.has(f.val.trim()));
    let filters = view.filters;
    if (sqls.length) {
      const ok = S.meta.sql !== false && (await dialog({
        title: "Apply SQL filter from link?",
        body: `This link filters ${this.ref.name} with SQL that runs as you: ${sqls.map((f) => f.val.trim()).join(" AND ")}`,
        ok: "Apply filter", cancel: "Skip it", danger: false,
      }));
      if (!ok) filters = filters.filter((f) => !sqls.includes(f));
    }
    Object.assign(this.settings, { search: view.search, filters, sort: view.sort, hidden: view.hidden });
    this.save();
  }

  // A link to the table that is already open, e.g. Back, or a pasted URL.
  async fromURL(view) {
    if (!this.meta) { this.urlView = view; return; }
    if (viewParams(view) === viewParams(this.settings)) return;
    await this.adopt(view);
    if (this.dead) return;
    this.clean();
    const search = $("[data-search]", this.main);
    if (search) search.value = this.settings.search;
    if (this.grid) this.grid.setCols(this.columns());
    this.applyView();
  }

  // Writes the current view into the address bar, without a history entry.
  syncURL() {
    if (S.view !== this || this.dead || !this.meta) return;
    const q = viewParams(this.settings);
    const h = `#/t/${encodeURIComponent(this.ref.schema)}/${encodeURIComponent(this.ref.name)}${this.tab === "structure" ? "/structure" : ""}${q ? "?" + q : ""}`;
    if (location.hash !== h) history.replaceState(history.state, "", h);
  }

  save() { store.set(this.key, this.settings); this.syncURL(); }

  renderFrame() {
    const m = this.meta;
    const kindName = m.hypertable ? "Hypertable" : { r: "", v: "View", m: "Materialized view", f: "Foreign table", p: "Partitioned" }[m.kind];
    let badge = "";
    if (S.meta.readonly) badge = `<span class="badge warn">Read-only session</span>`;
    else if (!m.keyMode && m.hypertable) {
      const dims = (m.dimensions || []).map((d) => qid(d)).join(", ") || "…";
      const tip = `A compressed hypertable can be edited only with a primary key or a unique key on not-null columns, which must include ${dims}. For example:\nALTER TABLE ${qid(m.schema)}.${qid(m.name)} ADD UNIQUE (${dims});`;
      badge = `<span class="badge" title="${esc(tip)}">Read-only: no primary key</span>`;
    }
    else if (!m.keyMode) badge = `<span class="badge" title="Rows can be edited only in tables with a primary key or a unique key on not-null columns">${kindName && m.kind !== "p" ? "Read-only " + kindName.toLowerCase() : "Read-only: no primary key"}</span>`;
    else if (kindName) badge = `<span class="badge">${kindName}</span>`;
    const base = `#/t/${encodeURIComponent(m.schema)}/${encodeURIComponent(m.name)}`;
    this.main.innerHTML = topbar(`
      <div class="crumb"><span class="sch">${esc(m.schema)} /</span><h2>${esc(m.name)}</h2>${m.comment ? `<span class="cmt" title="${esc(m.comment)}">${esc(m.comment)}</span>` : ""}</div>
      <nav class="tabs"><a href="${base}" data-tab="grid">${icon("i-grid", "sm")}Grid</a><a href="${base}/structure" data-tab="structure">${icon("i-struct", "sm")}Structure</a></nav>
      <span class="spacer"></span>${badge}
      <button class="btn icon" data-refresh title="Reload rows">${icon("i-refresh")}</button>`) + `<div data-body style="display:flex;flex-direction:column;flex:1;min-height:0"></div>`;
    bindTopbar(this.main);
    $("[data-refresh]", this.main).onclick = () => this.refresh(true);
    this.body = $("[data-body]", this.main);
    this.setTab(this.tab, true);
  }

  setTab(tab, force) {
    if (!this.meta) { this.tab = tab; return; }
    if (tab === this.tab && !force) return;
    this.tab = tab;
    $$(".tabs a", this.main).forEach((a) => a.classList.toggle("on", a.dataset.tab === tab));
    if (this.grid) { this.grid.destroy(); this.grid = null; }
    if (tab === "structure") this.renderStructure();
    else this.renderGrid();
  }

  // ---------- grid tab

  renderGrid() {
    this.body.innerHTML = `<div class="toolbar" data-tools></div><div class="grid-wrap" data-gw></div>`;
    this.rows = new RowStore(this);
    this.autoW ||= new Map();
    this.grid = new Grid($("[data-gw]", this.body), {
      label: this.meta.name,
      source: this.rows,
      rh: this.settings.rh,
      fit: this.settings.fit,
      pick: this.editable,
      onExpand: (r) => this.expand(r),
      onAdd: this.editable ? () => openRecord({ meta: this.meta, view: this, isNew: true }) : null,
      onEdit: this.editable ? (r, col, v, row) => this.writeCell(r, col.name, v, row) : null,
      onHeader: (col, el) => this.columnMenu(col, el),
      onResize: (col) => { this.settings.widths[col.name] = col.width; this.save(); },
      onLink: (r, col) => { const row = this.rows.get(r); if (row && row[col.idx] != null) openLinked(col.fk, row[col.idx]); },
      onClear: this.editable ? (s) => this.clearCells(s) : null,
      onPaste: this.editable ? (s, m) => this.paste(s, m) : null,
      onUndo: () => this.undoLast(),
      onContext: (e, hit) => this.contextMenu(e, hit),
      onDuplicate: this.editable ? () => this.duplicateRows(this.targetRows()) : null,
      onPick: (set) => this.paintTools(set),
      suggest: (col, q) => this.suggest(col, q),
      status: () => this.status(),
      full: (row, col) => fullRow(this.meta, row, { names: [col.name], key: this.resolveKey(this.keyOf(row)) }),
      copyRows: (r0, r1, cols) => this.copyRows(r0, r1, cols),
    });
    this.grid.setCols(this.columns());
    this.paintTools(this.grid.picked);
    this.refresh();
  }

  columns() {
    const hidden = new Set(this.settings.hidden);
    const sortBy = new Map(this.settings.sort.map((s) => [s.col, s.desc ? "desc" : "asc"]));
    const filtered = new Set(this.activeFilters().map((f) => f.col));
    return this.meta.columns.map((c, i) => ({ c, i })).filter(({ c }) => !hidden.has(c.name)).map(({ c, i }) => {
      const kind = kindOf(c);
      const idx = i + this.offset;
      const w = this.settings.widths[c.name] || this.autoW.get(c.name) || columnWidth({ label: c.name, kind, pk: c.pk, idx }, null);
      return {
        name: c.name, label: c.name, kind, idx, width: Math.round(w), type: c.type,
        pk: c.pk, fk: c.fk, enum: c.enum, nullable: c.nullable, meta: c,
        readonly: !this.editable || c.generated === "s" || c.identity === "a" || kind === "blob",
        sortDir: sortBy.get(c.name), filtered: filtered.has(c.name),
      };
    });
  }

  // Default widths from the first rows loaded, for fields without a saved
  // width. Measured once per open; not saved, so they follow the data.
  sizeColumns(rows) {
    this.meta.columns.forEach((c, i) => {
      this.autoW.set(c.name, columnWidth({ label: c.name, kind: kindOf(c), pk: c.pk, idx: i + this.offset }, rows));
    });
    if (this.grid && this.meta.columns.some((c) => !this.settings.widths[c.name])) this.grid.setCols(this.columns());
  }

  activeFilters() {
    return this.settings.filters.filter((f) => f.op === "sql" ? (f.val || "").trim() : NO_VALUE.has(f.op) || (f.val ?? "") !== "");
  }

  query() {
    return { schema: this.meta.schema, name: this.meta.name, sort: this.settings.sort, filters: this.activeFilters(), search: this.settings.search };
  }

  refresh(keepScroll) {
    if (!this.grid) return;
    this.rows.reset();
    this.grid.picked.clear();
    if (!keepScroll) { this.grid.sc.scrollTop = 0; this.grid.sel = null; }
    this.grid.setCols(this.columns());
    this.paintTools(this.grid.picked);
    this.rows.start();
  }

  loading(on) {
    const gw = $("[data-gw]", this.body);
    if (!gw) return;
    let bar = $(".loadbar", gw);
    if (on && !bar) gw.appendChild(html(`<div class="loadbar"></div>`));
    if (!on && bar) bar.remove();
  }

  countChanged() {
    if (!this.grid) return;
    this.grid.layout();
    this.grid.draw(true);
    const rc = $("[data-count]", this.body);
    if (rc) rc.textContent = this.countText();
  }

  countText() {
    const r = this.rows;
    if (!r.ready && !r.total) return "Loading…";
    return (r.exact ? "" : "≈ ") + plural(r.total, "record");
  }

  status() {
    if (!this.rows) return "";
    let s = `<span><b>${this.countText()}</b></span>`;
    if (this.activeFilters().length || this.settings.search) s += `<span>filtered</span>`;
    if (this.lastMs != null) s += `<span>${this.lastMs} ms</span>`;
    if (this.editable) s += `<span class="muted">Double-click or Enter to edit, Space to expand, ${MOD}+C / ${MOD}+V to copy and paste</span>`;
    return s;
  }

  paintTools(picked) {
    const t = $("[data-tools]", this.body);
    if (!t) return;
    if (picked && picked.size) {
      if (t.dataset.mode === "sel" && $("[data-n]", t)) { $("[data-n]", t).textContent = plural(picked.size, "record") + " selected"; return; }
      t.dataset.mode = "sel";
      t.innerHTML = `<div class="selbar"><span class="what" data-n>${plural(picked.size, "record")} selected</span>
        <button class="btn danger" data-del>${icon("i-trash")}Delete</button>
        <button class="btn" data-dup>${icon("i-copy")}Duplicate</button>
        <button class="btn" data-copykeys>${icon("i-copy")}Copy as TSV</button>
        <button class="btn" data-clear>${icon("i-x")}Clear selection</button></div>`;
      $("[data-del]", t).onclick = () => this.deletePicked();
      $("[data-dup]", t).onclick = () => this.duplicatePicked();
      $("[data-clear]", t).onclick = () => { this.grid.picked.clear(); this.grid.paintSel(); };
      $("[data-copykeys]", t).onclick = async () => {
        const cols = this.grid.cols;
        const rows = await this.rowsAt([...this.grid.picked].sort((a, b) => a - b), cols, "copy");
        if (!rows) return;
        copyMatrix([cols.map((c) => c.name), ...rows.map((row) => cols.map((c) => row[c.idx]))]);
        toast(`Copied ${plural(rows.length, "record")}`);
      };
      return;
    }
    if (t.dataset.mode === "normal") {
      this.paintToolState();
      return;
    }
    t.dataset.mode = "normal";
    t.innerHTML = `
      <button class="btn" data-fields>${icon("i-eyeoff")}<span class="lbl-txt" data-l>Hide fields</span></button>
      <button class="btn" data-filter>${icon("i-filter")}<span class="lbl-txt" data-l>Filter</span></button>
      <button class="btn" data-sort>${icon("i-sort")}<span class="lbl-txt" data-l>Sort</span></button>
      <button class="btn" data-view title="Row height, density and fit to screen">${icon("i-columns")}<span class="lbl-txt" data-l>View</span></button>
      <span class="sep"></span>
      <div class="tsearch">${icon("i-search", "sm")}<input class="input" data-search placeholder="Search all fields" spellcheck="false" aria-label="Search all fields"></div>
      <span class="spacer"></span>
      <span class="reccount" data-count>${this.countText()}</span>
      <button class="btn" data-export title="Download the filtered rows as CSV">${icon("i-download")}<span class="lbl-txt">CSV</span></button>
      ${this.editable ? `<button class="btn primary" data-add>${icon("i-plus")}<span class="lbl-txt">Add record</span></button>` : ""}`;
    const search = $("[data-search]", t);
    search.value = this.settings.search;
    const go = debounce(() => { this.settings.search = search.value; this.save(); this.refresh(); }, 280);
    search.addEventListener("input", go);
    search.addEventListener("keydown", (e) => { if (e.key === "Escape") { search.value = ""; go(); } if (e.key === "Enter" || e.key === "ArrowDown") { this.grid.el.focus(); if (!this.grid.sel) this.grid.select(0, 0); } });
    $("[data-fields]", t).onclick = (e) => this.fieldsMenu(e.currentTarget);
    $("[data-filter]", t).onclick = (e) => this.filterMenu(e.currentTarget);
    $("[data-sort]", t).onclick = (e) => this.sortMenu(e.currentTarget);
    $("[data-view]", t).onclick = (e) => viewMenu(e.currentTarget, this);
    $("[data-export]", t).onclick = () => this.exportCSV();
    const add = $("[data-add]", t);
    if (add) add.onclick = () => openRecord({ meta: this.meta, view: this, isNew: true });
    this.paintToolState();
  }

  paintToolState() {
    const t = $("[data-tools]", this.body);
    if (!t || t.dataset.mode !== "normal") return;
    const set = (sel, n, idle, active) => {
      const b = $(sel, t);
      b.classList.toggle("on", n > 0);
      $("[data-l]", b).textContent = n > 0 ? active(n) : idle;
    };
    set("[data-fields]", this.settings.hidden.length, "Hide fields", (n) => `${n} hidden ${n === 1 ? "field" : "fields"}`);
    set("[data-filter]", this.activeFilters().length, "Filter", (n) => `Filtered by ${n} ${n === 1 ? "field" : "fields"}`);
    set("[data-sort]", this.settings.sort.length, "Sort", (n) => `Sorted by ${n} ${n === 1 ? "field" : "fields"}`);
    set("[data-view]", this.settings.fit ? 1 : 0, "View", () => "Fit to screen");
    const rc = $("[data-count]", t);
    if (rc) rc.textContent = this.countText();
  }

  applyView() { this.save(); this.paintToolState(); this.refresh(); }

  fieldsMenu(anchor) {
    const el = html(`<div class="fieldlist" style="width:280px"><div class="scroll" data-l></div><hr><div style="display:flex;gap:4px;padding:2px"><button class="btn" data-all style="flex:1">Show all</button><button class="btn" data-none style="flex:1">Hide all</button></div></div>`);
    const paint = () => {
      const hidden = new Set(this.settings.hidden);
      $("[data-l]", el).innerHTML = this.meta.columns.map((c) => `<button class="mi" data-n="${esc(c.name)}">${icon(KIND_ICON[kindOf(c)], "sm")}<span style="overflow:hidden;text-overflow:ellipsis">${esc(c.name)}</span><span class="tog ${hidden.has(c.name) ? "" : "on"}"></span></button>`).join("");
    };
    paint();
    openPop(anchor, el);
    const apply = () => { this.save(); this.paintToolState(); this.grid.setCols(this.columns()); paint(); };
    el.addEventListener("click", (e) => {
      const b = e.target.closest("[data-n]");
      if (b) {
        const n = b.dataset.n, h = this.settings.hidden;
        if (h.includes(n)) this.settings.hidden = h.filter((x) => x !== n);
        else if (h.length < this.meta.columns.length - 1) h.push(n);
        apply();
      }
      if (e.target.closest("[data-all]")) { this.settings.hidden = []; apply(); }
      if (e.target.closest("[data-none]")) { this.settings.hidden = this.meta.columns.slice(1).map((c) => c.name); apply(); }
    });
  }

  filterMenu(anchor, focusLast) {
    const el = html(`<div class="rules"></div>`);
    const cols = this.meta.columns;
    const apply = debounce(() => this.applyView(), 350);
    // Suggested values for text fields. A native <datalist> popup is drawn
    // by the browser and ignores the app's styles, so the menu draws its own.
    const sl = html(`<div class="sug" hidden></div>`);
    const paint = () => {
      const fs = this.settings.filters;
      el.innerHTML = (fs.length ? fs.map((f, i) => {
        const lbl = `<span class="lbl">${i === 0 ? "Where" : "and"}</span>`;
        const rm = `<button class="btn icon" data-rm="${i}" title="Remove condition">${icon("i-x", "sm")}</button>`;
        if (f.op === "sql") return `<div class="rule" style="grid-template-columns:52px 1fr 30px">${lbl}<input class="input mono" data-i="${i}" data-k="val" value="${esc(f.val || "")}" placeholder="SQL condition, e.g. total > 100 AND note IS NOT NULL" spellcheck="false">${rm}</div>`;
        const col = cols.find((c) => c.name === f.col) || cols[0];
        const kind = kindOf(col);
        const ops = opsFor(kind);
        let val = "";
        if (!NO_VALUE.has(f.op)) {
          val = col.enum
            ? `<select class="select" data-i="${i}" data-k="val"><option value="">Choose…</option>${col.enum.map((x) => `<option ${x === f.val ? "selected" : ""}>${esc(x)}</option>`).join("")}</select>`
            : `<input class="input" data-i="${i}" data-k="val" value="${esc(f.val || "")}" placeholder="${kind === "datetime" || kind === "date" ? "2025-01-31" : "Value"}" spellcheck="false"${kind === "text" ? ` data-sug autocomplete="off"` : ""}>`;
        }
        return `<div class="rule">${lbl}
          <select class="select" data-i="${i}" data-k="col">${cols.map((c) => `<option ${c.name === col.name ? "selected" : ""} value="${esc(c.name)}">${esc(c.name)}</option>`).join("")}</select>
          <select class="select" data-i="${i}" data-k="op">${ops.map((o) => `<option value="${o}" ${o === f.op ? "selected" : ""}>${OP_LABEL[o]}</option>`).join("")}</select>
          ${val || "<span></span>"}${rm}</div>`;
      }).join("") : `<div class="none">No filters yet. Add a condition to narrow the rows.</div>`) +
      `<div class="foot"><span><button class="btn" data-add>${icon("i-plus", "sm")}Add condition</button>${S.meta.sql !== false ? `<button class="btn" data-addsql>${icon("i-terminal", "sm")}Add SQL condition</button>` : ""}</span>${fs.length ? `<button class="btn" data-clearall>Clear all</button>` : ""}</div>`;
      el.appendChild(sl);
      sl.hidden = true;
      curPop && curPop.place();
    };
    paint();
    openPop(anchor, el);
    const focusRule = () => { const ins = $$("input[data-k=val], select[data-k=val]", el); (ins[ins.length - 1] || null)?.focus(); };
    // Suggestions are kept while the menu is open.
    // A newer request cancels the one before, which frees its connection.
    const known = new Map();
    let seq = 0, ctl = null, sugFor = null, items = [], hi = -1;
    const hideSug = () => { sl.hidden = true; sugFor = null; items = []; hi = -1; };
    const paintSug = () => {
      sl.innerHTML = items.map((v, i) => `<button class="mi ${i === hi ? "hi" : ""}" data-s="${i}" tabindex="-1">${esc(v)}</button>`).join("");
      sl.querySelector(".hi")?.scrollIntoView({ block: "nearest" });
    };
    const showSug = (input, vals) => {
      items = vals; hi = -1; sugFor = input;
      if (!items.length) { sl.hidden = true; return; }
      paintSug();
      // Offsets from the menu box, which is the list's containing block.
      const pr = el.getBoundingClientRect(), r = input.getBoundingClientRect();
      sl.style.left = r.left - pr.left - el.clientLeft + "px";
      sl.style.top = r.bottom - pr.top - el.clientTop + 4 + "px";
      sl.style.width = r.width + "px";
      sl.hidden = false;
    };
    const pick = (input, v) => {
      input.value = v;
      this.settings.filters[+input.dataset.i].val = v;
      hideSug(); apply();
    };
    const suggest = async (input) => {
      const f = this.settings.filters[+input.dataset.i];
      if (!f) return;
      const key = f.col + "\0" + input.value, my = ++seq;
      if (!known.has(key)) {
        ctl?.abort();
        ctl = new AbortController();
        known.set(key, api("values", { ...this.ref, col: f.col, q: input.value }, { signal: ctl.signal }).then((r) => r.values, () => (known.delete(key), [])));
      }
      const vals = await known.get(key);
      if (my !== seq || !input.isConnected || document.activeElement !== input) return;
      showSug(input, vals.filter((v) => v !== input.value));
    };
    const suggestSoon = debounce(suggest, 200);
    el.addEventListener("focusin", (e) => { if (e.target.dataset?.sug !== undefined) suggest(e.target); });
    el.addEventListener("focusout", (e) => { if (e.target === sugFor) { seq++; hideSug(); } });
    sl.addEventListener("mousedown", (e) => {
      const b = e.target.closest("[data-s]");
      e.preventDefault();
      if (b && sugFor) pick(sugFor, items[+b.dataset.s]);
    });
    el.addEventListener("keydown", (e) => {
      if (sl.hidden || e.target !== sugFor) return;
      if (e.key === "ArrowDown") { e.preventDefault(); hi = Math.min(items.length - 1, hi + 1); paintSug(); }
      else if (e.key === "ArrowUp") { e.preventDefault(); hi = Math.max(-1, hi - 1); paintSug(); }
      else if (e.key === "Enter" && hi >= 0) { e.preventDefault(); pick(sugFor, items[hi]); }
    });
    // Escape closes the list first and the menu only on a second press.
    // The menu listens on document in the capture phase, so this listens on window.
    const onEsc = (e) => {
      if (!el.isConnected) { removeEventListener("keydown", onEsc, true); return; }
      if (e.key === "Escape" && !sl.hidden) { e.preventDefault(); e.stopPropagation(); seq++; hideSug(); }
    };
    addEventListener("keydown", onEsc, true);
    if (focusLast) focusRule();
    el.addEventListener("input", (e) => {
      const t = e.target, i = t.dataset.i;
      if (i === undefined) return;
      const f = this.settings.filters[+i];
      if (t.dataset.k === "val") { f.val = t.value; apply(); if (t.dataset.sug !== undefined) suggestSoon(t); return; }
      if (t.dataset.k === "col") {
        f.col = t.value;
        const ops = opsFor(kindOf(cols.find((c) => c.name === f.col)));
        if (!ops.includes(f.op)) f.op = ops[0];
        f.val = "";
      }
      if (t.dataset.k === "op") f.op = t.value;
      paint(); apply();
    });
    el.addEventListener("click", (e) => {
      if (e.target.closest("[data-add]")) {
        const c = cols[0];
        this.settings.filters.push({ col: c.name, op: opsFor(kindOf(c))[0], val: "" });
        paint(); focusRule();
      } else if (e.target.closest("[data-addsql]")) {
        this.settings.filters.push({ op: "sql", val: "" });
        paint(); focusRule();
      } else if (e.target.closest("[data-rm]")) {
        this.settings.filters.splice(+e.target.closest("[data-rm]").dataset.rm, 1);
        paint(); apply();
      } else if (e.target.closest("[data-clearall]")) {
        this.settings.filters = []; paint(); apply();
      }
    });
  }

  sortMenu(anchor) {
    const el = html(`<div class="rules"></div>`);
    const cols = this.meta.columns;
    const paint = () => {
      const ss = this.settings.sort;
      el.innerHTML = (ss.length ? ss.map((s, i) => {
        const kind = kindOf(cols.find((c) => c.name === s.col) || cols[0]);
        const [a, z] = kind === "num" ? ["1 → 9", "9 → 1"] : kind === "date" || kind === "datetime" ? ["Oldest first", "Newest first"] : kind === "bool" ? ["False first", "True first"] : ["A → Z", "Z → A"];
        return `<div class="rule sort"><span class="lbl">${i === 0 ? "Sort by" : "then"}</span>
          <select class="select" data-i="${i}" data-k="col">${cols.map((c) => `<option ${c.name === s.col ? "selected" : ""} value="${esc(c.name)}">${esc(c.name)}</option>`).join("")}</select>
          <select class="select" data-i="${i}" data-k="dir"><option value="asc">${a}</option><option value="desc" ${s.desc ? "selected" : ""}>${z}</option></select>
          <button class="btn icon" data-rm="${i}" title="Remove sort">${icon("i-x", "sm")}</button></div>`;
      }).join("") : `<div class="none">Rows follow the primary key. Pick a field to sort by.</div>`) +
      `<div class="foot"><button class="btn" data-add>${icon("i-plus", "sm")}Add sort</button>${ss.length ? `<button class="btn" data-clearall>Clear all</button>` : ""}</div>`;
      curPop && curPop.place();
    };
    paint();
    openPop(anchor, el);
    el.addEventListener("input", (e) => {
      const t = e.target, i = t.dataset.i;
      if (i === undefined) return;
      const s = this.settings.sort[+i];
      if (t.dataset.k === "col") s.col = t.value;
      if (t.dataset.k === "dir") s.desc = t.value === "desc";
      paint(); this.applyView();
    });
    el.addEventListener("click", (e) => {
      if (e.target.closest("[data-add]")) {
        const used = new Set(this.settings.sort.map((s) => s.col));
        const c = cols.find((x) => !used.has(x.name)) || cols[0];
        this.settings.sort.push({ col: c.name, desc: false });
        paint(); this.applyView();
      } else if (e.target.closest("[data-rm]")) {
        this.settings.sort.splice(+e.target.closest("[data-rm]").dataset.rm, 1);
        paint(); this.applyView();
      } else if (e.target.closest("[data-clearall]")) {
        this.settings.sort = []; paint(); this.applyView();
      }
    });
  }

  // Hooks for viewMenu() and the fit shortcut.
  get rowHeight() { return this.settings.rh; }
  setRowHeight(i) { this.settings.rh = i; this.save(); this.grid && this.grid.setRowHeight(i); }
  get fit() { return !!this.settings.fit; }
  toggleFit() {
    if (!this.grid) return;
    this.settings.fit = !this.settings.fit; this.save();
    this.grid.setFit(this.settings.fit);
    this.paintToolState();
    toast(this.settings.fit ? "Columns fit to screen" : "Columns at saved widths", { ms: 1400 });
  }

  columnMenu(col, anchor) {
    const c = col.meta;
    const tags = [c.pk && (this.meta.keyIndex ? "unique key" : "primary key"), c.fk && `→ ${c.fk.table}.${c.fk.column}`, !c.nullable && "not null", c.identity && "identity", c.generated && "generated"].filter(Boolean);
    const kind = col.kind;
    const [a, z] = kind === "num" ? ["1 → 9", "9 → 1"] : kind === "date" || kind === "datetime" ? ["oldest first", "newest first"] : ["A → Z", "Z → A"];
    const el = html(`<div style="width:250px">
      <h4><span class="mono">${esc(c.type)}</span>${tags.length ? `<span class="muted" style="display:block;font-weight:450;margin-top:2px">${esc(tags.join(", "))}</span>` : ""}${c.comment ? `<span style="display:block;font-weight:450;margin-top:4px;color:var(--ink-2)">${esc(c.comment)}</span>` : ""}</h4>
      <button class="mi" data-a="asc">${icon("i-arrow-up")}Sort ${a}</button>
      <button class="mi" data-a="desc">${icon("i-arrow-down")}Sort ${z}</button>
      <button class="mi" data-a="filter">${icon("i-filter")}Filter by this field</button>
      <hr>
      <button class="mi" data-a="hide">${icon("i-eyeoff")}Hide field</button>
      <button class="mi" data-a="fit">${icon("i-columns")}Fit width to content</button>
      <button class="mi" data-a="copy">${icon("i-copy")}Copy field name</button>
      ${c.fk ? `<hr><button class="mi" data-a="fk">${icon("i-link")}Open ${esc(c.fk.table)}</button>` : ""}
    </div>`);
    openPop(anchor, el);
    menuKeys(el, el);
    el.addEventListener("click", (e) => {
      const b = e.target.closest("[data-a]");
      if (!b) return;
      closePop();
      const act = b.dataset.a;
      if (act === "asc" || act === "desc") { this.settings.sort = [{ col: c.name, desc: act === "desc" }]; this.applyView(); }
      if (act === "filter") {
        this.settings.filters.push({ col: c.name, op: opsFor(kind)[0], val: "" });
        this.save();
        this.filterMenu($("[data-filter]", this.body), true);
      }
      if (act === "hide") { this.settings.hidden.push(c.name); this.save(); this.paintToolState(); this.grid.setCols(this.columns()); }
      if (act === "fit") this.grid.autosize(col);
      if (act === "copy") { copyText(c.name); toast("Copied field name"); }
      if (act === "fk") location.hash = `#/t/${encodeURIComponent(c.fk.schema)}/${encodeURIComponent(c.fk.table)}`;
    });
  }

  async exportCSV() {
    try {
      toast("Preparing CSV…", { ms: 1500 });
      const r = await api("export", this.query(), { raw: true });
      download(`${this.meta.name}.csv`, await r.blob());
    } catch (e) { toastErr(e); }
  }

  // ---------- editing

  keyOf(row) {
    const m = this.meta;
    if (m.keyMode === "ctid") return { __ctid: row[0] };
    const k = {};
    m.pk.forEach((p) => { k[p] = row[m.columns.findIndex((c) => c.name === p) + this.offset]; });
    return k;
  }

  // ---------- row identity
  //
  // Writes are addressed by row key, never by row position: a refresh can
  // move a record to another position, and a key can change (an edited
  // primary key, or ctid/xmin after any update). `renamed` maps every key a
  // write replaced to the key the database returned, so later writes and
  // undo entries that still hold the old key reach the same record.

  noteRename(oldKey, newKey) {
    const a = JSON.stringify(oldKey), b = JSON.stringify(newKey);
    if (a === b) return;
    this.renamed.set(a, newKey);
    if (this.renamed.size > 5000) this.renamed.delete(this.renamed.keys().next().value);
  }

  resolveKey(key) {
    for (let n = 0; n < 10000; n++) {
      const next = this.renamed.get(JSON.stringify(key));
      if (!next) break;
      key = next;
    }
    return key;
  }

  // Position of the loaded row with this key (trying `hint` first), or -1.
  locate(key, hint) {
    const want = JSON.stringify(key);
    const is = (row) => row && JSON.stringify(this.keyOf(row)) === want;
    if (hint != null && hint >= 0 && is(this.rows.get(hint))) return hint;
    for (const [p, rows] of this.rows.pages) {
      for (let i = 0; i < rows.length; i++) if (is(rows[i])) return p * PAGE + i;
    }
    return -1;
  }

  // Replaces the row with key `oldKey` wherever it is now loaded.
  putRow(oldKey, row, hint, flashCols) {
    const r = this.locate(oldKey, hint);
    if (r >= 0) {
      this.rows.set(r, row);
      (flashCols || []).forEach((n) => this.grid && this.grid.flash(r, n));
    }
    drawerRowChanged(this, oldKey, row);
    return r;
  }

  // Runs writes one after another, so responses can never apply out of
  // order and each write sends the key the previous one returned.
  serial(fn) {
    const run = this.writeChain.then(fn, fn);
    this.writeChain = run.catch(() => {});
    return run;
  }

  // ---------- writes
  //
  // Every grid write goes through applyChanges: one request, one transaction.
  // Each change carries the row as the user saw it; the server refuses the
  // write if any edited cell no longer has that value (someone else changed it).

  async writeCell(r, name, value, row) {
    row = row || this.rows.get(r);
    if (!row) { toast("That row is not loaded yet; nothing was saved", { err: true }); return false; }
    if (row[this.meta.columns.findIndex((c) => c.name === name) + this.offset] === value) return true;
    return this.applyChanges([{ r, row, values: { [name]: value } }]);
  }

  // changes: [{ r, row, values }]: r is a position hint, row the row as seen.
  // inserts: [values] for new records.
  applyChanges(changes, inserts = [], opts = {}) {
    return this.serial(() => this.applyNow(changes, inserts, opts));
  }

  async applyNow(changes, inserts, { label } = {}) {
    const cols = this.meta.columns;
    const idx = (n) => cols.findIndex((c) => c.name === n) + this.offset;
    const seen = (ch) => Object.fromEntries(Object.keys(ch.values).map((n) => [n, ch.row[idx(n)]]));
    const keys = changes.map((ch) => this.resolveKey(this.keyOf(ch.row)));
    // A cut cell being overwritten was never seen in full. Load its full
    // value, checking it still is what the grid showed, so the server's
    // conflict check and undo get the whole value.
    try {
      const full = await fullRows(this.meta, changes.map((ch) => ch.row), { names: (k) => Object.keys(changes[k].values), keys, check: true });
      changes = changes.map((ch, k) => full[k] === ch.row ? ch : { ...ch, row: full[k] });
    } catch (e) {
      changes.forEach((ch, i) => { const r = this.locate(keys[i], ch.r); if (r >= 0 && this.grid) this.grid.markErr(r, Object.keys(ch.values)[0]); });
      toastErr(e);
      return false;
    }
    const updates = changes.map((ch, i) => ({ key: keys[i], values: ch.values, expect: seen(ch) }));
    // Show the new values right away; remember what was there to put it back on failure.
    const shown = changes.map((ch, i) => {
      const r = this.locate(keys[i], ch.r);
      if (r < 0) return null;
      const prev = this.rows.get(r), opt = prev.slice();
      if (prev.cut) opt.cut = { ...prev.cut };
      for (const [n, v] of Object.entries(ch.values)) { opt[idx(n)] = v; if (opt.cut) delete opt.cut[idx(n)]; }
      this.rows.set(r, opt);
      return { prev };
    });
    if (changes.length && this.grid) this.grid.draw(true);
    try {
      const res = await api("batch", { schema: this.meta.schema, name: this.meta.name, updates, inserts, deletes: [] });
      const undoRows = res.updated.map((row, i) => {
        const names = Object.keys(changes[i].values);
        this.noteRename(keys[i], this.keyOf(row));
        const r = this.putRow(keys[i], row, changes[i].r, names);
        // A value the server returned cut is remembered by its hash.
        return { r, key: this.keyOf(row), old: seen(changes[i]), now: Object.fromEntries(names.map((n) => [n, isCut(row, idx(n)) ? { hash: row.cut[idx(n)] } : row[idx(n)]])) };
      });
      this.pushUndo({ rows: undoRows, created: res.inserted.map((row) => this.keyOf(row)), label });
      if (this.grid) this.grid.draw(true);
      if (res.inserted.length) this.refresh(true);
      return true;
    } catch (e) {
      changes.forEach((ch, i) => {
        if (!shown[i]) return;
        const r = this.locate(keys[i], ch.r);
        if (r >= 0) this.rows.set(r, shown[i].prev);
      });
      if (this.grid) {
        this.grid.draw(true);
        const bad = changes[e.index ?? 0];
        if (bad && (e.index == null || e.index < changes.length)) {
          const r = this.locate(keys[e.index ?? 0], bad.r);
          if (r >= 0) this.grid.markErr(r, Object.keys(bad.values)[0]);
        }
      }
      if (changes.length + inserts.length > 1) e.detail = [e.detail, "Nothing was saved: a batch is saved completely or not at all."].filter(Boolean).join("  ");
      toastErr(e);
      return false;
    }
  }

  pushUndo(u) {
    this.undo.push(u);
    if (this.undo.length > 200) this.undo.shift();
  }

  // Undo entries: { rows: [{ r, key, old, now }], created: [key], label }.
  // Reverting cells and removing added records happen in one transaction.
  // Undo only applies if the cells still hold what this change wrote, so it
  // never reverts someone else's later edit.
  undoLast() {
    return this.serial(() => this.undoNow());
  }

  async undoNow() {
    const u = this.undo.pop();
    if (!u) { toast("Nothing to undo"); return; }
    const rows = u.rows || [];
    const keys = rows.map((x) => this.resolveKey(x.key));
    const created = (u.created || []).map((k) => this.resolveKey(k));
    try {
      // Values this change left too long to keep come back in full, if
      // they still have the hash the change produced.
      const hashed = rows.map((x, i) => ({ i, names: Object.keys(x.now).filter((n) => x.now[n] && typeof x.now[n] === "object") })).filter((h) => h.names.length);
      const got = await fullCells(this.meta, hashed.map((h) => ({ key: keys[h.i], names: h.names, hashes: Object.fromEntries(h.names.map((n) => [n, rows[h.i].now[n].hash])) })));
      const now = rows.map((x) => ({ ...x.now }));
      hashed.forEach((h, k) => Object.assign(now[h.i], got[k]));
      const res = await api("batch", { schema: this.meta.schema, name: this.meta.name, updates: rows.map((x, i) => ({ key: keys[i], values: x.old, expect: now[i] })), inserts: [], deletes: created });
      res.updated.forEach((row, i) => {
        this.noteRename(keys[i], this.keyOf(row));
        this.putRow(keys[i], row, rows[i].r, Object.keys(rows[i].old));
      });
      if (created.length) this.refresh(true);
      else if (this.grid) this.grid.draw(true);
      const cells = rows.reduce((a, x) => a + Object.keys(x.old).length, 0);
      const what = [cells && (cells === 1 && !created.length ? `change to ${Object.keys(rows[0].old)[0]}` : plural(cells, "cell")), created.length && `removed ${plural(created.length, "added record")}`].filter(Boolean).join(", ");
      toast(`Undid ${u.label ? u.label + ": " : ""}${what}`);
    } catch (e) {
      e.detail = [e.detail, "Undo needs the rows as this change left them; they were changed or deleted since. Nothing was undone."].filter(Boolean).join("  ");
      toastErr(e);
    }
  }

  // Groups [r, name, value] jobs into one change per row, with the row as seen now.
  byRow(jobs) {
    const m = new Map();
    for (const [r, n, v] of jobs) { if (!m.has(r)) m.set(r, {}); m.get(r)[n] = v; }
    return [...m].map(([r, values]) => ({ r, row: this.rows.get(r), values }));
  }

  // Loads every row of a range before acting on it, so nothing is skipped
  // silently because it was not on screen yet.
  async loadRows(r0, r1, what) {
    if (await this.rows.loadRange(r0, r1)) return true;
    toast(`Could not load all the rows to ${what}; nothing was changed`, { err: true });
    return false;
  }

  async clearCells(s) {
    if (s.r1 - s.r0 + 1 > 2000) { toast("Clear up to 2,000 rows at a time"); return; }
    if (!(await this.loadRows(s.r0, s.r1, "clear"))) return;
    const cols = this.grid.cols;
    const jobs = [];
    for (let r = s.r0; r <= s.r1; r++) {
      const row = this.rows.get(r);
      if (!row) continue; // past the last record
      for (let c = s.c0; c <= s.c1; c++) {
        const col = cols[c];
        if (col.readonly || row[col.idx] === null) continue;
        if (col.nullable) jobs.push([r, col.name, null]);
        else if (col.kind === "text" && row[col.idx] !== "") jobs.push([r, col.name, ""]);
      }
    }
    if (!jobs.length) { toast("Nothing to clear: the selected fields are empty, read-only or required"); return; }
    if (jobs.length > 2000) { toast("Clear up to 2,000 cells at a time"); return; }
    const changes = this.byRow(jobs); // rows as seen when the user decided
    if (jobs.length > 1 && !(await dialog({ title: `Clear ${plural(jobs.length, "cell")}?`, body: "Each cell is set to null, or to empty text where null is not allowed. Ctrl/⌘+Z undoes it.", ok: "Clear cells" }))) return;
    if (await this.applyChanges(changes, [], { label: "clear" }) && jobs.length > 1) toast(`Cleared ${plural(jobs.length, "cell")}`);
  }

  async paste(s, matrix) {
    const cols = this.grid.cols;
    const jobs = [], inserts = [];
    const single = matrix.length === 1 && matrix[0].length === 1;
    const rows = single ? s.r1 - s.r0 + 1 : matrix.length;
    const width = single ? s.c1 - s.c0 + 1 : Math.max(...matrix.map((r) => r.length));
    if (rows > 2000) { toast("Paste up to 2,000 rows at a time"); return; }
    if (!(await this.loadRows(s.r0, s.r0 + rows - 1, "paste into"))) return;
    const n = this.rows.count();
    // Rows that fall past the last record become new records, but only when
    // the end of the table is known, i.e. the count is exact.
    const canAdd = this.editable && this.rows.exact && !single;
    const value = (col, raw) => {
      let v = raw;
      if (v === null) return col.nullable === false && col.kind === "text" ? "" : null; // a NULL copied from a grid
      if (v === "" && col.kind !== "text") v = null;
      if (col.kind === "bool" && v != null) v = /^(t|true|yes|y|1|on|x|✓|checked)$/i.test(v.trim()) ? "true" : "false";
      return v;
    };
    let dropped = 0;
    for (let i = 0; i < rows; i++) {
      const r = s.r0 + i;
      if (r >= n || (this.rows.exact && !this.rows.get(r))) {
        if (!canAdd) { dropped++; continue; }
        const values = {};
        for (let j = 0; j < width && s.c0 + j < cols.length; j++) {
          const col = cols[s.c0 + j];
          const raw = (matrix[i][j] === undefined ? "" : matrix[i][j]);
          // Empty cells are left out so the column default applies.
          if (col.readonly || raw === "") continue;
          values[col.name] = value(col, raw);
        }
        inserts.push(values);
        continue;
      }
      if (!this.rows.get(r)) { toast("Could not load all the rows to paste into; nothing was changed", { err: true }); return; }
      for (let j = 0; j < width && s.c0 + j < cols.length; j++) {
        const col = cols[s.c0 + j];
        if (col.readonly) continue;
        jobs.push([r, col.name, value(col, single ? matrix[0][0] : ((matrix[i][j] === undefined ? "" : matrix[i][j])))]);
      }
    }
    if (!jobs.length && !inserts.length) { toast(dropped ? "Nothing to paste: the rows are past the end of the table" : "Nothing to paste into: the selected fields are read-only"); return; }
    if (jobs.length > 2000 || inserts.length > 2000) { toast("Paste up to 2,000 cells at a time"); return; }
    const changes = this.byRow(jobs);
    if (inserts.length || jobs.length > 20 || dropped) {
      const parts = [jobs.length && `updates ${plural(jobs.length, "cell")}`, inserts.length && `adds ${plural(inserts.length, "new record")}`].filter(Boolean).join(" and ");
      const skip = dropped ? ` ${plural(dropped, "pasted row")} past the end of the table will be left out, because new records cannot be added here.` : "";
      const ok = await dialog({ title: inserts.length ? `Add ${plural(inserts.length, "record")}?` : `Paste into ${plural(jobs.length, "cell")}?`, body: `This paste ${parts}, in one transaction.${skip} Ctrl/⌘+Z undoes it.`, ok: "Paste", danger: false });
      if (!ok) return;
    }
    if (await this.applyChanges(changes, inserts, { label: "paste" }) && jobs.length + inserts.length > 1) {
      toast([jobs.length && `Pasted ${plural(jobs.length, "cell")}`, inserts.length && `added ${plural(inserts.length, "record")}`].filter(Boolean).join(", "));
    }
  }

  // Rows an action applies to: the picked rows, or else the selected range.
  targetRows() {
    if (this.grid.picked.size) return [...this.grid.picked].sort((a, b) => a - b);
    const s = this.grid.norm();
    if (!s) return [];
    const out = [];
    for (let r = s.r0; r <= s.r1; r++) out.push(r);
    return out;
  }

  // Keys of the rows at these positions, loading them first.
  async keysAt(list, what) {
    if (!list.length) return [];
    if (list.length > 2000) { toast(`${what[0].toUpperCase() + what.slice(1)} up to 2,000 records at a time`); return null; }
    if (!(await this.loadRows(list[0], list[list.length - 1], what))) return null;
    return list.map((r) => this.rows.get(r)).filter(Boolean).map((row) => this.resolveKey(this.keyOf(row)));
  }

  // Rows at these positions, loaded, with the full text of cut cells in cols.
  async rowsAt(list, cols, what) {
    if (!list.length) return [];
    if (list.length > 10000) { toast(`${what[0].toUpperCase() + what.slice(1)} up to 10,000 rows at a time`); return null; }
    if (!(await this.loadRows(list[0], list[list.length - 1], what))) return null;
    const rows = list.map((r) => this.rows.get(r)).filter(Boolean);
    try {
      return await fullRows(this.meta, rows, { names: cols.map((c) => c.name), keys: rows.map((row) => this.resolveKey(this.keyOf(row))) });
    } catch (e) { toastErr(e); return null; }
  }

  copyRows(r0, r1, cols) {
    const list = [];
    for (let r = r0; r <= Math.min(r1, r0 + 10000); r++) list.push(r);
    if (r1 - r0 + 1 > 10000) { toast("Copy up to 10,000 rows at a time"); return null; }
    return this.rowsAt(list, cols, "copy");
  }

  async deleteRows(list) {
    const keys = await this.keysAt(list, "delete");
    if (!keys || !keys.length) return;
    const ok = await dialog({ title: `Delete ${plural(keys.length, "record")}?`, body: `This removes them from ${this.meta.schema}.${this.meta.name} in one transaction. It cannot be undone.`, ok: `Delete ${plural(keys.length, "record")}` });
    if (!ok) return;
    try {
      const r = await api("delete", { schema: this.meta.schema, name: this.meta.name, keys });
      toast(`Deleted ${plural(r.deleted, "record")}`);
      this.refresh(true);
    } catch (e) { toastErr(e); }
  }

  async duplicateRows(list) {
    const keys = await this.keysAt(list, "duplicate");
    if (!keys || !keys.length) return;
    if (keys.length > 1000) { toast("Duplicate up to 1,000 records at a time"); return; }
    if (keys.length > 20 && !(await dialog({ title: `Duplicate ${plural(keys.length, "record")}?`, body: `Each copy is added to ${this.meta.schema}.${this.meta.name} in one transaction. Ctrl/⌘+Z undoes it.`, ok: "Duplicate", danger: false }))) return;
    try {
      const res = await api("duplicate", { schema: this.meta.schema, name: this.meta.name, keys });
      this.pushUndo({ created: res.rows.map((row) => this.keyOf(row)), label: "duplicate" });
      toast(`Duplicated ${plural(res.rows.length, "record")}`, { action: { label: "Undo", fn: () => this.undoLast() } });
      this.refresh(true);
    } catch (e) {
      if (/duplicate key/.test(e.message || "")) e.hint = "Expand a record and use Duplicate there to give the copy new unique values.";
      toastErr(e);
    }
  }

  deletePicked() { return this.deleteRows([...this.grid.picked].sort((a, b) => a - b)); }
  duplicatePicked() { return this.duplicateRows([...this.grid.picked].sort((a, b) => a - b)); }

  contextMenu(ev, hit) {
    const rows = this.targetRows();
    const n = rows.length;
    const rec = plural(n, "record");
    const ed = this.editable;
    const kbd = (k) => `<span class="r">${k}</span>`;
    const items = [
      ["expand", "i-expand", "Expand record", kbd("Space"), n === 1],
      ["copy", "i-copy", "Copy", kbd(`${MOD}+C`), true],
      ed && ["clear", "i-x", "Clear cells", kbd("Del"), true],
      ed && "-",
      ed && ["dup", "i-copy", n === 1 ? "Duplicate record" : `Duplicate ${rec}`, kbd(`${MOD}+Shift+D`), true],
      ed && ["del", "i-trash", n === 1 ? "Delete record" : `Delete ${rec}`, "", true, "danger"],
    ].filter(Boolean);
    const el = html(`<div style="width:230px;outline:none" role="menu" tabindex="-1">${items.map((it) => it === "-" ? "<hr>" : `<button class="mi ${it[5] || ""}" data-a="${it[0]}" role="menuitem" ${it[4] ? "" : "disabled"}>${icon(it[1])}${esc(it[2])}${it[3]}</button>`).join("")}</div>`);
    const at = { getBoundingClientRect: () => ({ left: ev.clientX, right: ev.clientX, top: ev.clientY, bottom: ev.clientY - 6 }), focus: () => this.grid.el.focus({ preventScroll: true }) };
    openPop(at, el, { cls: "ctx" });
    menuKeys(el, el);
    el.focus({ preventScroll: true }); // arrow keys and Enter work right away
    el.addEventListener("click", (e) => {
      const b = e.target.closest("[data-a]");
      if (!b) return;
      closePop();
      this.grid.el.focus({ preventScroll: true });
      ({
        expand: () => this.expand(hit.r),
        copy: () => this.grid.copy(),
        clear: () => this.clearCells(this.grid.norm()),
        dup: () => this.duplicateRows(rows),
        del: () => this.deleteRows(rows),
      })[b.dataset.a]();
    });
  }

  async suggest(col, q) {
    const fk = col.fk;
    const meta = await cachedMeta(fk.schema, fk.table);
    const lc = labelColumn(meta);
    const off = meta.keyMode === "ctid" ? 1 : 0;
    const idx = meta.columns.findIndex((c) => c.name === fk.column) + off;
    const li = lc ? meta.columns.indexOf(lc) + off : -1;
    const res = await api("rows", { schema: fk.schema, name: fk.table, search: q, limit: 8, sort: lc ? [{ col: lc.name }] : [] });
    return res.rows.map((r) => ({ value: r[idx], label: li >= 0 ? r[li] : "" }));
  }

  expand(r) {
    const row = this.rows.get(r);
    if (row) openRecord({ meta: this.meta, view: this, r, row });
  }

  // ---------- structure tab

  renderStructure() {
    const m = this.meta;
    const kind = { r: "Table", v: "View", m: "Materialized view", f: "Foreign table", p: "Partitioned table" }[m.kind];
    const col = (c) => {
      const k = kindOf(c);
      return `<tr><td><span style="display:inline-flex;align-items:center;gap:7px;font-weight:550">${icon(KIND_ICON[k], "sm")}${esc(c.name)}${c.pk ? icon("i-key", "xs keyic") : ""}</span>${c.comment ? `<div class="muted" style="margin-top:2px">${esc(c.comment)}</div>` : ""}</td>
        <td class="mono">${esc(c.type)}${c.enum ? `<div class="pills" style="margin-top:6px;flex-wrap:wrap">${c.enum.map((x, i) => `<span class="pill" style="--h:${(i * 67 + 205) % 360}">${esc(x)}</span>`).join("")}</div>` : ""}</td>
        <td>${c.nullable ? `<span class="muted">nullable</span>` : "not null"}</td>
        <td class="mono">${c.identity ? `identity (${c.identity === "a" ? "always" : "by default"})` : c.generated ? `generated: ${esc(c.default || "")}` : esc(c.default || "")}</td>
        <td>${c.fk ? `<a class="link" href="#/t/${encodeURIComponent(c.fk.schema)}/${encodeURIComponent(c.fk.table)}">${icon("i-link")}${esc(c.fk.table)}.${esc(c.fk.column)}</a>` : ""}</td></tr>`;
    };
    this.body.innerHTML = `<div class="struct">
      <div class="facts">
        <div><b>Kind</b><span>${kind}</span></div>
        ${m.kind !== "v" ? `<div><b>Rows (estimate)</b><span>${m.estimate >= 0 ? fmtN(m.estimate) : "not analyzed"}</span></div>` : ""}
        ${m.size != null ? `<div><b>Size on disk</b><span>${fmtBytes(m.size)}</span></div>` : ""}
        <div><b>Fields</b><span>${m.columns.length}</span></div>
        <div><b>${m.keyIndex ? "Row key" : "Primary key"}</b><span>${m.pk.length ? esc(m.pk.join(", ")) + (m.keyIndex ? ` <span class="muted">(index ${esc(m.keyIndex)})</span>` : "") : "none"}</span></div>
      </div>
      <section><h3>Fields <span class="n">${m.columns.length}</span></h3><div class="tcard"><table class="t"><thead><tr><th>Name</th><th>Type</th><th>Null</th><th>Default</th><th>References</th></tr></thead><tbody>${m.columns.map(col).join("")}</tbody></table></div></section>
      ${m.indexes.length ? `<section><h3>Indexes <span class="n">${m.indexes.length}</span></h3><div class="tcard"><table class="t"><thead><tr><th>Name</th><th>Definition</th><th>Size</th></tr></thead><tbody>${m.indexes.map((i) => `<tr><td>${esc(i.name)}${i.primary ? ` <span class="badge">primary</span>` : i.unique ? ` <span class="badge">unique</span>` : ""}</td><td class="mono">${esc(i.def)}</td><td style="white-space:nowrap">${fmtBytes(i.size)}</td></tr>`).join("")}</tbody></table></div></section>` : ""}
      ${m.constraints.length ? `<section><h3>Constraints <span class="n">${m.constraints.length}</span></h3><div class="tcard"><table class="t"><thead><tr><th>Name</th><th>Kind</th><th>Definition</th></tr></thead><tbody>${m.constraints.map((c) => `<tr><td>${esc(c.name)}</td><td style="white-space:nowrap">${esc(c.type)}</td><td class="mono">${esc(c.def)}</td></tr>`).join("")}</tbody></table></div></section>` : ""}
      ${m.incoming.length ? `<section><h3>Referenced by <span class="n">${m.incoming.length}</span></h3><div class="tcard"><table class="t"><thead><tr><th>Table</th><th>Field</th><th>Points at</th></tr></thead><tbody>${m.incoming.map((i) => `<tr><td><a class="link" href="#/t/${encodeURIComponent(i.schema)}/${encodeURIComponent(i.table)}">${icon("k-r")}${esc(i.schema === m.schema ? i.table : i.schema + "." + i.table)}</a></td><td class="mono">${esc(i.column)}</td><td class="mono">${esc(i.refColumn)}</td></tr>`).join("")}</tbody></table></div></section>` : ""}
      ${m.viewDef ? `<section><h3>Definition</h3><pre class="code">${highlightSQL(m.viewDef)}</pre></section>` : ""}
    </div>`;
  }

  destroy() { this.dead = true; if (this.grid) this.grid.destroy(); }
}

const metaCache = new Map();
async function cachedMeta(schema, name) {
  const k = S.meta.database + "\0" + schema + "\0" + name;
  if (!metaCache.has(k)) metaCache.set(k, getMeta(schema, name).catch((e) => { metaCache.delete(k); throw e; }));
  return metaCache.get(k);
}

// ---------- full values of cut cells

const CHANGED_MSG = "The record was changed or deleted by someone else since you loaded it; nothing was changed. Refresh to see the current values";

// Loads the full text of cells. items: [{ key, names, hashes }]. With
// hashes ({ name: md5 }), a value whose hash differs, or a record that is
// gone, fails like a conflicting edit: the user never saw that value.
async function fullCells(meta, items) {
  if (!items.length) return [];
  const out = [];
  for (let at = 0; at < items.length; at += 500) {
    const part = items.slice(at, at + 500);
    const cols = [...new Set(part.flatMap((it) => it.names))];
    const res = await api("cells", { schema: meta.schema, name: meta.name, keys: part.map((it) => it.key), cols });
    part.forEach((it, i) => {
      const got = res.rows[i];
      if (!got) throw new ApiError({ error: CHANGED_MSG }, 409);
      const vals = {};
      for (const n of it.names) {
        const j = cols.indexOf(n);
        if (it.hashes && n in it.hashes && got.hashes[j] !== it.hashes[n]) throw new ApiError({ error: CHANGED_MSG }, 409);
        vals[n] = got.values[j];
      }
      out.push(vals);
    });
  }
  return out;
}

// Copies of rows with the full text of their cut cells among names (an
// array, or a function of the row's position; all cut cells when omitted).
// keys default to the rows' own keys. Rows with nothing to load are
// returned as they are.
async function fullRows(meta, rows, { names, keys, check } = {}) {
  const off = meta.keyMode === "ctid" ? 1 : 0;
  const nameAt = (i) => meta.columns[i - off].name;
  const need = [];
  rows.forEach((row, k) => {
    if (!row || !row.cut) return;
    const want = typeof names === "function" ? names(k) : names;
    const idx = Object.keys(row.cut).map(Number).filter((i) => !want || want.includes(nameAt(i)));
    if (idx.length) need.push({ k, idx });
  });
  if (!need.length) return rows;
  const got = await fullCells(meta, need.map(({ k, idx }) => ({
    key: keys ? keys[k] : keyFor(meta, rows[k]),
    names: idx.map(nameAt),
    hashes: check ? Object.fromEntries(idx.map((i) => [nameAt(i), rows[k].cut[i]])) : null,
  })));
  const out = rows.slice();
  need.forEach(({ k, idx }, n) => {
    const row = rows[k], copy = row.slice(), cut = { ...row.cut };
    idx.forEach((i) => { copy[i] = got[n][nameAt(i)]; delete cut[i]; });
    if (Object.keys(cut).length) copy.cut = cut;
    out[k] = copy;
  });
  return out;
}

async function fullRow(meta, row, { names, key, check } = {}) {
  if (!row || !row.cut) return row;
  return (await fullRows(meta, [row], { names, keys: key ? [key] : null, check }))[0];
}

async function openLinked(fk, value) {
  try {
    const meta = await cachedMeta(fk.schema, fk.table);
    const res = await api("rows", { schema: fk.schema, name: fk.table, filters: [{ col: fk.column, op: "eq", val: value }], limit: 1 });
    if (!res.rows.length) { toast(`No ${fk.table} record has ${fk.column} = ${value}`, { err: true }); return; }
    openRecord({ meta, row: res.rows[0], push: !!drawer });
  } catch (e) { toastErr(e); }
}

// ============================================================ record drawer

let drawer = null;

// Resolves true when there is nothing unsaved, or the user agrees to drop it.
async function okToDiscard(body = "Your changes to this record have not been saved.") {
  if (!drawer || !drawer.dirty()) return true;
  return dialog({ title: "Discard unsaved changes?", body, ok: "Discard", cancel: "Keep editing" });
}

// Without force this may wait for the discard dialog; with force it closes at once.
async function closeDrawer(force) {
  if (!drawer) return true;
  if (!force && !(await okToDiscard())) return false;
  if (!drawer) return true;
  drawer.scrim.remove();
  drawer.el.remove();
  document.removeEventListener("keydown", drawer.key, true);
  const v = drawer.stack[0]?.view;
  drawer = null;
  if (v && v.grid) v.grid.el.focus({ preventScroll: true });
  return true;
}

// A grid write replaced the record with key oldKey: keep an open drawer on
// it current. With unsaved edits the drawer keeps what the user saw, so
// saving still detects the conflict.
function drawerRowChanged(view, oldKey, row) {
  if (!drawer) return;
  const top = drawer.stack[drawer.stack.length - 1];
  if (top.view !== view || top.isNew || !top.row) return;
  if (JSON.stringify(keyFor(top.meta, top.row)) !== JSON.stringify(oldKey)) return;
  if (drawer.dirty()) return;
  fullRow(top.meta, row).then((full) => {
    if (drawer && drawer.stack[drawer.stack.length - 1] === top && !drawer.dirty()) { top.row = full; drawer.render(); }
  }, () => {});
}

// The drawer shows and edits whole values, never the grid's cut ones.
async function fillEntry(entry) {
  const key = (row) => entry.view ? entry.view.resolveKey(keyFor(entry.meta, row)) : undefined;
  if (entry.row) entry.row = await fullRow(entry.meta, entry.row, { key: key(entry.row) });
  if (entry.copyOf) entry.copyOf = { ...entry.copyOf, row: await fullRow(entry.meta, entry.copyOf.row, { key: key(entry.copyOf.row) }) };
}

async function openRecord(entry) {
  try { await fillEntry(entry); } catch (e) { toastErr(e); return; }
  if (!drawer) {
    const scrim = html(`<div class="scrim"></div>`);
    const el = html(`<aside class="drawer" role="dialog" aria-modal="true" aria-label="Record"></aside>`);
    drawer = { scrim, el, stack: [] };
    scrim.onclick = () => closeDrawer();
    document.body.append(scrim, el);
    drawer.key = (e) => {
      if (curPop) return;
      if (e.key === "Escape" && !document.querySelector(".dialog")) { e.preventDefault(); if (drawer.stack.length > 1) popRecord(); else closeDrawer(); }
      if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); drawer.save(); }
    };
    document.addEventListener("keydown", drawer.key, true);
    drawer.render = () => renderRecord();
    drawer.dirty = () => (drawer.fields || []).some((f) => f.changed());
    drawer.save = () => saveRecord();
  } else {
    if (!(await okToDiscard())) return;
    if (!entry.push) drawer.stack = [];
  }
  drawer.stack.push(entry);
  renderRecord();
}

async function popRecord() {
  if (!(await okToDiscard())) return;
  drawer.stack.pop();
  renderRecord();
}

function renderRecord() {
  const e = drawer.stack[drawer.stack.length - 1];
  const { meta, isNew } = e;
  const copy = isNew ? e.copyOf : null; // a new record seeded from an existing one
  const blank = isNew && !copy;
  const off = meta.keyMode === "ctid" ? 1 : 0;
  const editable = !!meta.keyMode && !S.meta.readonly;
  const row = copy ? copy.row : e.row || [];
  const lc = labelColumn(meta);
  const name = (lc && row[meta.columns.indexOf(lc) + off]) || (meta.pk.length ? meta.pk.map((p) => row[meta.columns.findIndex((c) => c.name === p) + off]).join(" / ") : "Record");
  const title = blank ? `New ${meta.name} record` : copy ? `Copy of ${name}` : name;
  const canStep = !isNew && e.view && e.r != null;
  drawer.fields = meta.columns.map((c, i) => {
    const kind = kindOf(c);
    const orig = blank ? undefined : row[i + off];
    // On a copy, these get fresh values from the database instead of the original's.
    const selfFill = !!copy && !!(c.identity || c.generated || (c.pk && c.default));
    // A value still cut (should not happen) is shown but never edited.
    const ro = !editable || c.generated === "s" || (c.identity === "a") || kind === "blob" || (blank && (c.identity || c.generated)) || selfFill || isCut(row, i + off);
    const f = { c, kind, orig, val: orig === undefined ? null : orig, touched: false, ro, selfFill, copied: !!copy };
    if (kind === "json" && orig != null) { const p = prettyJSON(orig); if (p != null) { f.pretty = p; f.val = p; } }
    f.changed = () => blank ? f.touched : f.val !== (f.pretty ?? f.orig);
    return f;
  });
  const el = drawer.el;
  el.innerHTML = `
    <div class="dr-head">
      ${drawer.stack.length > 1 ? `<button class="btn icon" data-back title="Back">${icon("i-back")}</button>` : ""}
      <div class="ttl"><small>${esc(meta.schema)}.${esc(meta.name)}</small><b>${esc(title)}</b></div>
      ${canStep ? `<button class="btn icon" data-prev title="Previous record">${icon("i-up")}</button><button class="btn icon" data-next title="Next record">${icon("i-down")}</button>` : ""}
      <button class="btn icon" data-close title="Close (Esc)">${icon("i-x")}</button>
    </div>
    <div class="dr-body">
      ${copy ? `<div class="dr-note">${icon("i-copy", "sm")}<span>Fields start as copies of the original. Change any that must be unique, then create the copy.</span></div>` : ""}
      ${drawer.fields.map((f, i) => fieldHTML(f, i, blank, meta)).join("")}
      ${!isNew && meta.incoming.length ? `<div class="dr-section"><h5>Linked records</h5>${meta.incoming.map((inc, i) => `<button class="linked" data-inc="${i}">${icon("k-r")}<span class="nm">${esc(inc.table)}<small>${esc(inc.column)} = ${esc(row[meta.columns.findIndex((c) => c.name === inc.refColumn) + off] ?? "")}</small></span><span class="n" data-incn="${i}"><span class="spin"></span></span>${icon("i-right", "sm")}</button>`).join("")}</div>` : ""}
    </div>
    ${editable ? `<div class="dr-foot">
      ${!isNew ? `<button class="btn danger" data-delete>${icon("i-trash")}Delete record</button><button class="btn line" data-dup title="Add a copy of this record">${icon("i-copy")}Duplicate</button>` : ""}
      <span class="spacer"></span>
      <span class="muted" data-dirty style="font-size:12px"></span>
      <button class="btn line" data-discard>${isNew ? "Cancel" : "Discard"}</button>
      <button class="btn primary" data-save>${copy ? "Create copy" : isNew ? "Create record" : "Save changes"}</button>
    </div>` : ""}`;
  const body = $(".dr-body", el);
  const paintDirty = () => {
    const n = drawer.fields.filter((f) => f.changed()).length;
    const d = $("[data-dirty]", el);
    if (d) d.textContent = n && !isNew ? `${plural(n, "change")} · ${MOD}+Enter saves` : "";
    const s = $("[data-save]", el);
    if (s && !isNew) s.disabled = !n;
    drawer.fields.forEach((f, i) => $(`[data-f="${i}"]`, el)?.classList.toggle("changed", f.changed()));
  };
  paintDirty();
  body.addEventListener("input", (ev) => {
    const t = ev.target.closest("[data-in]");
    if (!t) return;
    const f = drawer.fields[+t.dataset.in];
    f.val = t.value; f.touched = true;
    const nb = $(`[data-null="${t.dataset.in}"]`, el);
    if (nb) nb.classList.remove("on");
    paintDirty();
  });
  body.addEventListener("click", async (ev) => {
    const seg = ev.target.closest("[data-seg]");
    if (seg) {
      const i = +seg.dataset.seg, f = drawer.fields[i];
      f.val = seg.dataset.v === "\0null" ? null : seg.dataset.v; f.touched = true;
      $$(`[data-seg="${i}"]`, el).forEach((b) => b.classList.toggle("on", b === seg));
      paintDirty();
      return;
    }
    const nb = ev.target.closest("[data-null]");
    if (nb) {
      const i = +nb.dataset.null, f = drawer.fields[i];
      const input = $(`[data-in="${i}"]`, el);
      if (f.val === null) { f.val = input ? input.value : ""; nb.classList.remove("on"); input && input.classList.remove("is-null"); }
      else { f.val = null; nb.classList.add("on"); if (input) { input.value = ""; input.placeholder = "null"; } }
      f.touched = true;
      paintDirty();
      return;
    }
    const open = ev.target.closest("[data-openfk]");
    if (open) {
      const f = drawer.fields[+open.dataset.openfk];
      if (f.val != null && f.val !== "") openLinked(f.c.fk, f.val);
      return;
    }
    const inc = ev.target.closest("[data-inc]");
    if (inc) {
      const x = meta.incoming[+inc.dataset.inc];
      const v = row[meta.columns.findIndex((c) => c.name === x.refColumn) + off];
      if (!(await closeDrawer())) return;
      S.pendingFilter = { schema: x.schema, name: x.table, filter: { col: x.column, op: "eq", val: v } };
      const h = `#/t/${encodeURIComponent(x.schema)}/${encodeURIComponent(x.table)}`;
      // Compared by name: the hash of the open table may also carry a view query.
      if (S.view instanceof TableView && S.view.ref.schema === x.schema && S.view.ref.name === x.table) { S.view.settings.filters = [S.pendingFilter.filter]; S.pendingFilter = null; S.view.applyView(); }
      else location.hash = h;
    }
  });
  $("[data-close]", el).onclick = () => closeDrawer();
  $("[data-back]", el) && ($("[data-back]", el).onclick = popRecord);
  const step = async (d) => {
    if (!(await okToDiscard())) return;
    const at = e.view.locate(e.view.resolveKey(keyFor(meta, e.row)), e.r);
    if (at < 0) return;
    const r = at + d;
    let nr = e.view.rows.get(r);
    if (r < 0 || !nr) return;
    try { nr = await fullRow(meta, nr, { key: e.view.resolveKey(keyFor(meta, nr)) }); } catch (er) { toastErr(er); return; }
    e.r = r; e.row = nr;
    e.view.grid.select(r, e.view.grid.sel ? e.view.grid.sel.c2 : 0);
    renderRecord();
  };
  if (canStep) { $("[data-prev]", el).onclick = () => step(-1); $("[data-next]", el).onclick = () => step(1); }
  const disc = $("[data-discard]", el);
  if (disc) disc.onclick = () => {
    if (copy && drawer.stack.length > 1) { drawer.stack.pop(); renderRecord(); }
    else if (isNew) closeDrawer(true);
    else renderRecord();
  };
  const sv = $("[data-save]", el);
  if (sv) sv.onclick = saveRecord;
  const del = $("[data-delete]", el);
  if (del) del.onclick = async () => {
    if (!(await dialog({ title: "Delete this record?", body: `It is removed from ${meta.schema}.${meta.name} right away. This cannot be undone.`, ok: "Delete record" }))) return;
    try {
      await api("delete", { schema: meta.schema, name: meta.name, keys: [e.view ? e.view.resolveKey(keyFor(meta, row)) : keyFor(meta, row)] });
      toast("Deleted 1 record");
      const v = e.view;
      closeDrawer(true);
      if (v && v.grid) v.refresh(true);
    } catch (er) { toastErr(er); }
  };
  const dup = $("[data-dup]", el);
  if (dup) dup.onclick = () => openRecord({ meta, view: e.view, isNew: true, push: true, copyOf: { key: keyFor(meta, row), row } });
  // counts for linked records
  if (!isNew) meta.incoming.forEach(async (x, i) => {
    const v = row[meta.columns.findIndex((c) => c.name === x.refColumn) + off];
    try {
      const r = await api("count", { schema: x.schema, name: x.table, filters: [{ col: x.column, op: "eq", val: v }] });
      const n = $(`[data-incn="${i}"]`, el);
      if (n) n.textContent = (r.exact ? "" : "≈ ") + fmtN(r.count);
    } catch { const n = $(`[data-incn="${i}"]`, el); if (n) n.textContent = ""; }
  });
  setTimeout(() => {
    if (!drawer) return;
    // autosize textareas
    $$("textarea[data-in]", el).forEach((t) => { t.style.height = Math.min(320, Math.max(84, t.scrollHeight + 2)) + "px"; });
    if (isNew) $("[data-in]", el)?.focus();
  });
}

function keyFor(meta, row) {
  if (meta.keyMode === "ctid") return { __ctid: row[0] };
  const k = {};
  meta.pk.forEach((p) => { k[p] = row[meta.columns.findIndex((c) => c.name === p)]; });
  return k;
}

function fieldHTML(f, i, isNew, meta) {
  const c = f.c;
  const tags = [c.pk && (meta.keyIndex ? "unique key" : "primary key"), !c.nullable && !c.pk && "required", c.identity && "identity", c.generated && "generated"].filter(Boolean);
  const nullBtn = c.nullable && !f.ro ? `<button class="nullbtn ${f.val === null && !isNew ? "on" : ""}" data-null="${i}" title="Set to null">null</button>` : "";
  const label = `<div class="dr-label">${icon(KIND_ICON[f.kind], "sm")}<span class="name">${esc(c.name)}</span><span class="ty">${esc(c.type)}</span>${tags.map((t) => `<span class="tag">${t}</span>`).join("")}<span class="grow"></span>${nullBtn}</div>`;
  const help = c.comment ? `<div class="help">${esc(c.comment)}</div>` : "";
  const defHelp = isNew && c.default ? `<div class="help">Leave untouched to use the default: <span class="mono">${esc(c.default)}</span></div>` : "";
  let ctl;
  const v = f.val;
  if (f.ro) {
    ctl = isNew || f.selfFill ? `<div class="ro-val muted">Set by the database${c.default ? `: <span class="mono">${esc(c.default)}</span>` : ""}</div>`
      : `<div class="ro-val">${renderValue({ kind: f.kind === "link" ? "text" : f.kind, enum: c.enum }, v, true)}${f.copied && f.kind === "blob" && v != null ? ` <span class="muted">copied from the original</span>` : ""}</div>`;
  } else if (f.kind === "bool") {
    const opts = [["true", "true"], ["false", "false"], ...(c.nullable ? [["\0null", "null"]] : [])];
    ctl = `<div class="seg">${opts.map(([val, lbl]) => `<button data-seg="${i}" data-v="${val}" class="${(isNew ? false : (val === "\0null" ? v === null : v !== null && isTrue(v) === (val === "true"))) ? "on" : ""}">${lbl}</button>`).join("")}</div>`;
  } else if (f.kind === "enum" && c.enum) {
    const opts = [...c.enum, ...(c.nullable ? ["\0null"] : [])];
    ctl = `<div class="seg">${opts.map((o) => `<button data-seg="${i}" data-v="${esc(o)}" class="${!isNew && (o === "\0null" ? v === null : v === o) ? "on" : ""}">${o === "\0null" ? "null" : `<span class="pill" style="--h:${(c.enum.indexOf(o) * 67 + 205) % 360}">${esc(o)}</span>`}</button>`).join("")}</div>`;
  } else if (f.kind === "link") {
    ctl = `<div style="display:flex;gap:6px"><input class="input mono" data-in="${i}" value="${esc(v ?? "")}" placeholder="${v === null && !isNew ? "null" : esc(c.fk.table + "." + c.fk.column)}" spellcheck="false"><button class="btn line" data-openfk="${i}" title="Open the linked ${esc(c.fk.table)} record">${icon("i-link")}Open</button></div>`;
  } else if (f.kind === "json" || (f.kind === "text" && v && (v.length > 70 || v.includes("\n")))) {
    ctl = `<textarea class="input ${f.kind === "json" ? "mono" : ""}" data-in="${i}" spellcheck="${f.kind === "text"}" placeholder="${v === null && !isNew ? "null" : ""}">${esc(v ?? "")}</textarea>`;
  } else {
    const ph = v === null && !isNew ? "null" : f.kind === "datetime" ? "2025-01-31 14:30:00+00" : f.kind === "date" ? "2025-01-31" : f.kind === "array" ? "{one,two}" : "";
    ctl = `<input class="input ${["num", "uuid", "array", "range", "net", "other", "time"].includes(f.kind) ? "mono" : ""}" data-in="${i}" value="${esc(v ?? "")}" placeholder="${esc(ph)}" spellcheck="false" ${f.kind === "num" ? 'inputmode="decimal"' : ""}>`;
  }
  return `<div class="dr-field" data-f="${i}">${label}${ctl}${defHelp}${help}</div>`;
}

async function saveRecord() {
  if (!drawer) return;
  const e = drawer.stack[drawer.stack.length - 1];
  const { meta, isNew } = e;
  const values = {}, expect = {};
  for (const f of drawer.fields) {
    if (!f.changed()) continue;
    let v = f.val;
    if (v === "" && !["text", "json"].includes(f.kind) && f.c.nullable) v = null;
    values[f.c.name] = v;
    if (!isNew) expect[f.c.name] = f.orig;
  }
  if (!isNew && !Object.keys(values).length) return;
  const btn = $("[data-save]", drawer.el);
  if (btn) { btn.disabled = true; btn.innerHTML = `<span class="spin"></span>Saving`; }
  try {
    if (isNew) {
      // A copy sends only the fields you changed; the server copies the rest, binary data included.
      const res = e.copyOf
        ? { row: (await api("duplicate", { schema: meta.schema, name: meta.name, keys: [e.view ? e.view.resolveKey(e.copyOf.key) : e.copyOf.key], values })).rows[0] }
        : await api("insert", { schema: meta.schema, name: meta.name, values });
      toast(e.copyOf ? "Copy created" : "Record created");
      if (e.view && e.view.grid) e.view.pushUndo({ created: [keyFor(meta, res.row)], label: e.copyOf ? "duplicate" : "add" });
      e.isNew = false; e.copyOf = null; e.row = await fullRow(meta, res.row).catch(() => null); e.r = null;
      if (!e.row) { closeDrawer(true); return; }
      if (e.view && e.view.grid) e.view.refresh(true);
      renderRecord();
    } else {
      const v = e.view;
      const send = () => {
        const key = v ? v.resolveKey(keyFor(meta, e.row)) : keyFor(meta, e.row);
        return api("update", { schema: meta.schema, name: meta.name, key, values, expect }).then((res) => ({ key, res }));
      };
      // Through the view's write queue, so it is ordered with grid edits.
      const { key, res } = v ? await v.serial(send) : await send();
      toast("Changes saved");
      e.row = res.row;
      if (v) {
        v.noteRename(key, keyFor(meta, res.row));
        const r = v.locate(key, e.r);
        if (r >= 0) {
          v.rows.set(r, res.row);
          e.r = r;
          if (v.grid) { Object.keys(values).forEach((n) => v.grid.flash(r, n)); v.grid.draw(true); }
        }
      }
      // The saved row comes back with long values cut; the drawer needs them whole.
      try { e.row = await fullRow(meta, res.row); } catch { closeDrawer(true); return; }
      if (!drawer || drawer.stack[drawer.stack.length - 1] !== e) return;
      renderRecord();
    }
  } catch (er) {
    toastErr(er);
    if (btn) { btn.disabled = false; btn.textContent = e.copyOf ? "Create copy" : isNew ? "Create record" : "Save changes"; }
  }
}

// ============================================================ SQL editor

const SQL_KW = new Set(("select from where and or not in is null like ilike between as on join left right inner outer full cross natural using group by order having limit offset fetch first next rows only " +
  "insert into values update set delete returning create table view index unique primary key foreign references check default constraint alter add drop column rename to cascade restrict if exists " +
  "begin commit rollback transaction savepoint release with recursive union all intersect except distinct case when then else end asc desc nulls last true false " +
  "grant revoke schema database function returns language replace trigger before after for each row execute procedure sequence type enum materialized refresh " +
  "explain analyze verbose vacuum truncate copy lateral over partition window filter within any some array cast collate do nothing conflict excluded serial bigserial " +
  "integer int bigint smallint text varchar char boolean numeric decimal real double precision date time timestamp timestamptz interval json jsonb uuid bytea zone").split(" "));

function highlightSQL(src) {
  const re = /(--[^\n]*|\/\*[\s\S]*?(?:\*\/|$))|('(?:[^']|'')*'?)|("(?:[^"]|"")*"?)|(\$\d+|\b\d+(?:\.\d+)?(?:e[+-]?\d+)?\b)|([A-Za-z_][A-Za-z0-9_$]*)(?=(\s*\())?/g;
  let out = "", last = 0, m;
  while ((m = re.exec(src))) {
    out += esc(src.slice(last, m.index));
    const t = esc(m[0]);
    if (m[1]) out += `<span class="tk-c">${t}</span>`;
    else if (m[2]) out += `<span class="tk-s">${t}</span>`;
    else if (m[3]) out += `<span class="tk-q">${t}</span>`;
    else if (m[4]) out += `<span class="tk-n">${t}</span>`;
    else if (SQL_KW.has(m[5].toLowerCase())) out += `<span class="tk-k">${t}</span>`;
    else if (m[6] !== undefined) out += `<span class="tk-f">${t}</span>`;
    else out += t;
    last = re.lastIndex;
    if (m[0] === "") re.lastIndex++;
  }
  return out + esc(src.slice(last));
}

class SqlView {
  constructor(main) {
    this.main = main;
    this.ref = null;
    document.title = `SQL · ${S.meta.database} · airadminer`;
    main.innerHTML = topbar(`<div class="crumb"><span class="sch">${esc(S.meta.database)} /</span><h2>SQL editor</h2></div><span class="spacer"></span>
      <button class="btn" data-hist>${icon("i-history")}<span class="lbl-txt">History</span></button>`) + `
      <div class="sql">
        <div class="sql-ed"><div class="gut" aria-hidden="true"></div><pre aria-hidden="true"></pre><textarea spellcheck="false" wrap="off" aria-label="SQL" placeholder="SELECT * FROM …"></textarea></div>
        <div class="sql-split" title="Drag to resize"></div>
        <div class="sql-bar">
          <button class="btn primary" data-run>${icon("i-play", "sm")}Run<span class="kbd" style="border-color:transparent;color:inherit;opacity:.75">${MOD}↵</span></button>
          <button class="btn" data-stop hidden>${icon("i-stop", "sm")}Cancel</button>
          <div class="rtabs tabs" data-rtabs hidden></div>
          <span class="stat" data-stat>Runs the selection, or everything when nothing is selected.</span>
          <span class="spacer"></span>
          <button class="btn" data-view hidden title="Row height, density and fit to screen">${icon("i-columns")}<span class="lbl-txt">View</span></button>
          <button class="btn" data-csv hidden>${icon("i-download")}<span class="lbl-txt">CSV</span></button>
        </div>
        <div class="sql-out" data-out></div>
      </div>`;
    bindTopbar(main);
    this.ta = $("textarea", main);
    this.pre = $(".sql-ed pre", main);
    this.gut = $(".gut", main);
    const ed = $(".sql-ed", main);
    ed.style.setProperty("--edh", store.get("aa.sql.h", 38) + "%");
    this.ta.value = store.get("aa.sql." + S.meta.database, "");
    this.paint();
    this.ta.addEventListener("input", () => { this.paint(); this.saveText(); });
    this.ta.addEventListener("scroll", () => { this.pre.scrollTop = this.ta.scrollTop; this.pre.scrollLeft = this.ta.scrollLeft; this.gut.scrollTop = this.ta.scrollTop; });
    this.ta.addEventListener("keydown", (e) => this.key(e));
    $("[data-run]", main).onclick = () => this.run();
    $("[data-stop]", main).onclick = () => this.ctl && this.ctl.abort();
    $("[data-hist]", main).onclick = (e) => this.history(e.currentTarget);
    $("[data-csv]", main).onclick = () => this.csv();
    $("[data-view]", main).onclick = (e) => viewMenu(e.currentTarget, this);
    this.splitter($(".sql-split", main), ed);
    this.ta.focus();
    this.saveText = debounce(() => store.set("aa.sql." + S.meta.database, this.ta.value), 300);
  }

  // Hooks for viewMenu() and the fit shortcut; kept for all result grids.
  get rowHeight() { return store.get("aa.sqlview.rh", 0); }
  setRowHeight(i) { store.set("aa.sqlview.rh", i); this.grid && this.grid.setRowHeight(i); }
  get fit() { return store.get("aa.sqlview.fit", false); }
  toggleFit() {
    const on = !this.fit;
    store.set("aa.sqlview.fit", on);
    this.grid && this.grid.setFit(on);
    $("[data-view]", this.main).classList.toggle("on", on);
  }

  paint() {
    const v = this.ta.value;
    this.pre.innerHTML = highlightSQL(v) + "\n";
    const lines = v.split("\n").length;
    if (lines !== this.lines) { this.lines = lines; this.gut.innerHTML = Array.from({ length: lines }, (_, i) => `<div>${i + 1}</div>`).join(""); }
  }

  key(e) {
    const ta = this.ta;
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); this.run(); return; }
    if (e.key === "Tab" && !e.metaKey && !e.ctrlKey) {
      e.preventDefault();
      const { selectionStart: a, selectionEnd: b, value } = ta;
      if (a === b && !e.shiftKey) { ta.setRangeText("  ", a, b, "end"); }
      else {
        const ls = value.lastIndexOf("\n", a - 1) + 1;
        const block = value.slice(ls, b);
        const nb = e.shiftKey ? block.replace(/^ {1,2}/gm, "") : block.replace(/^/gm, "  ");
        ta.setRangeText(nb, ls, b, "select");
      }
      this.paint(); this.saveText();
    }
    if ((e.metaKey || e.ctrlKey) && e.key === "/") {
      e.preventDefault();
      const { selectionStart: a, selectionEnd: b, value } = ta;
      const ls = value.lastIndexOf("\n", a - 1) + 1;
      const block = value.slice(ls, b);
      const all = block.split("\n").every((l) => /^\s*--/.test(l) || !l.trim());
      ta.setRangeText(all ? block.replace(/^(\s*)-- ?/gm, "$1") : block.replace(/^/gm, "-- "), ls, b, "select");
      this.paint(); this.saveText();
    }
  }

  splitter(bar, ed) {
    bar.addEventListener("mousedown", (e) => {
      e.preventDefault();
      const box = $(".sql", this.main).getBoundingClientRect();
      const mv = (ev) => {
        const pct = Math.max(12, Math.min(80, ((ev.clientY - box.top) / box.height) * 100));
        ed.style.setProperty("--edh", pct + "%");
        store.set("aa.sql.h", Math.round(pct));
      };
      const up = () => { removeEventListener("mousemove", mv); removeEventListener("mouseup", up); this.grid && this.grid.draw(true); };
      addEventListener("mousemove", mv); addEventListener("mouseup", up);
    });
  }

  async run() {
    if (this.ctl) return;
    const ta = this.ta;
    const hasSel = ta.selectionEnd > ta.selectionStart;
    const sql = hasSel ? ta.value.slice(ta.selectionStart, ta.selectionEnd) : ta.value;
    const base = hasSel ? ta.selectionStart : 0;
    if (!sql.trim()) { toast("Write a query first"); return; }
    this.ctl = new AbortController();
    const runBtn = $("[data-run]", this.main), stop = $("[data-stop]", this.main);
    runBtn.disabled = true; stop.hidden = false;
    $("[data-stat]", this.main).innerHTML = `<span style="display:inline-flex;gap:8px;align-items:center"><span class="spin"></span>Running…</span>`;
    const t0 = performance.now();
    let res;
    try {
      res = await api("query", { sql }, { signal: this.ctl.signal });
    } catch (e) {
      res = { results: [], error: e.name === "AbortError" ? { error: "Query canceled" } : e, ms: Math.round(performance.now() - t0) };
    } finally {
      this.ctl = null; runBtn.disabled = false; stop.hidden = true;
    }
    this.res = res; this.base = base; this.sql = sql;
    const hist = store.get("aa.sql.hist", []);
    const entry = { sql: sql.trim(), at: Date.now(), ms: res.ms, ok: !res.error, db: S.meta.database };
    store.set("aa.sql.hist", [entry, ...hist.filter((h) => h.sql !== entry.sql)].slice(0, 60));
    if (res.schemaChanged) { metaCache.clear(); loadTables().then(paintTables).catch(() => {}); }
    const withRows = res.results.findLastIndex ? res.results.findLastIndex((r) => r.columns) : -1;
    this.show(withRows >= 0 ? withRows : res.results.length - 1);
  }

  show(i) {
    const res = this.res, out = $("[data-out]", this.main);
    const tabs = $("[data-rtabs]", this.main);
    const stat = $("[data-stat]", this.main);
    const csv = $("[data-csv]", this.main);
    if (this.grid) { this.grid.destroy(); this.grid = null; }
    const shown = res.results.map((r, k) => ({ r, k })).filter(({ r }) => r.columns);
    tabs.hidden = shown.length < 2;
    tabs.innerHTML = shown.map(({ k }, n) => `<a href="#" data-k="${k}" class="${k === i ? "on" : ""}">Result ${n + 1}</a>`).join("");
    tabs.onclick = (e) => { const a = e.target.closest("[data-k]"); if (a) { e.preventDefault(); this.show(+a.dataset.k); } };
    const cur = res.results[i];
    this.cur = cur && cur.columns ? cur : null;
    const tags = res.results.filter((r) => !r.columns).map((r) => r.tag).filter(Boolean);
    let errHTML = "";
    if (res.error) {
      const e = res.error;
      let where = "";
      if (e.position) {
        const pos = this.base + e.position - 1;
        const v = this.ta.value;
        const ls = v.lastIndexOf("\n", pos - 1) + 1;
        let le = v.indexOf("\n", pos); if (le < 0) le = v.length;
        const lineNo = v.slice(0, ls).split("\n").length;
        where = `<pre>${esc(`LINE ${lineNo}: ` + v.slice(ls, le))}\n${" ".repeat(`LINE ${lineNo}: `.length + pos - ls)}^</pre>`;
        this.ta.focus();
        this.ta.setSelectionRange(pos, pos);
      }
      errHTML = `<div class="sql-msg"><div class="sql-err" role="alert"><b>${esc(e.error || e.message)}</b>${e.detail ? `<p>${esc(e.detail)}</p>` : ""}${e.hint ? `<p>Hint: ${esc(e.hint)}</p>` : ""}${e.code ? `<p class="muted">SQLSTATE ${esc(e.code)}</p>` : ""}${res.results.some((r) => !r.columns) && !/\b(begin|commit|start\s+transaction)\b/i.test(this.sql) ? `<p class="muted">Statements sent together run as one transaction, so the changes made before this error were rolled back.</p>` : ""}${where}</div></div>`;
    }
    csv.hidden = !cur || !cur.columns;
    $("[data-view]", this.main).hidden = csv.hidden;
    $("[data-view]", this.main).classList.toggle("on", this.fit);
    if (cur && cur.columns) {
      const count = cur.truncated ? `${plural(cur.total, "row")}, showing the first ${fmtN(cur.rows.length)}` : plural(cur.rows.length, "row");
      stat.textContent = `${count}  ·  ${res.ms} ms${tags.length ? "  ·  " + tags.join(", ") : ""}`;
      out.innerHTML = errHTML + `<div class="grid-wrap" data-gw></div>`;
      const cols = cur.columns.map((c, k) => {
        const kind = kindFromTypeName(c.type);
        const col = { name: c.name, label: c.name, kind, idx: k, type: c.type, readonly: true };
        col.width = columnWidth(col, cur.rows.slice(0, PAGE));
        return col;
      });
      const src = { count: () => cur.rows.length, get: (r) => cur.rows[r], ensure() {} };
      this.grid = new Grid($("[data-gw]", out), { source: src, label: "Query result", rh: this.rowHeight, fit: this.fit, status: () => `<span><b>${plural(cur.rows.length, "row")}</b></span><span class="muted">Enter shows the full value, ${MOD}+C copies</span>` });
      this.grid.setCols(cols);
    } else if (!res.error || res.results.length) {
      stat.textContent = `${res.ms} ms`;
      out.innerHTML = errHTML + `<div class="sql-msg"><div class="sql-ok">${icon("i-check")}${esc(tags.join(", ") || "Done")}</div></div>`;
    } else {
      stat.textContent = `Failed after ${res.ms} ms`;
      out.innerHTML = errHTML;
    }
  }

  csv() {
    const cur = this.cur;
    if (!cur) return;
    download("query.csv", new Blob([toCSV(cur.columns.map((c) => c.name), cur.rows)], { type: "text/csv" }));
  }

  history(anchor) {
    const hist = store.get("aa.sql.hist", []).filter((h) => !h.db || h.db === S.meta.database);
    const ago = (t) => { const s = (Date.now() - t) / 1000; return s < 60 ? "just now" : s < 3600 ? Math.floor(s / 60) + " min ago" : s < 86400 ? Math.floor(s / 3600) + " h ago" : new Date(t).toLocaleDateString(); };
    const el = html(`<div class="hist" style="width:min(520px, calc(100vw - 24px))"><h4>Recent queries on ${esc(S.meta.database)}</h4><div class="scroll">${hist.length ? hist.map((h, i) => `<button class="mi" data-i="${i}"><code>${esc(h.sql)}</code><small>${ago(h.at)}  ·  ${h.ms} ms${h.ok ? "" : "  ·  failed"}</small></button>`).join("") : `<div class="muted" style="padding:6px 8px 10px">Queries you run show up here.</div>`}</div></div>`);
    openPop(anchor, el, { align: "right" });
    el.addEventListener("click", (e) => {
      const b = e.target.closest("[data-i]");
      if (!b) return;
      closePop();
      this.ta.value = hist[+b.dataset.i].sql;
      this.paint(); this.saveText(); this.ta.focus();
    });
  }

  destroy() { if (this.ctl) this.ctl.abort(); if (this.grid) this.grid.destroy(); }
}

// ============================================================ command palette

function fuzzy(q, s) {
  if (!q) return { score: 0, marks: [] };
  const lq = q.toLowerCase(), ls = s.toLowerCase();
  const at = ls.indexOf(lq);
  if (at >= 0) return { score: 100 - at - s.length * 0.1 + (at === 0 ? 50 : 0), marks: Array.from({ length: lq.length }, (_, i) => at + i) };
  const marks = []; let j = 0;
  for (let i = 0; i < ls.length && j < lq.length; i++) if (ls[i] === lq[j]) { marks.push(i); j++; }
  if (j < lq.length) return null;
  return { score: 20 - (marks[marks.length - 1] - marks[0]) - s.length * 0.1, marks };
}
const markText = (s, marks) => { const set = new Set(marks); return [...s].map((ch, i) => set.has(i) ? `<mark>${esc(ch)}</mark>` : esc(ch)).join(""); };

function palette() {
  if (!S.meta || $(".palette")) return;
  closePop();
  const scrim = html(`<div class="scrim" style="z-index:59;background:rgba(12,16,24,.25)"></div>`);
  const el = html(`<div class="pop palette" role="dialog" aria-label="Go to"><input class="input" placeholder="Jump to a table or run a command" spellcheck="false"><div class="scroll"></div></div>`);
  document.body.append(scrim, el);
  const input = $("input", el), list = $(".scroll", el);
  const actions = [
    ...(S.meta.sql !== false ? [{ label: "Open SQL editor", icon: "i-terminal", run: () => (location.hash = "#/sql") }] : []),
    ...THEME_ORDER.filter((t) => t !== themePref()).map((t) => ({ label: t === "system" ? "Theme: follow the system" : `Theme: ${t}`, icon: THEMES[t].icon, run: () => setTheme(t) })),
    { label: sideShown() ? "Hide sidebar" : "Show sidebar", icon: "i-sidebar", run: () => toggleSide() },
    { label: S.density === "compact" ? "Density: comfortable" : "Density: compact", icon: "i-grid", run: () => setDensity(S.density === "compact" ? "comfortable" : "compact") },
    ...(S.view && S.view.toggleFit && S.view.grid ? [{ label: S.view.fit ? "Show columns at saved widths" : "Fit columns to screen", icon: "i-expand", run: () => S.view.toggleFit() }] : []),
    { label: "Reload table list", icon: "i-refresh", run: async () => { await loadTables(); paintTables(); toast("Table list reloaded"); } },
    ...S.meta.databases.filter((d) => d !== S.meta.database).map((d) => ({ label: `Switch to database ${d}`, icon: "i-db", run: () => switchDB(d) })),
    ...(S.meta.fixed ? [] : [{ label: "Sign out", icon: "i-logout", run: signOut }]),
  ];
  let items = [], hi = 0;
  const close = () => { scrim.remove(); el.remove(); };
  const paint = () => {
    const q = input.value.trim();
    const tables = S.tables.map((t) => {
      const full = t.schema === "public" ? t.name : `${t.schema}.${t.name}`;
      const pre = full.length - t.name.length;
      let m = fuzzy(q, t.name);
      if (m) m = { score: m.score + 5, marks: m.marks.map((i) => i + pre) };
      else m = fuzzy(q, full);
      return m && { label: full, marks: m.marks, score: m.score, icon: "k-" + t.kind, hint: compact(t.estimate), run: () => (location.hash = `#/t/${encodeURIComponent(t.schema)}/${encodeURIComponent(t.name)}`) };
    }).filter(Boolean).sort((a, b) => b.score - a.score).slice(0, 40);
    const acts = actions.map((a) => { const m = fuzzy(q, a.label); return m && { ...a, marks: m.marks, score: m.score }; }).filter(Boolean).sort((a, b) => b.score - a.score);
    items = q ? [...tables, ...acts] : [...tables.slice(0, 12), ...acts];
    hi = Math.min(hi, items.length - 1);
    let h = "", k = 0;
    if (tables.length) { h += `<div class="grp">Tables</div>`; for (const t of (q ? tables : tables.slice(0, 12))) h += row(t, k++); }
    if (acts.length) { h += `<div class="grp">Commands</div>`; for (const a of acts) h += row(a, k++); }
    list.innerHTML = h || `<div class="muted" style="padding:12px">Nothing matches “${esc(q)}”.</div>`;
    $(".mi.hi", list)?.scrollIntoView({ block: "nearest" });
  };
  const row = (it, k) => `<button class="mi ${k === hi ? "hi" : ""}" data-k="${k}">${icon(it.icon)}<span>${markText(it.label, it.marks || [])}</span>${it.hint ? `<span class="r">${it.hint}</span>` : ""}</button>`;
  paint();
  input.focus();
  input.addEventListener("input", () => { hi = 0; paint(); });
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); hi = Math.min(items.length - 1, hi + 1); paint(); }
    else if (e.key === "ArrowUp") { e.preventDefault(); hi = Math.max(0, hi - 1); paint(); }
    else if (e.key === "Enter") { e.preventDefault(); const it = items[hi]; close(); it && it.run(); }
    else if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); close(); }
  });
  list.addEventListener("click", (e) => { const b = e.target.closest("[data-k]"); if (b) { const it = items[+b.dataset.k]; close(); it.run(); } });
  scrim.onclick = close;
}

addEventListener("keydown", (e) => {
  const mod = e.metaKey || e.ctrlKey, k = e.key.toLowerCase();
  if (mod && !e.altKey && !e.shiftKey && k === "k") { e.preventDefault(); palette(); }
  else if (mod && !e.altKey && !e.shiftKey && k === "b" && $(".shell")) { e.preventDefault(); toggleSide(); }
  else if (mod && !e.altKey && e.shiftKey && k === "f" && S.view && S.view.grid) { e.preventDefault(); toggleFit(); }
});

// Browser tests (e2e/grid.mjs) set window.__AA_E2E before this script runs
// to reach internals; a normal page never has it.
if (E2E) E2E.app = { S, api, prettyJSON, pastedMatrix, saveRecord, closeDrawer, toggleSide, setDensity, fitWidths, get drawer() { return drawer; }, get lastCopy() { return lastCopy; } };

boot();
})();
