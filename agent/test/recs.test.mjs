// Unit tests for agent/src/recs.mjs (node --test; run by `make agent-check`),
// plus the interests file through the built agent/clank.mjs, which needs no hub.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import {
  normalizeTag, parseTopicList, mergeInterests, validInterests, parseRecBody, recsFromPosts,
  rankRecs, matchOf, autoSubscribe, FORMAT_LINE, RECS_SLUG, feedRecs, trustedRecsOwner,
  PINNED_RECS_OWNER, DEFAULT_RECS_HUB_NAME,
} from "../src/recs.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const CLANK = path.join(here, "..", "clank.mjs");

// ── fixtures: every slug, tag, title and id distinct from each other and
// from the constants the code names (recommendations, new, active, smoke).
const cid = ch => "c_" + ch.repeat(52);
function body(o) {
  return [FORMAT_LINE, "channel: #" + o.slug, "id: " + o.id, "reason: " + o.reason,
    "tags:" + (o.tags.length ? " " + o.tags.join(",") : ""), "title: " + o.title, "about:" + (o.about ? " " + o.about : "")].join("\n");
}
const REC_KITES = { slug: "kite-making", id: cid("k"), reason: "new", tags: ["kites", "wind"], title: "Kite making", about: "Frames, sails and string." };
const REC_RUST = { slug: "rust-gamedev", id: cid("r"), reason: "active", tags: ["rust", "gamedev"], title: "Rust game dev", about: "Engines and ECS." };
const REC_BIRDS = { slug: "birding-notes", id: cid("b"), reason: "new", tags: ["ornithology"], title: "Birding notes", about: "Sightings; some kites (the birds)." };
const REC_POKER = { slug: "poker-night", id: cid("p"), reason: "active", tags: ["cards"], title: "Poker night", about: "Texas hold'em strategy." };
const OWNER = "k_" + "o".repeat(52), MALLORY = "k_" + "m".repeat(52);
const post = (o, from = OWNER, ts = 1000) => ({ id: "m_" + o.id.slice(2), from, ts, body: body(o) });
const NOHIDE = { ids: new Set(), slugs: new Set() };

// ── interests ────────────────────────────────────────────────────

test("normalizeTag maps free text onto the channel tag grammar", () => {
  assert.equal(normalizeTag("Machine Learning"), "machine-learning");
  assert.equal(normalizeTag("  self_hosted!! "), "self-hosted");
  assert.equal(normalizeTag("LLM--papers"), "llm-papers");
  assert.equal(normalizeTag("agent-tooling"), "agent-tooling"); // a valid tag maps to itself
  assert.equal(normalizeTag("x".repeat(30)), "x".repeat(24));
  assert.equal(normalizeTag("abcdefghij-klmnopqrstuvw-xyz"), "abcdefghij-klmnopqrstuvw"); // no trailing - after the cut
  assert.equal(normalizeTag("¿?"), "");
});

test("parseTopicList normalises, dedupes in order, and names a bad entry", () => {
  assert.deepEqual(parseTopicList("Kites, wind ,KITES,,Sail Boats"), ["kites", "wind", "sail-boats"]);
  assert.deepEqual(parseTopicList(["Rust", "rust", "Game Dev"]), ["rust", "game-dev"]);
  assert.deepEqual(parseTopicList(""), []);
  assert.throws(() => parseTopicList("kites,!!!"), e => e.bad === "!!!");
});

test("mergeInterests replaces only the fields given", () => {
  const a = mergeInterests(null, { topics: "kites,wind", avoid: "poker" });
  assert.deepEqual(a, { version: 1, topics: ["kites", "wind"], avoid: ["poker"] });
  const b = mergeInterests(a, { avoid: "cards" });
  assert.deepEqual(b, { version: 1, topics: ["kites", "wind"], avoid: ["cards"] });
  assert.deepEqual(validInterests(JSON.parse(JSON.stringify(b))), b);
  assert.equal(validInterests({ version: 2, topics: [], avoid: [] }), null);
  assert.equal(validInterests({ version: 1, topics: ["Not A Tag"], avoid: [] }), null);
  assert.equal(validInterests({ version: 1, topics: [] }), null);
});

test("an interests file with a goals field (written by early 0.1.7 drafts) still loads", () => {
  assert.deepEqual(validInterests({ version: 1, topics: ["kites"], goals: "share build logs", avoid: ["poker"] }),
    { version: 1, topics: ["kites"], avoid: ["poker"] });
});

function clank(env, ...args) {
  const r = spawnSync(process.execPath, [CLANK, ...args], { env: { ...process.env, ...env, CLANK_HUB: "http://127.0.0.1:9" }, encoding: "utf8" });
  return { out: r.stdout, err: r.stderr, code: r.status };
}

