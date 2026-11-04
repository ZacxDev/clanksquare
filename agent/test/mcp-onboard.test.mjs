// The stdio MCP server's auto-init path: the first tool call that creates
// the key subscribes it to #recommendations (once, as init does), unless
// CLANK_NO_RECS=1, and stdout stays pure JSON-RPC. Runs the built
// agent/clank.mjs against a minimal in-process fake hub.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import * as crypto from "node:crypto";
import * as fs from "node:fs";
import * as http from "node:http";
import * as os from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const CLANK = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "clank.mjs");

function base32(buf) {
  const A = "abcdefghijklmnopqrstuvwxyz234567";
  let bits = 0, v = 0, out = "";
  for (const b of buf) {
    v = (v << 8) | b; bits += 8;
    while (bits >= 5) { out += A[(v >>> (bits - 5)) & 31]; bits -= 5; }
  }
  if (bits > 0) out += A[(v << (5 - bits)) & 31];
  return out;
}
const payloadOf = tok => Buffer.from(tok.split(".")[2], "base64url");
const objectId = tok => "m_" + base32(crypto.createHash("sha256").update(payloadOf(tok)).digest());

// The fake hub stores channel geneses and subs; it checks no signature (the
// client verifies what it is served, and the tokens are the client's own).
async function fakeHub() {
  const channels = {}; // slug -> { id, token }
  const subs = [];     // { channel, token, from }
  const srv = http.createServer((req, res) => {
    let body = "";
    req.on("data", c => { body += c; });
    req.on("end", () => {
      const json = (status, v) => { res.writeHead(status, { "Content-Type": "application/json" }); res.end(JSON.stringify(v)); };
      const u = new URL(req.url, "http://x");
      if (req.method === "POST" && u.pathname === "/v1/submit") {
        const tok = body.trim(), p = JSON.parse(payloadOf(tok)), id = objectId(tok);
        if (p.t === "channel") channels[p.slug] = { id: "c_" + id.slice(2), token: tok };
        else if (p.t === "sub") subs.push({ channel: p.channel, token: tok, from: p.from });
        return json(200, { id });
      }
      if (req.method === "GET" && u.pathname === "/v1/subs") return json(200, { items: subs.map(s => ({ channel: s.channel, token: s.token })) });
      const m = /^\/v1\/channels\/(%23|#)([a-z0-9-]+)$/.exec(u.pathname);
      if (req.method === "GET" && m && channels[m[2]]) {
        const c = channels[m[2]];
        return json(200, { channel: c.id, manifest: c.token, genesis: c.token });
      }
      json(404, { error: "E_NOT_FOUND", detail: "fake" });
    });
  });
  await new Promise(r => srv.listen(0, "127.0.0.1", r));
  return { url: "http://127.0.0.1:" + srv.address().port, subs, close: () => srv.close() };
}

function env(hub, key, extra = {}) {
  const e = { ...process.env, CLANK_HUB: hub.url, CLANK_HUB_NAME: "hub.test", CLANK_KEY: key, ...extra };
  delete e.CLANK_NO_RECS;
  return { ...e, ...extra };
}

// mcp runs the stdio server for one clank_whoami call and returns
// { lines: stdout lines, err: stderr }.
function mcp(hub, key, extra) {
  return new Promise((resolve, reject) => {
    const p = spawn(process.execPath, [CLANK, "mcp"], { env: env(hub, key, extra) });
    let out = "", err = "";
    p.stdout.on("data", c => { out += c; });
    p.stderr.on("data", c => { err += c; });
    p.on("error", reject);
    p.on("close", () => resolve({ lines: out.split("\n").filter(Boolean), err }));
    const meta = { "io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": {} };
    p.stdin.end(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "clank_whoami", arguments: {}, _meta: meta } }) + "\n");
  });
}

// cli runs one clank.mjs command without blocking the fake hub's event loop.
function cli(e, args) {
  return new Promise((resolve, reject) => {
    const p = spawn(process.execPath, [CLANK, ...args], { env: e, stdio: ["ignore", "pipe", "pipe"] });
    let out = "", err = "";
    p.stdout.on("data", c => { out += c; });
    p.stderr.on("data", c => { err += c; });
    p.on("error", reject);
    p.on("close", code => resolve({ code, out, err }));
  });
}

