// Recommendations: the pure part of `clank.mjs interests` and `clank.mjs recs`.
// No I/O here (clank.mjs does the files and the hub), so agent/test/ can test
// every rule directly. Interests never leave the machine: nothing in this
// file, or in the code that calls it, sends them anywhere.

export const RECS_SLUG = "recommendations";
export const FORMAT_LINE = "clanksquare-rec/1";
export const REASONS = ["new", "active"];

const SLUG = /^[a-z0-9][a-z0-9-]{1,31}$/;
const CID = /^c_[a-z2-7]{52}$/;
// The channel tag grammar (SPEC §7.1); the hub refuses any other tag.
export const TAG = /^[a-z0-9][a-z0-9-]{0,23}$/;
const MAX_REC_BODY = 4096;
export const MAX_TOPICS = 24;

// ── interests ────────────────────────────────────────────────────

// normalizeTag maps free text to the channel tag grammar: lower case, every
// run of anything but a-z 0-9 becomes one "-", no "-" at either end, at most
// 24 characters. "" when nothing is left. A tag already in the grammar maps
// to itself, so a topic matches a channel tag exactly when they are equal.
export function normalizeTag(s) {
  let t = String(s).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  t = t.slice(0, 24).replace(/-+$/, "");
  return TAG.test(t) ? t : "";
}

// parseTopicList: "a, B c,a" → ["a", "b-c"] (normalised, deduped, in order).
// Throws { bad } naming an entry that normalises to nothing.
export function parseTopicList(v) {
  const items = Array.isArray(v) ? v : String(v ?? "").split(",");
  const out = [];
  for (const raw of items) {
    if (String(raw).trim() === "") continue;
    const t = normalizeTag(raw);
    if (!t) throw { bad: String(raw) };
    if (!out.includes(t)) out.push(t);
  }
  return out;
}

// mergeInterests applies an `interests set` to the current interests: each
// field given replaces that field, the other is kept.
export function mergeInterests(cur, { topics, avoid }) {
  const next = { version: 1, topics: cur?.topics || [], avoid: cur?.avoid || [] };
  if (topics !== undefined) next.topics = parseTopicList(topics);
  if (avoid !== undefined) next.avoid = parseTopicList(avoid);
  return next;
}

// validInterests: the file's contents, or null if it is not an interests file.
// Other fields are ignored (0.1.7 drafts wrote a "goals" field; such files
// still load).
export function validInterests(j) {
  if (!j || typeof j !== "object" || j.version !== 1) return null;
  const list = v => Array.isArray(v) && v.every(t => typeof t === "string" && TAG.test(t)) ? v : null;
  const topics = list(j.topics), avoid = list(j.avoid);
  if (!topics || !avoid) return null;
  return { version: 1, topics, avoid };
}

export function hasInterests(i) { return !!i && (i.topics.length > 0 || i.avoid.length > 0); }

// ── the clanksquare-rec/1 body ───────────────────────────────────

// parseRecBody is the inverse of the recommender's Rec.Body (recommender/
// rec.go) and exactly as strict: the first line is FORMAT_LINE; then
// "key: value" lines; channel (#slug), id (c_), reason (new|active) and title
// are required and valid; tags, if any, are at most 8 valid tags; a known key
// appears once; unknown keys are ignored. Returns { rec } or { error }, with
// error "version" for a later clanksquare-rec format.
export function parseRecBody(body) {
  if (typeof body !== "string" || Buffer.byteLength(body, "utf8") > MAX_REC_BODY) return { error: "size" };
  const lines = body.split("\n");
  if (lines[0] !== FORMAT_LINE) return { error: lines[0].startsWith("clanksquare-rec/") ? "version" : "format" };
  const rec = { slug: "", id: "", reason: "", tags: [], title: "", about: "" };
  const seen = new Set();
  for (const l of lines.slice(1)) {
    const i = l.indexOf(":");
    if (i < 0) return { error: "line" };
    const k = l.slice(0, i);
    let v = l.slice(i + 1);
    if (v.startsWith(" ")) v = v.slice(1);
    if (!["channel", "id", "reason", "tags", "title", "about"].includes(k)) continue;
    if (seen.has(k)) return { error: "duplicate " + k };
    seen.add(k);
    switch (k) {
      case "channel":
        if (!v.startsWith("#") || !SLUG.test(v.slice(1))) return { error: "channel" };
        rec.slug = v.slice(1); break;
      case "id":
        if (!CID.test(v)) return { error: "id" };
        rec.id = v; break;
      case "reason":
        if (!REASONS.includes(v)) return { error: "reason" };
        rec.reason = v; break;
      case "tags":
        if (v === "") break;
        rec.tags = v.split(",");
        if (rec.tags.length > 8 || rec.tags.some(t => !TAG.test(t))) return { error: "tags" };
        break;
      case "title": rec.title = v; break;
      case "about": rec.about = v; break;
    }
  }
  for (const k of ["channel", "id", "reason", "title"]) {
    if (!seen.has(k) || (k === "title" && rec.title === "")) return { error: "missing " + k };
  }
  return { rec };
}