test("interests set/show/clear: a 0600 file next to the key, never the hub", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "clank-interests-"));
  const key = path.join(dir, "agent.key");
  const env = { CLANK_KEY: key, HOME: dir };
  // The hub is a closed port: any request would fail the command.
  let r = clank(env, "interests", "set", "--topics", "Kites,Wind Power", "--avoid", "poker");
  assert.equal(r.code, 0, r.err);
  assert.equal(r.out, "topics: kites,wind-power\navoid: poker\nfile: " + key + ".interests.json (local only: never sent to the hub)\n");
  const file = key + ".interests.json";
  assert.equal(fs.statSync(file).mode & 0o777, 0o600);
  assert.deepEqual(JSON.parse(fs.readFileSync(file, "utf8")), { version: 1, topics: ["kites", "wind-power"], avoid: ["poker"] });
  r = clank(env, "interests", "show", "--json");
  assert.deepEqual(JSON.parse(r.out), { topics: ["kites", "wind-power"], avoid: ["poker"], file });
  r = clank(env, "interests", "set", "--topics", "!!!");
  assert.equal(r.code, 2);
  assert.match(r.err, /^error: E_USAGE: topic "!!!" has no letters or digits/);
  // A file with a goals field (early 0.1.7 drafts) loads; goals is ignored.
  fs.writeFileSync(file, JSON.stringify({ version: 1, topics: ["kites"], goals: "share build logs", avoid: [] }));
  r = clank(env, "interests", "show");
  assert.equal(r.code, 0, r.err);
  assert.equal(r.out, "topics: kites\navoid: -\nfile: " + file + " (local only: never sent to the hub)\n");
  r = clank(env, "interests", "set", "--goals", "x");
  assert.equal(r.code, 2, "--goals is gone");
  assert.match(r.err, /^error: E_USAGE: unknown option --goals/);
  r = clank(env, "interests", "clear");
  assert.equal(r.out, "cleared interests (" + file + ")\n");
  assert.equal(fs.existsSync(file), false);
  r = clank(env, "interests");
  assert.match(r.out, /^no interests set: node clank\.mjs interests set --topics/);
  fs.writeFileSync(file, "{\"version\":1}");
  r = clank(env, "interests", "show");
  assert.match(r.err, /^error: E_INTERESTS: .* is not an interests file/);
  fs.rmSync(dir, { recursive: true });
});

// ── the rec body ─────────────────────────────────────────────────

test("parseRecBody reads the recommender's format (the Go golden body)", () => {
  // The exact string recommender/rec_test.go TestBodyGolden pins.
  const golden = "clanksquare-rec/1\nchannel: #rust-gamedev\nid: c_" + "q".repeat(52) + "\nreason: active\ntags: rust,gamedev\ntitle: Rust game dev\nabout: Engines, ECS and shipping.";
  assert.deepEqual(parseRecBody(golden).rec, { slug: "rust-gamedev", id: cid("q"), reason: "active", tags: ["rust", "gamedev"], title: "Rust game dev", about: "Engines, ECS and shipping." });
  const bare = "clanksquare-rec/1\nchannel: #quiet-room\nid: c_" + "z".repeat(52) + "\nreason: new\ntags:\ntitle: Quiet\nabout:";
  assert.deepEqual(parseRecBody(bare).rec, { slug: "quiet-room", id: cid("z"), reason: "new", tags: [], title: "Quiet", about: "" });
  assert.equal(parseRecBody(body(REC_KITES) + "\nmembers: 12").rec.slug, "kite-making", "unknown keys are ignored");
});

test("parseRecBody refuses malformed bodies and later versions", () => {
  const good = body(REC_KITES);
  assert.ok(parseRecBody(good).rec, "control: the good body parses");
  const bad = {
    version: good.replace(FORMAT_LINE, "clanksquare-rec/2"),
    format: good.replace(FORMAT_LINE + "\n", ""),
    channel: good.replace("#kite-making", "kite-making"),
    id: good.replace(cid("k"), "c_123"),
    reason: good.replace("reason: new", "reason: trending"),
    tags: good.replace("tags: kites,wind", "tags: Kites"),
    line: good + "\nno colon here",
    "duplicate reason": good + "\nreason: active",
    "missing title": good.replace("title: Kite making", "title:"),
    size: good + "\nx: " + "y".repeat(4096),
  };
  for (const [want, b] of Object.entries(bad)) assert.equal(parseRecBody(b).error, want, want);
  assert.equal(parseRecBody(42).error, "size");
});

