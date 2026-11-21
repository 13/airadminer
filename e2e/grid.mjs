// End-to-end tests of the grid's write paths in headless Chrome, driven over
// the DevTools protocol (no npm packages needed).
//
//   make e2e      # builds, starts airadminer with -dsn, runs this file
//
// Environment: AIRADMINER_URL (server), CHROME (browser binary).
// Every test works in its own schema and drops it afterwards.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const BASE = process.env.AIRADMINER_URL || "http://127.0.0.1:18090/";
const CHROME = process.env.CHROME || "google-chrome";
const PORT = 9300 + Math.floor(Math.random() * 500);

const profile = mkdtempSync(join(tmpdir(), "aa-e2e-"));
const chrome = spawn(CHROME, ["--headless=new", `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, "--no-first-run", "--no-default-browser-check", "--disable-gpu", "about:blank"], { stdio: "ignore" });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let ws, seq = 0;
const waiting = new Map();
const pageErrors = [];

async function connect() {
  for (let i = 0; i < 100; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
      const page = list.find((t) => t.type === "page");
      if (page) {
        ws = new WebSocket(page.webSocketDebuggerUrl);
        await new Promise((ok, bad) => { ws.onopen = ok; ws.onerror = bad; });
        ws.onmessage = (m) => {
          const msg = JSON.parse(m.data);
          if (msg.id && waiting.has(msg.id)) { waiting.get(msg.id)(msg); waiting.delete(msg.id); }
          if (msg.method === "Runtime.exceptionThrown") pageErrors.push(msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text);
        };
        return;
      }
    } catch {}
    await sleep(100);
  }
  throw new Error("Chrome did not start");
}

function send(method, params = {}) {
  const id = ++seq;
  ws.send(JSON.stringify({ id, method, params }));
  return new Promise((ok) => waiting.set(id, ok));
}

// Runs an async function body in the page and returns its JSON result.
async function run(body) {
  const res = await send("Runtime.evaluate", { expression: `(async () => { ${body} })()`, awaitPromise: true, returnByValue: true });
  if (res.result.exceptionDetails) throw new Error(res.result.exceptionDetails.exception?.description || res.result.exceptionDetails.text);
  return res.result.result.value;
}

const helpers = `
  const A = __AA_E2E.app;
  window.S = A.S; window.api = A.api; window.prettyJSON = A.prettyJSON; window.pastedMatrix = A.pastedMatrix;
  window.saveRecord = A.saveRecord; window.closeDrawer = A.closeDrawer;
  Object.defineProperty(window, "drawer", { get: () => A.drawer });
  Object.defineProperty(window, "lastCopy", { get: () => A.lastCopy });
  window.sql = async (q) => { const r = await api("query", { sql: q }); if (r.error) throw new Error(r.error.error); return r.results.at(-1)?.rows || []; };
  window.until = async (fn, what) => { for (let i = 0; i < 200; i++) { try { if (await fn()) return; } catch {} await new Promise((r) => setTimeout(r, 25)); } throw new Error("timed out waiting for " + what); };
  window.open_ = async (schema, name) => {
    location.hash = "#/t/" + schema + "/" + name;
    await until(() => S.view && S.view.meta && S.view.meta.name === name && S.view.rows.ready, "table " + name);
  };
  window.col = (n) => S.view.grid.cols.findIndex((c) => c.name === n);
  __AA_E2E.confirm = true;   // confirm every dialog
  return true;
`;

const tests = {
  async "paste reaches rows that were not loaded yet"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, v text); INSERT INTO ${sc}.items SELECT g, 'orig' FROM generate_series(1, 1000) g");
      await open_("${sc}", "items");
      const c = col("v");
      await S.view.paste({ r0: 0, r1: 0, c0: c, c1: c }, Array.from({ length: 700 }, (_, i) => ["p" + i]));
      const [[n]] = await sql("SELECT count(*) FROM ${sc}.items WHERE v = 'p' || (id - 1)");
      if (n !== "700") throw new Error("pasted rows saved: " + n + " of 700");
    `);
  },

  async "an edit lands on its record after the rows moved"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, v text); INSERT INTO ${sc}.items SELECT g, 'orig' FROM generate_series(1, 50) g");
      await open_("${sc}", "items");
      const row = S.view.rows.get(5);                  // id 6, as the editor saw it
      S.view.settings.sort = [{ col: "id", desc: true }];
      S.view.refresh(true);
      await until(() => S.view.rows.get(5) && S.view.rows.get(5)[0] === "45", "resorted rows");
      if (!(await S.view.writeCell(5, "v", "moved", row))) throw new Error("write failed");
      const rows = await sql("SELECT id FROM ${sc}.items WHERE v = 'moved'");
      if (rows.length !== 1 || rows[0][0] !== "6") throw new Error("edited ids: " + JSON.stringify(rows));
      const shown = S.view.rows.get(S.view.locate({ id: "6" }));
      if (shown[1] !== "moved") throw new Error("grid shows " + shown[1] + " for id 6");
      if (S.view.rows.get(5)[1] !== "orig") throw new Error("the row now at position 5 was repainted");
    `);
  },

  async "quick edits to one row of a table without a primary key all save"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.nopk (v text, w text); INSERT INTO ${sc}.nopk VALUES ('a0', 'b0')");
      await open_("${sc}", "nopk");
      const row = S.view.rows.get(0);
      const ok = await Promise.all([S.view.writeCell(0, "v", "a1", row), S.view.writeCell(0, "w", "b1", row)]);
      if (!ok.every(Boolean)) throw new Error("a write failed: " + document.querySelector(".toast")?.textContent);
      const ok2 = await S.view.writeCell(0, "v", "a2");  // uses the key the last write returned
      const [[v, w]] = await sql("SELECT v, w FROM ${sc}.nopk");
      if (!ok2 || v !== "a2" || w !== "b1") throw new Error("database has " + v + ", " + w);
      await S.view.undoLast();
      const [[v2]] = await sql("SELECT v FROM ${sc}.nopk");
      if (v2 !== "a1") throw new Error("undo on a ctid table: " + v2);
    `);
  },

  async "an edit based on a stale value does not overwrite someone else's change"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, v text); INSERT INTO ${sc}.items VALUES (1, 'orig')");
      await open_("${sc}", "items");
      const row = S.view.rows.get(0);
      await sql("UPDATE ${sc}.items SET v = 'theirs'");
      if (await S.view.writeCell(0, "v", "mine", row)) throw new Error("stale write succeeded");
      const [[v]] = await sql("SELECT v FROM ${sc}.items");
      if (v !== "theirs") throw new Error("database has " + v);
      if (S.view.rows.get(0)[1] !== "orig") throw new Error("failed write left an optimistic value");
    `);
  },

  async "undo does not revert a later change by someone else"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, v text); INSERT INTO ${sc}.items VALUES (1, 'orig')");
      await open_("${sc}", "items");
      await S.view.writeCell(0, "v", "mine");
      await sql("UPDATE ${sc}.items SET v = 'theirs'");
      await S.view.undoLast();
      const [[v]] = await sql("SELECT v FROM ${sc}.items");
      if (v !== "theirs") throw new Error("undo overwrote: " + v);
    `);
  },

  async "copy and paste inside the grid keeps NULL"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, t text); INSERT INTO ${sc}.items VALUES (1, NULL), (2, 'x'), (3, 'a'), (4, 'b')");
      await open_("${sc}", "items");
      const c = col("t");
      S.view.grid.sel = { r: 0, c, r2: 1, c2: c };
      await S.view.grid.copy();
      await S.view.paste({ r0: 2, r1: 2, c0: c, c1: c }, pastedMatrix(lastCopy.text));
      const rows = await sql("SELECT id, t IS NULL, t FROM ${sc}.items WHERE id IN (3, 4) ORDER BY id");
      if (JSON.stringify(rows) !== JSON.stringify([["3", "t", null], ["4", "f", "x"]])) throw new Error(JSON.stringify(rows));
    `);
  },

  async "in-cell editor, checkbox and enum picker save to the database"(sc) {
    return run(`
      await sql("CREATE TYPE ${sc}.mood AS ENUM ('ok', 'sad'); CREATE TABLE ${sc}.items (id int PRIMARY KEY, v text, b bool, m ${sc}.mood); INSERT INTO ${sc}.items VALUES (1, 'a', false, 'ok')");
      await open_("${sc}", "items");
      const g = S.view.grid;
      g.select(0, col("v"));
      g.startEdit("typed");
      g.commitEdit();
      await until(async () => (await sql("SELECT v FROM ${sc}.items"))[0][0] === "typed", "text edit saved");
      await new Promise((r) => setTimeout(r, 300)); await S.view.writeChain;
      if (document.querySelector(".toast.err")) throw new Error("after text: " + document.querySelector(".toast.err").textContent);
      g.select(0, col("b"));
      g.startEdit();                                    // Enter on a checkbox toggles it
      await until(async () => (await sql("SELECT b FROM ${sc}.items"))[0][0] === "t", "checkbox saved");
      await new Promise((r) => setTimeout(r, 300)); await S.view.writeChain;
      if (document.querySelector(".toast.err")) throw new Error("after bool: " + document.querySelector(".toast.err").textContent);
      g.select(0, col("m"));
      g.startEdit();
      await until(() => document.querySelector(".pop [data-i]"), "enum picker");
      [...document.querySelectorAll(".pop [data-i]")].find((b) => b.textContent.includes("sad")).click();
      await until(async () => (await sql("SELECT m FROM ${sc}.items"))[0][0] === "sad", "enum saved");
      await S.view.writeChain;
      if (document.querySelector(".toast.err")) throw new Error(document.querySelector(".toast.err").textContent);
    `);
  },

  async "JSON is formatted without changing numbers"() {
    return run(`
      const src = '{"a":12345678901234567890,"b":[1.10,{},[]],"s":"x\\\\"y, {z}: 1"}';
      const p = prettyJSON(src);
      if (!p.includes("12345678901234567890") || !p.includes("1.10") || !p.includes('"x\\\\"y, {z}: 1"')) throw new Error(p);
      if (p.replace(/\\s+/g, "") !== src.replace(/\\s+/g, "").replace(/\\s/g, "")) {
        // whitespace inside the string must survive; compare structurally instead
        if (JSON.stringify(JSON.parse(p)) !== JSON.stringify(JSON.parse(src))) throw new Error("changed: " + p);
      }
      if (prettyJSON("{bad") !== null) throw new Error("invalid JSON must not be formatted");
    `);
  },

  async "saving the record form after the grid moved updates the right record"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, v text, j jsonb); INSERT INTO ${sc}.items SELECT g, 'orig', '{\\"n\\": 12345678901234567890}' FROM generate_series(1, 30) g");
      await open_("${sc}", "items");
      S.view.expand(9);                                 // id 10
      await until(() => drawer && drawer.fields, "record form");
      S.view.settings.sort = [{ col: "id", desc: true }];
      S.view.refresh(true);
      await until(() => S.view.rows.get(9) && S.view.rows.get(9)[0] === "21", "resorted rows");
      drawer.fields.find((f) => f.c.name === "v").val = "from form";
      await saveRecord();
      const rows = await sql("SELECT id, j::text FROM ${sc}.items WHERE v = 'from form'");
      if (rows.length !== 1 || rows[0][0] !== "10") throw new Error("saved into " + JSON.stringify(rows));
      if (!rows[0][1].includes("12345678901234567890")) throw new Error("JSON changed: " + rows[0][1]);
      if (S.view.rows.get(9)[1] !== "orig") throw new Error("the row now at position 9 was repainted");
      await closeDrawer(true);
    `);
  },

  async "a long value cut in the grid is edited, copied, pasted over and undone in full"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, doc text); INSERT INTO ${sc}.items VALUES (1, repeat('ab', 20000)), (2, 'short')");
      await open_("${sc}", "items");
      const g = S.view.grid, c = col("doc");
      const dbDoc = async (id) => (await sql("SELECT doc FROM ${sc}.items WHERE id = " + id))[0][0];
      const row = S.view.rows.get(0);
      if (!row.cut || row[1].length > 8192) throw new Error("value not cut: " + row[1].length);
      if (!g.cellEl(0, c).textContent.endsWith("…")) throw new Error("cut value not marked");
      // in-cell editor opens with the whole value
      g.select(0, c); g.startEdit();
      await until(() => g.editor, "editor");
      if (g.editor.input.value.length !== 40000) throw new Error("editor got " + g.editor.input.value.length + " characters");
      g.editor.input.value += "X"; g.commitEdit();
      await until(async () => (await dbDoc(1)).length === 40001, "edit saved");
      await S.view.writeChain;
      // record form
      S.view.expand(0);
      await until(() => drawer && drawer.fields, "record form");
      if (drawer.fields.find((f) => f.c.name === "doc").val.length !== 40001) throw new Error("form shows a cut value");
      await closeDrawer(true);
      // copy
      g.sel = { r: 0, c, r2: 0, c2: c };
      await g.copy();
      if (lastCopy.matrix[0][0].length !== 40001) throw new Error("copied " + lastCopy.matrix[0][0].length + " characters");
      // paste over it, then undo restores all of it
      await S.view.paste({ r0: 0, r1: 0, c0: c, c1: c }, [["pasted"]]);
      if (await dbDoc(1) !== "pasted") throw new Error("paste not saved");
      await S.view.undoLast();
      const back = await dbDoc(1);
      if (back.length !== 40001 || !back.endsWith("X")) throw new Error("undo restored " + back.length + " characters");
      // a long value written by the grid comes back cut; undo still works
      if (!(await S.view.writeCell(1, "doc", "z".repeat(30000)))) throw new Error("long write failed");
      if (!S.view.rows.get(S.view.locate({ id: "2" })).cut) throw new Error("written value not cut");
      await S.view.undoLast();
      if (await dbDoc(2) !== "short") throw new Error("undo of a long write: " + (await dbDoc(2)).slice(0, 20));
      // overwriting a cut value someone else changed meanwhile is refused
      S.view.refresh(true);
      await until(() => S.view.rows.get(0) && S.view.rows.get(0).cut, "reloaded");
      await sql("UPDATE ${sc}.items SET doc = repeat('cd', 20000) WHERE id = 1");
      await S.view.paste({ r0: 0, r1: 0, c0: c, c1: c }, [["mine"]]);
      if (!(await dbDoc(1)).startsWith("cdcd")) throw new Error("overwrote a concurrent change");
    `);
  },

  async "pages far from view are dropped and load again"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, v text); INSERT INTO ${sc}.items SELECT g, 'v' || g FROM generate_series(1, 20000) g");
      await open_("${sc}", "items");
      const g = S.view.grid;
      for (let r = 0; r < 18000; r += 200) {
        g.sc.scrollTop = r * g.rh; g.draw();
        await until(() => S.view.rows.get(r), "row " + r);
      }
      const kept = S.view.rows.pages.size;
      if (kept > 62) throw new Error(kept + " pages kept");
      if (S.view.rows.get(0)) throw new Error("first page was not dropped");
      g.sc.scrollTop = 0; g.draw();
      await until(() => S.view.rows.get(0) && S.view.rows.get(0)[1] === "v1", "first page again");
    `);
  },

  async "a wide table draws only the columns in view"(sc) {
    return run(`
      await sql("DO $$ BEGIN EXECUTE (SELECT 'CREATE TABLE ${sc}.wide (id int PRIMARY KEY, ' || string_agg('c' || g || ' int DEFAULT ' || g, ', ') || ')' FROM generate_series(1, 300) g); END $$; INSERT INTO ${sc}.wide (id) VALUES (1)");
      await open_("${sc}", "wide");
      const g = S.view.grid;
      await until(() => g.cellEl(0, 1), "first row");
      const n = document.querySelectorAll('.g-row[data-r="0"] .g-c[data-c]').length;
      if (n > 40) throw new Error(n + " cells drawn for one row");
      g.sc.scrollLeft = g.lefts[250]; g.draw();
      await until(() => g.cellEl(0, 250), "column 250");
      if (g.cellEl(0, 250).textContent.trim() !== "250") throw new Error("column 250 shows " + g.cellEl(0, 250).textContent);
      if (!g.cellEl(0, 0)) throw new Error("frozen first column missing");
      g.select(0, 299);
      await until(() => g.cellEl(0, 299), "last column after keyboard move");
    `);
  },

  async "scrolling deep in the default order pages by key"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, v text); INSERT INTO ${sc}.items SELECT g, 'v' || g FROM generate_series(1, 20000) g");
      await open_("${sc}", "items");
      const bodies = [], orig = window.fetch;
      window.fetch = (u, o) => { if (String(u).endsWith("api/rows")) bodies.push(JSON.parse(o.body)); return orig(u, o); };
      try {
        const g = S.view.grid, at = 15000;
        g.sc.scrollTop = at * g.rh; g.draw();
        await until(() => S.view.rows.get(at), "jump");
        for (let r = at; r < at + 2000; r += 200) { g.sc.scrollTop = r * g.rh; g.draw(); await until(() => S.view.rows.get(r + 199), "down " + r); }
        for (let r = at; r > at - 2000; r -= 200) { g.sc.scrollTop = r * g.rh; g.draw(); await until(() => S.view.rows.get(r - 200), "up " + r); }
        for (let r = at - 2000; r < at + 2000; r++) {
          const row = S.view.rows.get(r);
          if (!row) throw new Error("row " + r + " not loaded");
          if (row[0] !== String(r + 1)) throw new Error("row " + r + " is id " + row[0]);
        }
        const keyed = bodies.filter((b) => b.after || b.before).length;
        if (keyed < 15) throw new Error("only " + keyed + " of " + bodies.length + " page requests used a key");
      } finally { window.fetch = orig; }
    `);
  },

  async "the footer adds big integers exactly"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, n bigint); INSERT INTO ${sc}.items VALUES (1, 9007199254740993), (2, 1)");
      await open_("${sc}", "items");
      const g = S.view.grid, c = col("n");
      g.sel = { r: 0, c, r2: 1, c2: c }; g.paintFoot();
      const want = (9007199254740994n).toLocaleString();
      if (!g.foot.textContent.includes("Sum " + want)) throw new Error(g.foot.textContent);
    `);
  },

  async "a malformed link and quick filter changes cause no errors"(sc) {
    return run(`
      await sql("CREATE TABLE ${sc}.items (id int PRIMARY KEY, v text); INSERT INTO ${sc}.items SELECT g, 'v' || g FROM generate_series(1, 5000) g");
      location.hash = "#/t/%E0%A4%A/x";
      await new Promise((r) => setTimeout(r, 200));
      await open_("${sc}", "items");
      for (let i = 0; i < 8; i++) { S.view.settings.search = "v" + i; S.view.refresh(); }
      await until(() => S.view.rows.ready && S.view.rows.get(0), "filtered rows");
      await new Promise((r) => setTimeout(r, 300));
      if (document.querySelector(".toast.err")) throw new Error(document.querySelector(".toast.err").textContent);
      if (S.view.rows.get(0)[1] !== "v7") throw new Error("shows " + S.view.rows.get(0)[1]);
    `);
  },
};