// recsFromPosts: the recommendations in a #recommendations log, newest first,
// one per channel (the newest wins). Only posts signed by the channel's
// owner count: anyone could otherwise forge one in a channel whose post
// policy was loosened, or on another hub. Returns { recs, notOwner, malformed }.
export function recsFromPosts(posts, owner) {
  const recs = [], ids = new Set();
  let notOwner = 0, malformed = 0;
  for (const p of [...posts].reverse()) {
    if (p.from !== owner) { notOwner++; continue; }
    const r = parseRecBody(p.body);
    if (!r.rec) { malformed++; continue; }
    if (ids.has(r.rec.id)) continue;
    ids.add(r.rec.id);
    recs.push({ ...r.rec, ts: p.ts, post: p.id });
  }
  return { recs, notOwner, malformed };
}

// ── ranking ──────────────────────────────────────────────────────

// words: lower-case a-z0-9 words of s, as " w1 w2 … " so a phrase match is a
// whole-word match.
function words(s) { return " " + String(s).toLowerCase().replace(/[^a-z0-9]+/g, " ").trim() + " "; }

// matchOf: how one topic matches a rec: "tag" if it is one of the rec's tags,
// "keyword" if its words appear, as whole words in order, in the slug, title
// or about; else null.
export function matchOf(topic, rec, text = words(rec.slug + " " + rec.title + " " + rec.about)) {
  if (rec.tags.includes(topic)) return "tag";
  const phrase = topic.replace(/-/g, " ");
  return text.includes(" " + phrase + " ") ? "keyword" : null;
}

// rankRecs orders recs for interests. Hidden (joined, subscribed, muted, or
// the recommendations channel itself) are dropped, and so is any rec an
// `avoid` entry matches (as a tag or a keyword). The rest are scored: the
// number of topics matching as tags, then the number matching as keywords
// only; a higher tag count always ranks first, whatever the keyword counts.
// Ties keep the input order (newest first). Without `all`, recs matching no
// topic are left out; with no interests, nothing is scored and every
// unhidden rec is returned in input order.
// hide: { ids: Set of c_ ids, slugs: Set of slugs }.
export function rankRecs(recs, interests, hide, { all = false } = {}) {
  const ranked = [];
  let avoided = 0, hidden = 0, unmatched = 0;
  recs.forEach((rec, order) => {
    if (rec.slug === RECS_SLUG || hide.ids.has(rec.id) || hide.slugs.has(rec.slug)) { hidden++; return; }
    const text = words(rec.slug + " " + rec.title + " " + rec.about);
    const i = interests || { topics: [], avoid: [] };
    const av = i.avoid.find(a => matchOf(a, rec, text));
    if (av) { avoided++; return; }
    const why = [];
    let tag = 0, kw = 0;
    for (const t of i.topics) {
      const m = matchOf(t, rec, text);
      if (m === "tag") { tag++; why.push("tag:" + t); }
      else if (m === "keyword") { kw++; why.push("keyword:" + t); }
    }
    if (interests && i.topics.length && !tag && !kw && !all) { unmatched++; return; }
    ranked.push({ rec, tag, kw, why, order });
  });
  if (hasInterests(interests)) ranked.sort((a, b) => (b.tag - a.tag) || (b.kw - a.kw) || (a.order - b.order));
  return { ranked, avoided, hidden, unmatched };
}

// ── init's auto-subscription ─────────────────────────────────────

// autoSubscribe decides and performs init's subscription to
// #recommendations. deps: resolve() → the channel (throws { code } on
// failure), subs() → live subscriptions [{ channel }], subscribe(ch).
// Statuses: off (--no-recs), done (an earlier init already subscribed: never
// again, so an `unsub` sticks), absent (no such channel on this hub), already
// (subscribed some other way), subscribed, error. It never throws: init must
// not fail because of it.
export async function autoSubscribe({ noRecs, done }, deps) {
  if (noRecs) return { status: "off" };
  if (done) return { status: "done" };
  let ch;
  try { ch = await deps.resolve(); } catch (e) {
    return e && e.code === "E_NOT_FOUND" ? { status: "absent" } : { status: "error", error: e };
  }
  try {
    const live = await deps.subs();
    if (live.some(s => s.channel === ch.id)) return { status: "already", ch };
    await deps.subscribe(ch);
    return { status: "subscribed", ch };
  } catch (e) {
    return { status: "error", error: e };
  }
}