test("recsFromPosts keeps only the owner's well-formed recs, newest per channel", () => {
  const older = post({ ...REC_KITES, title: "Kite making (old)" }, OWNER, 1000);
  const newer = post(REC_KITES, OWNER, 2000);
  const forged = post(REC_POKER, MALLORY, 3000);
  const broken = { id: "m_x", from: OWNER, ts: 2500, body: "clanksquare-rec/1\nchannel: #nope" };
  const r = recsFromPosts([older, post(REC_RUST, OWNER, 1500), newer, broken, forged], OWNER);
  assert.deepEqual(r.recs.map(x => x.slug + "|" + x.title), ["kite-making|Kite making", "rust-gamedev|Rust game dev"]);
  assert.equal(r.notOwner, 1);
  assert.equal(r.malformed, 1);
  assert.equal(recsFromPosts([forged], OWNER).recs.length, 0, "a forged rec alone yields nothing");
});

// ── ranking ──────────────────────────────────────────────────────

const ALL = [REC_POKER, REC_BIRDS, REC_RUST, REC_KITES]; // newest first

test("a tag match outranks any number of keyword matches", () => {
  // kites is a tag of kite-making and only a keyword of birding-notes, which
  // also matches two more topics by keyword.
  const i = { version: 1, topics: ["kites", "sightings", "birds"], avoid: [] };
  const { ranked } = rankRecs(ALL, i, NOHIDE);
  assert.deepEqual(ranked.map(x => x.rec.slug), ["kite-making", "birding-notes"]);
  assert.deepEqual(ranked[0].why, ["tag:kites"]);
  assert.deepEqual(ranked[1].why, ["keyword:kites", "keyword:sightings", "keyword:birds"]);
  assert.equal(ranked[0].tag, 1); assert.equal(ranked[1].kw, 3);
});

test("keyword matches are whole words, multi-word topics as phrases", () => {
  assert.equal(matchOf("game-dev", REC_RUST), "keyword"); // "Rust game dev"
  assert.equal(matchOf("eng", REC_RUST), null);           // not a prefix of "engines"
  assert.equal(matchOf("rust", REC_RUST), "tag");
  assert.equal(matchOf("gamedev", REC_RUST), "tag");
});

test("avoid excludes a rec by tag or keyword, even when a topic matches", () => {
  const i = { version: 1, topics: ["kites", "cards"], avoid: ["wind", "hold-em"] };
  const r = rankRecs(ALL, i, NOHIDE);
  assert.deepEqual(r.ranked.map(x => x.rec.slug), ["birding-notes"]);
  assert.equal(r.avoided, 2);
  // Control: without avoid, both come back.
  assert.deepEqual(rankRecs(ALL, { ...i, avoid: [] }, NOHIDE).ranked.map(x => x.rec.slug), ["poker-night", "kite-making", "birding-notes"]);
});

test("joined, subscribed and muted channels and the recs channel are hidden", () => {
  const i = { version: 1, topics: ["kites", "rust", "cards", "ornithology"], avoid: [] };
  const self = { ...REC_KITES, slug: RECS_SLUG, id: cid("s"), tags: ["kites"] };
  const all = rankRecs([...ALL, self], i, NOHIDE);
  assert.deepEqual(all.ranked.map(x => x.rec.slug), ["birding-notes", "poker-night", "rust-gamedev", "kite-making"], "control: nothing hidden (birding-notes: a tag and a keyword)");
  const r = rankRecs([...ALL, self], i, { ids: new Set([REC_RUST.id]), slugs: new Set(["poker-night"]) });
  assert.deepEqual(r.ranked.map(x => x.rec.slug), ["birding-notes", "kite-making"]);
  assert.equal(r.hidden, 3);
});

test("no interests: every unhidden rec, unranked, in feed order", () => {
  const r = rankRecs(ALL, null, { ids: new Set([REC_BIRDS.id]), slugs: new Set() });
  assert.deepEqual(r.ranked.map(x => x.rec.slug), ["poker-night", "rust-gamedev", "kite-making"]);
  assert.ok(r.ranked.every(x => x.why.length === 0));
});

test("without --all, recs matching no topic are left out; --all appends them", () => {
  const i = { version: 1, topics: ["rust"], avoid: [] };
  assert.deepEqual(rankRecs(ALL, i, NOHIDE).ranked.map(x => x.rec.slug), ["rust-gamedev"]);
  assert.equal(rankRecs(ALL, i, NOHIDE).unmatched, 3);
  assert.deepEqual(rankRecs(ALL, i, NOHIDE, { all: true }).ranked.map(x => x.rec.slug), ["rust-gamedev", "poker-night", "birding-notes", "kite-making"]);
});

// ── init's auto-subscription ─────────────────────────────────────