// Run against a server started with -no-sql (E2E_SUITE=nosql).
const nosqlTests = {
  async "with -no-sql the SQL editor and SQL filters are gone"() {
    return run(`
      if (S.meta.sql !== false) throw new Error("server is not in -no-sql mode");
      if (document.querySelector("[data-sqllink]")) throw new Error("the sidebar links to the SQL editor");
      location.hash = "#/sql";
      await until(() => S.view && S.view.meta && S.view.rows && S.view.rows.ready, "a table instead of the editor");
      document.querySelector("[data-filter]").click();
      await until(() => document.querySelector(".rules.pop"), "filter menu");
      if (document.querySelector("[data-addsql]")) throw new Error("the filter menu offers SQL conditions");
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "k", ctrlKey: true, metaKey: true, bubbles: true }));
      await until(() => document.querySelector(".palette"), "command palette");
      if (/SQL editor/.test(document.querySelector(".palette").textContent)) throw new Error("the palette offers the SQL editor");
      const status = await api("query", { sql: "SELECT 1" }).then(() => 200, (e) => e.status);
      if (status !== 403) throw new Error("the query endpoint answered " + status);
    `);
  },
};
const nosql = process.env.E2E_SUITE === "nosql";

let failed = 0;
try {
  await connect();
  await send("Runtime.enable");
  await send("Page.enable");
  await send("Page.addScriptToEvaluateOnNewDocument", { source: "window.__AA_E2E = {}" });
  await send("Page.navigate", { url: BASE });
  for (let i = 0; i < 200; i++) {
    try { if (await run("return !!(window.__AA_E2E && __AA_E2E.app && __AA_E2E.app.S.meta)")) break; } catch {}
    await sleep(50);
  }
  await run(helpers);
  for (const [name, fn] of Object.entries(nosql ? nosqlTests : tests)) {
    const sc = "e2e_" + Math.random().toString(36).slice(2, 8);
    try {
      await run(`document.querySelectorAll(".toast").forEach((t) => t.remove()); ${nosql ? "" : `await sql("CREATE SCHEMA ${sc}");`} return true;`);
      await fn(sc);
      console.log("ok   " + name);
    } catch (e) {
      failed++;
      console.log("FAIL " + name + "\n     " + String(e.message || e).split("\n")[0]);
    } finally {
      await run(`try { await closeDrawer(true); } catch {} location.hash = ""; ${nosql ? "" : `await sql("DROP SCHEMA IF EXISTS ${sc} CASCADE");`} return true;`).catch(() => {});
    }
  }
  const errs = pageErrors.filter((e) => !/clipboard|Clipboard|Document is not focused/.test(e));
  if (errs.length) { failed++; console.log("FAIL uncaught page errors:\n     " + errs.join("\n     ")); }
} finally {
  try { ws && ws.close(); } catch {}
  chrome.kill();
  await sleep(200);
  rmSync(profile, { recursive: true, force: true });
}
console.log(failed ? `${failed} failed` : "all passed");
process.exit(failed ? 1 : 0);