async function withHub(fn) {
  const hub = await fakeHub();
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "clank-mcp-onboard-"));
  try {
    // The channel, created by another key that does not subscribe itself.
    const ownerEnv = env(hub, path.join(dir, "owner.key"), { CLANK_NO_RECS: "1" });
    let ownerId = "";
    for (const args of [["init"], ["create", "recommendations", "--title", "Recs", "--post", "owner"]]) {
      const r = await cli(ownerEnv, args);
      assert.equal(r.code, 0, r.err);
      ownerId ||= r.out.trim();
    }
    await fn(hub, dir, ownerId);
  } finally { hub.close(); fs.rmSync(dir, { recursive: true }); }
}

function assertPureJSONRPC(lines) {
  assert.ok(lines.length >= 1, "no answer on stdout");
  for (const l of lines) {
    const m = JSON.parse(l); // throws on anything that is not JSON
    assert.equal(m.jsonrpc, "2.0", "stdout line is not JSON-RPC: " + l);
  }
}

test("MCP auto-init subscribes the new key to #recommendations, once", async () => {
  await withHub(async (hub, dir, owner) => {
    const key = path.join(dir, "agent.key");
    const trust = { CLANK_RECS_OWNER: owner }; // hub.test is not the default hub
    const first = await mcp(hub, key, trust);
    assertPureJSONRPC(first.lines);
    const res = JSON.parse(first.lines[0]).result;
    assert.equal(res.isError, false, JSON.stringify(res));
    const id = /key=(k_[a-z2-7]{52})/.exec(res.content[0].text)[1];
    assert.deepEqual(hub.subs.map(s => s.from), [id], "the MCP auto-init path did not subscribe the new key");
    assert.match(first.err, /subscribed to #recommendations: .*CLANK_NO_RECS=1 in the server's environment/);
    // A second server on the same key: the key exists, nothing more is sent.
    const second = await mcp(hub, key, trust);
    assertPureJSONRPC(second.lines);
    assert.equal(hub.subs.length, 1, "subscribed twice");
    assert.equal(second.err, "");
  });
});

test("MCP auto-init with CLANK_NO_RECS=1 does not subscribe", async () => {
  await withHub(async (hub, dir, owner) => {
    const r = await mcp(hub, path.join(dir, "quiet.key"), { CLANK_NO_RECS: "1", CLANK_RECS_OWNER: owner });
    assertPureJSONRPC(r.lines);
    assert.equal(JSON.parse(r.lines[0]).result.isError, false);
    assert.equal(fs.existsSync(path.join(dir, "quiet.key")), true, "control: the key was created");
    assert.equal(hub.subs.length, 0);
  });
});

test("MCP auto-init on a hub without #recommendations: the tool call still succeeds", async () => {
  const hub = await fakeHub();
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "clank-mcp-onboard-"));
  try {
    const r = await mcp(hub, path.join(dir, "agent.key"), { CLANK_RECS_OWNER: "k_" + "a".repeat(52) });
    assertPureJSONRPC(r.lines);
    assert.equal(JSON.parse(r.lines[0]).result.isError, false);
    assert.match(r.err, /^warning: no #recommendations channel on this hub/);
    assert.equal(hub.subs.length, 0);
  } finally { hub.close(); fs.rmSync(dir, { recursive: true }); }
});

test("MCP auto-init: a #recommendations owned by another key is not subscribed", async () => {
  await withHub(async (hub, dir) => {
    const r = await mcp(hub, path.join(dir, "agent.key"), { CLANK_RECS_OWNER: "k_" + "b".repeat(52) });
    assertPureJSONRPC(r.lines);
    assert.equal(JSON.parse(r.lines[0]).result.isError, false);
    assert.match(r.err, /^warning: #recommendations on this hub is owned by k_.*not the trusted recommender .*init did not subscribe/);
    assert.equal(hub.subs.length, 0);
  });
});

test("MCP auto-init on a non-default hub without CLANK_RECS_OWNER: no subscription, said once", async () => {
  await withHub(async (hub, dir) => {
    const key = path.join(dir, "agent.key");
    const r = await mcp(hub, key);
    assertPureJSONRPC(r.lines);
    assert.match(r.err, /^warning: no trusted recommender key for this hub \(set CLANK_RECS_OWNER=k_…/);
    assert.equal(hub.subs.length, 0);
    // Said once per hub: init on the same key and hub stays quiet.
    const again = await cli(env(hub, key), ["init"]);
    assert.equal(again.code, 0);
    assert.equal(again.err, "");
  });
});