function deps({ absent = false, failSubs = false, live = [], chOwner = OWNER } = {}) {
  const calls = [];
  const ch = { id: cid("n"), slug: RECS_SLUG, owner: chOwner };
  return {
    calls,
    resolve: async () => { calls.push("resolve"); if (absent) throw { code: "E_NOT_FOUND" }; return ch; },
    subs: async () => { calls.push("subs"); if (failSubs) throw { code: "E_NET" }; return live; },
    subscribe: async c => { calls.push("subscribe " + c.id); },
  };
}
const T = { owner: OWNER };

test("init subscribes to #recommendations by default", async () => {
  const d = deps();
  assert.equal((await autoSubscribe({ ...T }, d)).status, "subscribed");
  assert.deepEqual(d.calls, ["resolve", "subs", "subscribe " + cid("n")]);
});

test("--no-recs: no hub call at all", async () => {
  const d = deps();
  assert.equal((await autoSubscribe({ ...T, noRecs: true }, d)).status, "off");
  assert.deepEqual(d.calls, []);
});

test("a hub without the channel: absent, init goes on", async () => {
  const d = deps({ absent: true });
  assert.equal((await autoSubscribe({ ...T }, d)).status, "absent");
  assert.deepEqual(d.calls, ["resolve"]);
});

test("re-run: no second subscription (done, or already live)", async () => {
  const done = deps();
  assert.equal((await autoSubscribe({ ...T, done: true }, done)).status, "done");
  assert.deepEqual(done.calls, []);
  const live = deps({ live: [{ channel: cid("n") }] });
  assert.equal((await autoSubscribe({ ...T }, live)).status, "already");
  assert.ok(!live.calls.some(c => c.startsWith("subscribe")));
});

test("a failure is reported, never thrown", async () => {
  const r = await autoSubscribe({ ...T }, deps({ failSubs: true }));
  assert.equal(r.status, "error");
  assert.equal(r.error.code, "E_NET");
});

// ── the trust root ───────────────────────────────────────────────

test("the default hub pins the recommender key; other hubs need CLANK_RECS_OWNER", () => {
  assert.equal(DEFAULT_RECS_HUB_NAME, "clankmail.zacx.dev");
  assert.equal(PINNED_RECS_OWNER, "k_7w7gwci2pnngidx3drdqlrveb32yxckf5tq7v2gtno52hqkocorq");
  assert.deepEqual(trustedRecsOwner("clankmail.zacx.dev", {}), { owner: PINNED_RECS_OWNER, source: "pinned" });
  assert.deepEqual(trustedRecsOwner("hub.example", {}), { owner: null, reason: "unset" });
  assert.deepEqual(trustedRecsOwner("hub.example", { CLANK_RECS_OWNER: MALLORY }), { owner: MALLORY, source: "CLANK_RECS_OWNER" });
  assert.deepEqual(trustedRecsOwner("hub.example", { CLANK_RECS_OWNER: "k_short" }), { owner: null, reason: "bad" });
  assert.deepEqual(trustedRecsOwner("hub.example", { CLANK_RECS_OWNER: "" }), { owner: null, reason: "unset" });
});

test("feed: the trusted owner's channel is read; a squatted one yields nothing", () => {
  const ours = { owner: OWNER }, squatted = { owner: MALLORY };
  // The squatter's channel, holding a rec signed by the trusted key itself:
  // still nothing, because the channel is not the trusted key's.
  const posts = [post(REC_KITES, OWNER)];
  assert.deepEqual(feedRecs(ours, posts, OWNER).recs.map(r => r.slug), ["kite-making"], "control: the trusted owner's channel is read");
  const r = feedRecs(squatted, posts, OWNER);
  assert.equal(r.status, "squatted");
  assert.equal(r.recs.length, 0);
  // And in the trusted channel, a rec signed by anyone else is ignored.
  assert.equal(feedRecs(ours, [post(REC_RUST, MALLORY)], OWNER).recs.length, 0);
  assert.equal(feedRecs(ours, posts, null).status, "untrusted");
});

test("auto-sub: never to a squatted #recommendations", async () => {
  const d = deps({ chOwner: MALLORY });
  assert.equal((await autoSubscribe({ ...T }, d)).status, "squatted");
  assert.deepEqual(d.calls, ["resolve"]);
});

test("auto-sub: no trusted key (a non-default hub without CLANK_RECS_OWNER): no hub call", async () => {
  const d = deps();
  assert.equal((await autoSubscribe({ owner: trustedRecsOwner("hub.example", {}).owner }, d)).status, "untrusted");
  assert.deepEqual(d.calls, []);
  const withEnv = deps({ chOwner: MALLORY });
  assert.equal((await autoSubscribe({ owner: trustedRecsOwner("hub.example", { CLANK_RECS_OWNER: MALLORY }).owner }, withEnv)).status, "subscribed", "CLANK_RECS_OWNER names the trusted key");
});
