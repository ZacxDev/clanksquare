// The stdio MCP server runs tool calls concurrently and serialises only the
// tools flagged `state: true` (withStateLock), each loading the state file
// fresh, so two of them cannot lose each other's write. This checks it
// structurally: every MCP tool whose run reaches a local-file writer
// (saveState / writePrivate, directly or through any function in
// agent/src/clank.mjs) must carry `state: true`, and the set of writing tools
// is pinned, so a tool that starts or stops writing shows up here.
import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src", "clank.mjs");
const WRITERS = ["saveState", "writePrivate"];

// analyze: { writes: Set of tool names whose run reaches a writer,
// flagged: Set of tool names with state: true, tools: all tool names }.
export function analyze(src) {
  // Top-level functions: name -> body (up to the next top-level declaration
  // of any kind: function, const, let, class).
  const fns = {};
  const decls = [...src.matchAll(/^(?:async function|function|const|let|class)\b/gm)].map(m => m.index);
  for (const m of src.matchAll(/^(?:async )?function (\w+)\s*\(/gm)) {
    const end = decls.find(d => d > m.index) ?? src.length;
    fns[m[1]] = src.slice(m.index + m[0].length, end);
  }
  const calls = body => new Set([...body.matchAll(/\b(\w+)\s*\(/g)].map(m => m[1]).filter(n => n in fns));
  // Fixed point: a function writes if it calls a writer or a writing function.
  const writing = new Set(WRITERS.filter(w => w in fns));
  for (let changed = true; changed;) {
    changed = false;
    for (const [name, body] of Object.entries(fns)) {
      if (writing.has(name)) continue;
      if ([...calls(body)].some(c => writing.has(c))) { writing.add(name); changed = true; }
    }
  }
  // The TOOLS table: one entry per `{ name: "clank_…"`, up to the next one.
  const table = src.slice(src.indexOf("const TOOLS = ["), src.indexOf("];", src.indexOf("const TOOLS = [")));
  const entries = [...table.matchAll(/\{ name: "(clank_\w+)"/g)];
  const tools = [], flagged = new Set(), writes = new Set();
  entries.forEach((m, i) => {
    const text = table.slice(m.index, i + 1 < entries.length ? entries[i + 1].index : table.length);
    tools.push(m[1]);
    if (/^\{ name: "clank_\w+", state: true,/.test(text)) flagged.add(m[1]);
    const run = text.slice(text.indexOf("run:"));
    if ([...calls(run)].some(c => writing.has(c))) writes.add(m[1]);
  });
  return { tools, flagged, writes, writing };
}

test("every MCP tool that writes local state is flagged state: true", () => {
  const a = analyze(fs.readFileSync(SRC, "utf8"));
  assert.equal(a.tools.length, 12, "the tool table parse found " + a.tools.length + " tools");
  for (const t of a.writes) assert.ok(a.flagged.has(t), t + " writes local state but is not flagged state: true");
  assert.deepEqual([...a.writes].sort(), [
    // clank_interests_show runs opInterests, whose "set" branch writes.
    "clank_create_channel", "clank_digest", "clank_interests_set", "clank_interests_show", "clank_join", "clank_set_name", "clank_subscribe", "clank_whoami",
  ]);
});

test("the analyzer sees an unflagged writer (positive control)", () => {
  const src = `function saveState(ctx) {}
async function opWrites(ctx) { helper(ctx); }
function helper(ctx) { saveState(ctx); }
async function opReads(ctx) { return 1; }
const TOOLS = [
  { name: "clank_a", description: "x", run: (ctx, a) => opWrites(ctx) },
  { name: "clank_b", state: true, description: "y", run: (ctx, a) => opWrites(ctx) },
  { name: "clank_c", description: "z", run: ctx => opReads(ctx) },
];`;
  const a = analyze(src);
  assert.deepEqual([...a.writes].sort(), ["clank_a", "clank_b"]);
  assert.deepEqual([...a.flagged], ["clank_b"]);
  assert.ok(!a.flagged.has("clank_a"), "clank_a is the unflagged writer the real test must catch");
});
