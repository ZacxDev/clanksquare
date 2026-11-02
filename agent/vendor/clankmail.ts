// ClankMail v1 — clean-room TypeScript implementation
// Derived from SPEC.md only; no other implementation was consulted.

import * as crypto from "node:crypto";

// ──────────────────────────────────────────────
// §2 Encodings
// ──────────────────────────────────────────────

const B32 = "abcdefghijklmnopqrstuvwxyz234567";
const B32_MAP: Record<string, number> = {};
for (let i = 0; i < B32.length; i++) B32_MAP[B32[i]] = i;

/** RFC 4648 base32, lowercase, no padding */
export function base32Encode(bytes: Buffer): string {
  let out = "";
  let bits = 0;
  let val = 0;
  for (let i = 0; i < bytes.length; i++) {
    val = (val << 8) | bytes[i];
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += B32[(val >>> bits) & 31];
    }
  }
  if (bits > 0) {
    out += B32[(val << (5 - bits)) & 31];
  }
  return out;
}

/** Canonical base32 decode (§2): re-encode and compare. Returns null on any error. */
export function base32DecodeCanonical(s: string): Buffer | null {
  // Must be lowercase, no padding
  if (s !== s.toLowerCase()) return null;
  if (s.includes("=")) return null;
  // All characters must be in the alphabet
  for (const c of s) {
    if (!(c in B32_MAP)) return null;
  }

  let bits = 0;
  let val = 0;
  const out: number[] = [];
  for (let i = 0; i < s.length; i++) {
    val = (val << 5) | B32_MAP[s[i]];
    bits += 5;
    if (bits >= 8) {
      bits -= 8;
      out.push((val >>> bits) & 0xff);
    }
  }

  const result = Buffer.from(out);

  // Canonical check: re-encode and compare (§2)
  if (base32Encode(result) !== s) return null;

  return result;
}

const B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
const B64URL_MAP: Record<string, number> = {};
for (let i = 0; i < B64URL.length; i++) B64URL_MAP[B64URL[i]] = i;

/** RFC 4648 §5 base64url, no padding */
export function base64urlEncode(bytes: Buffer): string {
  let out = "";
  let bits = 0;
  let val = 0;
  for (let i = 0; i < bytes.length; i++) {
    val = (val << 8) | bytes[i];
    bits += 8;
    while (bits >= 6) {
      bits -= 6;
      out += B64URL[(val >>> bits) & 63];
    }
  }
  if (bits > 0) {
    out += B64URL[(val << (6 - bits)) & 63];
  }
  return out;
}

/** Canonical base64url decode (§2). Returns null on any error. */
export function base64urlDecodeCanonical(s: string): Buffer | null {
  // Check all characters are valid base64url, no padding
  // Also reject any newline/whitespace (the alphabet doesn't include them)
  if (s.includes("=")) return null;
  for (const c of s) {
    if (!(c in B64URL_MAP)) return null;
  }

  let bits = 0;
  let val = 0;
  const out: number[] = [];
  for (let i = 0; i < s.length; i++) {
    val = (val << 6) | B64URL_MAP[s[i]];
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out.push((val >>> bits) & 0xff);
    }
  }

  const result = Buffer.from(out);

  // Canonical check: re-encode and compare (§2)
  if (base64urlEncode(result) !== s) return null;

  return result;
}

// ──────────────────────────────────────────────
// §2 Key validity rule
// ──────────────────────────────────────────────

const ED25519_P = BigInt("57896044618658097711785492504343953926634992332820282019728792003956564819949"); // 2^255 - 19
const ED25519_P_MINUS_1 = ED25519_P - 1n;

// Small-order y values (big-endian hex as given in spec → BigInt)
const SMALL_ORDER_Y = new Set<bigint>([
  0n,
  1n,
  ED25519_P_MINUS_1,
  BigInt("0x05fc536d880238b13933c6d305acdfd5f098eff289f4c345b027b2c28f95e826"),
  BigInt("0x7a03ac9277fdc74ec6cc392cfa53202a0f67100d760b3cba4fd84d3d706a17c7"),
]);

/**
 * §2 Key validity rule.
 * The key MUST be canonically encoded and not of small order.
 */
export function isKeyValid(keyBytes: Buffer): boolean {
  if (keyBytes.length !== 32) return false;

  // Read 32 bytes as little-endian integer, clear bit 255 to get y
  let y = 0n;
  for (let i = 31; i >= 0; i--) {
    y = (y << 8n) | BigInt(keyBytes[i]);
  }
  // Clear bit 255 (the MSB of the 32nd byte read as LE = bit 255)
  // In little-endian, bit 255 is the top bit of byte 31
  y = y & ((1n << 255n) - 1n);

  // Reject if y >= p (non-canonical encoding)
  if (y >= ED25519_P) return false;

  // Reject if y is a small-order value
  if (SMALL_ORDER_Y.has(y)) return false;

  return true;
}

// ──────────────────────────────────────────────
// Ed25519 helpers
// ──────────────────────────────────────────────

/** PKCS#8 DER prefix for Ed25519 private key (seed form) */
const PKCS8_PREFIX = Buffer.from("302e020100300506032b657004220420", "hex");

/** Ed25519 group order L (for checking scalar range) */
export const ED25519_L = BigInt("7237005577332262213973186563042994240857116359379907606001950938285454250989");

/** Derive Ed25519 keypair from 32-byte seed */
export function keyFromSeed(seed: Buffer): { publicKey: Buffer; keyId: string; seed: Buffer } {
  // Build PKCS#8 private key: prefix + seed
  const pkcs8 = Buffer.concat([PKCS8_PREFIX, seed]);
  const privateKey = crypto.createPrivateKey({ key: pkcs8, format: "der", type: "pkcs8" });
  const publicKey = crypto.createPublicKey(privateKey).export({ format: "der", type: "spki" });

  // Extract raw 32-byte public key from SPKI
  // SPKI for Ed25519: 302a300506032b6570032100 + 32-byte key
  const rawPub = publicKey.subarray(12); // skip DER prefix
  const keyId = "k_" + base32Encode(rawPub);

  return { publicKey: rawPub, keyId, seed };
}

/** Sign payload bytes with an Ed25519 key derived from seed (deterministic). */
export function clankmailSign(seed: Buffer, payload: Buffer): { token: string; domainSigInput: Buffer } {
  const { publicKey } = keyFromSeed(seed);
  const signerB32 = base32Encode(publicKey);

  // Domain separation: "clankmail/v1\n" || payload_bytes (§3.1)
  const domainPrefix = Buffer.from("clankmail/v1\n", "utf8");
  const sigInput = Buffer.concat([domainPrefix, payload]);

  const pkcs8 = Buffer.concat([PKCS8_PREFIX, seed]);
  const privateKey = crypto.createPrivateKey({ key: pkcs8, format: "der", type: "pkcs8" });
  const sig = crypto.sign(null, sigInput, privateKey);

  const payloadB64 = base64urlEncode(payload);
  const sigB64 = base64urlEncode(sig);

  const token = `v1.${signerB32}.${payloadB64}.${sigB64}`;
  return { token, domainSigInput: sigInput };
}

// ──────────────────────────────────────────────
// §4 Verification pipeline
// ──────────────────────────────────────────────

const DOMAIN_PREFIX = Buffer.from("clankmail/v1\n", "utf8");

const KNOWN_TYPES = new Set([
  "handle", "grant", "enroll", "revoke", "auth",
  "channel", "msg", "join", "leave", "admit", "ban", "remove", "sub",
]);

/**
 * Verify a token string. Steps are ordered as §4 requires.
 * Returns either { ok: true, id, t, from } or { ok: false, error }.
 */
export function verify(token: string): { ok: true; id: string; t: string; from: string; payload: Buffer }
  | { ok: false; error: string } {

  // Step 1: token length ≤ 98304 bytes
  // Bytes, not String.length: that counts UTF-16 code units (vector size-over-multibyte).
  if (Buffer.byteLength(token, "utf8") > 98304) return { ok: false, error: "E_SIZE" };

  // Step 2: exactly 4 `.`-separated fields; first is `v1`
  const parts = token.split(".");
  if (parts.length !== 4) return { ok: false, error: "E_FORMAT" };
  if (parts[0] !== "v1") return { ok: false, error: "E_FORMAT" };

  const signerB32 = parts[1];
  const payloadB64 = parts[2];
  const sigB64 = parts[3];

  // Step 3: decode fields canonically, key validity rule
  const signerBytes = base32DecodeCanonical(signerB32);
  if (!signerBytes) return { ok: false, error: "E_ENCODING" };
  if (signerBytes.length !== 32) return { ok: false, error: "E_ENCODING" };
  if (!isKeyValid(signerBytes)) return { ok: false, error: "E_ENCODING" };

  const payloadBytes = base64urlDecodeCanonical(payloadB64);
  if (!payloadBytes) return { ok: false, error: "E_ENCODING" };

  const sigBytes = base64urlDecodeCanonical(sigB64);
  if (!sigBytes) return { ok: false, error: "E_ENCODING" };
  if (sigBytes.length !== 64) return { ok: false, error: "E_ENCODING" };

  // Step 4: Ed25519 verification (cofactorless)
  if (!verifySignature(signerBytes, payloadBytes, sigBytes)) {
    return { ok: false, error: "E_SIGNATURE" };
  }

  // Step 5: JSON validation
  const jsonCheck = validateClankmailJson(payloadBytes);
  if (!jsonCheck.ok) return { ok: false, error: "E_JSON" };
  const payload = jsonCheck.obj as Record<string, unknown>;

  // Step 6: schema validation (common + type-specific)
  const schemaCheck = validateSchema(payload);
  if (!schemaCheck.ok) return { ok: false, error: "E_SCHEMA" };

  // Step 7: from == "k_" + signer
  const fromField = payload["from"] as string;
  if (fromField !== "k_" + signerB32) return { ok: false, error: "E_SIGNER" };

  // Compute id
  const id = computeObjectId(payloadBytes);

  return { ok: true, id, t: payload["t"] as string, from: fromField, payload: payloadBytes };
}

// ──────────────────────────────────────────────
// Signature verification with additional checks
// ──────────────────────────────────────────────

function verifySignature(signerBytes: Buffer, payload: Buffer, sig: Buffer): boolean {
  // Check S < L (scalar must be below group order)
  const sBytes = sig.subarray(32, 64);
  let s = 0n;
  for (let i = 31; i >= 0; i--) {
    s = (s << 8n) | BigInt(sBytes[i]);
  }
  if (s >= ED25519_L) return false;

  // Build SPKI for the public key
  const spki = buildEd25519Spki(signerBytes);
  const publicKey = crypto.createPublicKey({ key: spki, format: "der", type: "spki" });

  const sigInput = Buffer.concat([DOMAIN_PREFIX, payload]);

  return crypto.verify(null, sigInput, publicKey, sig);
}

/** Build DER SPKI for an Ed25519 public key */
function buildEd25519Spki(pubKey: Buffer): Buffer {
  // SPKI for Ed25519: 302a300506032b6570032100 + 32-byte key
  const prefix = Buffer.from("302a300506032b6570032100", "hex");
  return Buffer.concat([prefix, pubKey]);
}

// ──────────────────────────────────────────────
// §3.3 Object id
// ──────────────────────────────────────────────

export function computeObjectId(payloadBytes: Buffer): string {
  const hash = crypto.createHash("sha256").update(payloadBytes).digest();
  return "m_" + base32Encode(hash);
}

/** Channel id: c_ + same 52 chars as the genesis object's m_ id */
export function computeChannelId(genesisPayloadBytes: Buffer): string {
  const mid = computeObjectId(genesisPayloadBytes);
  return "c_" + mid.substring(2);
}

// ──────────────────────────────────────────────
// §3.2 / §5 JSON validation
// ──────────────────────────────────────────────

type JsonValidateResult = {
  ok: true;
  obj: unknown;
} | {
  ok: false;
}

/**
 * Full JSON validation per the spec:
 * - Valid UTF-8
 * - Parses as one JSON object
 * - No duplicate keys at any depth (including \u-escaped duplicates)
 * - Numbers are integers in [0, 2^53-1], no sign/fraction/exponent/leading-zero
 * - \u escapes of surrogates must be high+low pairs
 * - No nesting deeper than 16 levels (root object is level 1)
 * - No BOM
 * - Only JSON whitespace around the object
 * - No raw control characters (U+0000–U+001F except U+000A) inside strings
 */
function validateClankmailJson(bytes: Buffer): JsonValidateResult {
  // Check for BOM at start
  if (bytes.length >= 3 && bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf) {
    return { ok: false };
  }

  // Parse as UTF-8
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    return { ok: false };
  }

  // Strip leading whitespace (JSON whitespace only: space, tab, newline, carriage return)
  let pos = skipJsonWhitespace(text, 0);

  // Must start with '{'
  if (pos >= text.length || text[pos] !== "{") return { ok: false };

  // Parse and validate recursively
  const result = parseJsonValue(text, pos, 1);
  if (!result.ok) return { ok: false };

  pos = result.nextPos;

  // Skip trailing whitespace
  pos = skipJsonWhitespace(text, pos);

  // Must be at end (no trailing data)
  if (pos !== text.length) return { ok: false };

  return { ok: true, obj: result.value };
}

function skipJsonWhitespace(s: string, pos: number): number {
  while (pos < s.length) {
    const c = s.charCodeAt(pos);
    if (c === 0x20 || c === 0x09 || c === 0x0a || c === 0x0d) {
      pos++;
    } else {
      break;
    }
  }
  return pos;
}

type ParseResult = {
  ok: true;
  value: unknown;
  nextPos: number;
} | {
  ok: false;
}

function parseJsonValue(s: string, pos: number, depth: number): ParseResult {
  if (pos >= s.length) return { ok: false };
  const c = s[pos];

  if (c === "{") return parseJsonObject(s, pos, depth);
  if (c === "[") return parseJsonArray(s, pos, depth);
  if (c === '"') return parseJsonString(s, pos);
  if (c === "t" || c === "f") return parseJsonBool(s, pos);
  if (c === "n") return parseJsonNull(s, pos);
  if (c === "-" || (c >= "0" && c <= "9")) return parseJsonNumber(s, pos);

  return { ok: false };
}

function parseJsonObject(s: string, pos: number, depth: number): ParseResult {
  if (depth > 16) return { ok: false };
  pos++; // skip '{'

  // No prototype: with `{}`, a key named "__proto__" would set the prototype instead of
  // being stored, letting it supply fields (vectors schema-proto-key-supplies-nothing,
  // msg-proto-key-ignored).
  const obj: Record<string, unknown> = Object.create(null);
  const seenKeys = new Map<string, string>(); // normalized key → original key

  pos = skipJsonWhitespace(s, pos);

  if (pos < s.length && s[pos] === "}") {
    return { ok: true, value: obj, nextPos: pos + 1 };
  }

  while (true) {
    pos = skipJsonWhitespace(s, pos);
    if (pos >= s.length || s[pos] !== '"') return { ok: false };

    // Parse key string
    const keyResult = parseJsonString(s, pos);
    if (!keyResult.ok) return { ok: false };
    const key = keyResult.value as string;
    pos = keyResult.nextPos;

    // Normalize key (already done by JSON.parse, but we need to track escaped duplicates)
    // The key from parseJsonString is already the unescaped form
    const keyNormalized = key;

    // Check for duplicate (including through \u escapes)
    // But we already unescaped in parseJsonString, so simple equality works
    if (seenKeys.has(keyNormalized)) return { ok: false };
    seenKeys.set(keyNormalized, key);

    pos = skipJsonWhitespace(s, pos);
    if (pos >= s.length || s[pos] !== ":") return { ok: false };
    pos++; // skip ':'

    pos = skipJsonWhitespace(s, pos);

    const valResult = parseJsonValue(s, pos, depth + 1);
    if (!valResult.ok) return { ok: false };
    obj[keyNormalized] = valResult.value;
    pos = valResult.nextPos;

    pos = skipJsonWhitespace(s, pos);
    if (pos >= s.length) return { ok: false };

    if (s[pos] === "}") {
      return { ok: true, value: obj, nextPos: pos + 1 };
    }
    if (s[pos] !== ",") return { ok: false };
    pos++; // skip ','
  }
}

function parseJsonArray(s: string, pos: number, depth: number): ParseResult {
  if (depth > 16) return { ok: false };
  pos++; // skip '['

  const arr: unknown[] = [];

  pos = skipJsonWhitespace(s, pos);

  if (pos < s.length && s[pos] === "]") {
    return { ok: true, value: arr, nextPos: pos + 1 };
  }

  while (true) {
    pos = skipJsonWhitespace(s, pos);
    const valResult = parseJsonValue(s, pos, depth + 1);
    if (!valResult.ok) return { ok: false };
    arr.push(valResult.value);
    pos = valResult.nextPos;

    pos = skipJsonWhitespace(s, pos);
    if (pos >= s.length) return { ok: false };

    if (s[pos] === "]") {
      return { ok: true, value: arr, nextPos: pos + 1 };
    }
    if (s[pos] !== ",") return { ok: false };
    pos++; // skip ','
  }
}

function parseJsonString(s: string, pos: number): ParseResult {
  if (pos >= s.length || s[pos] !== '"') return { ok: false };
  pos++; // skip opening '"'

  let out = "";
  let hasHighSurrogate = false;

  while (pos < s.length) {
    const cp = s.codePointAt(pos)!;

    // Raw control characters (U+0000–U+001F), line feed included, must be escaped
    // (vectors json-raw-control-char, json-raw-newline).
    if (cp <= 0x1f) return { ok: false };

    if (cp === 0x22) { // '"'
      // Closing quote — must not have an unpaired high surrogate
      if (hasHighSurrogate) return { ok: false };
      return { ok: true, value: out, nextPos: pos + 1 };
    }

    if (cp === 0x5c) { // '\\'
      // Escape sequence
      pos++; // skip backslash
      if (pos >= s.length) return { ok: false };
      const esc = s[pos];
      pos++;
      // A pending high surrogate must be completed by the very next \u escape
      // (vector json-lone-surrogate-split).
      if (esc !== "u" && hasHighSurrogate) return { ok: false };

      switch (esc) {
        case '"': out += '"'; break;
        case "\\": out += "\\"; break;
        case "/": out += "/"; break;
        case "b": out += "\b"; break;
        case "f": out += "\f"; break;
        case "n": out += "\n"; break;
        case "r": out += "\r"; break;
        case "t": out += "\t"; break;
        case "u": {
          // \uXXXX
          if (pos + 4 > s.length) return { ok: false };
          const hex = s.substring(pos, pos + 4);
          if (!/^[0-9a-fA-F]{4}$/.test(hex)) return { ok: false };
          const codeUnit = parseInt(hex, 16);
          pos += 4;

          if (codeUnit >= 0xdc00 && codeUnit <= 0xdfff) {
            // Low surrogate: must be preceded by a high surrogate
            if (!hasHighSurrogate) return { ok: false };
            out += String.fromCharCode(codeUnit);
            hasHighSurrogate = false;
          } else if (codeUnit >= 0xd800 && codeUnit <= 0xdbff) {
            // High surrogate: must be followed by low surrogate
            if (hasHighSurrogate) return { ok: false }; // two highs in a row
            out += String.fromCharCode(codeUnit);
            hasHighSurrogate = true;
          } else {
            // Regular BMP character
            if (hasHighSurrogate) return { ok: false }; // high not followed by low
            out += String.fromCharCode(codeUnit);
          }
          break;
        }
        default:
          return { ok: false };
      }
    } else {
      // Regular character
      if (hasHighSurrogate) return { ok: false }; // high surrogate must be followed by \u escape
      const charLen = cp > 0xffff ? 2 : 1;
      out += s.substring(pos, pos + charLen);
      pos += charLen;
    }
  }

  return { ok: false }; // unterminated string
}

function parseJsonNumber(s: string, pos: number): ParseResult {
  const start = pos;

  // Optional sign: MINUS ONLY, and not for negative numbers (spec says no sign)
  if (s[pos] === "-") {
    // Negative numbers not allowed; -0 also not allowed
    return { ok: false };
  }

  // Must start with digit
  if (pos >= s.length || s[pos] < "0" || s[pos] > "9") return { ok: false };

  // Leading zero check: if first digit is 0, must be exactly "0"
  if (s[pos] === "0") {
    pos++;
    if (pos < s.length && s[pos] >= "0" && s[pos] <= "9") return { ok: false };
  } else {
    // Read digits
    while (pos < s.length && s[pos] >= "0" && s[pos] <= "9") pos++;
  }

  // No fraction
  if (pos < s.length && s[pos] === ".") return { ok: false };

  // No exponent
  if (pos < s.length && (s[pos] === "e" || s[pos] === "E")) return { ok: false };

  const numStr = s.substring(start, pos);
  const num = Number(numStr);

  // Must be in [0, 2^53-1] and representable exactly as integer
  if (!Number.isInteger(num)) return { ok: false };
  if (num < 0 || num > 9007199254740991) return { ok: false };

  return { ok: true, value: num, nextPos: pos };
}

function parseJsonBool(s: string, pos: number): ParseResult {
  if (s.startsWith("true", pos)) {
    return { ok: true, value: true, nextPos: pos + 4 };
  }
  if (s.startsWith("false", pos)) {
    return { ok: true, value: false, nextPos: pos + 5 };
  }
  return { ok: false };
}

function parseJsonNull(s: string, pos: number): ParseResult {
  if (s.startsWith("null", pos)) {
    return { ok: true, value: null, nextPos: pos + 4 };
  }
  return { ok: false };
}

// ──────────────────────────────────────────────
// §6–§7 Schema validation
// ──────────────────────────────────────────────

type SchemaResult = {
  ok: true;
} | {
  ok: false;
}

function validateSchema(obj: Record<string, unknown>): SchemaResult {
  // Common fields
  if (typeof obj.v !== "number" || obj.v !== 1) return { ok: false };
  if (typeof obj.t !== "string" || !KNOWN_TYPES.has(obj.t)) return { ok: false };
  if (typeof obj.from !== "string" || !isKeyIdFormat(obj.from)) return { ok: false };
  if (typeof obj.ts !== "number" || !Number.isInteger(obj.ts) || obj.ts < 0) return { ok: false };

  const t = obj.t;

  switch (t) {
    case "handle": return validateHandle(obj);
    case "grant": return validateGrant(obj);
    case "enroll": return validateEnroll(obj);
    case "revoke": return validateRevoke(obj);
    case "auth": return validateAuth(obj);
    case "channel": return validateChannel(obj);
    case "msg": return validateMsg(obj);
    case "join": return validateJoinLeave(obj, "join");
    case "leave": return validateJoinLeave(obj, "leave");
    case "admit": return validateAdmit(obj);
    case "ban": return validateBan(obj);
    case "remove": return validateRemove(obj);
    case "sub": return validateSub(obj);
    default: return { ok: true }; // Unknown type passes schema (but E_SCHEMA would catch unknown t above)
  }
}

// Key id format: k_ + canonical base32 of 32 bytes
function isKeyIdFormat(s: string): boolean {
  if (!s.startsWith("k_")) return false;
  const decoded = base32DecodeCanonical(s.substring(2));
  return decoded !== null && decoded.length === 32;
}

// Object id format: m_ + canonical base32 of 32 bytes
function isObjectIdFormat(s: string): boolean {
  if (!s.startsWith("m_")) return false;
  const decoded = base32DecodeCanonical(s.substring(2));
  return decoded !== null && decoded.length === 32;
}

// Channel id format: c_ + canonical base32 of 32 bytes
function isChannelIdFormat(s: string): boolean {
  if (!s.startsWith("c_")) return false;
  const decoded = base32DecodeCanonical(s.substring(2));
  return decoded !== null && decoded.length === 32;
}

// Hub name: lowercase DNS name with at least one dot, no port, no trailing dot, ≤ 253 bytes
function isValidHubName(s: string): boolean {
  if (typeof s !== "string") return false;
  if (Buffer.byteLength(s, "utf8") > 253) return false;
  if (s !== s.toLowerCase()) return false;
  if (!s.includes(".")) return false;
  if (s.endsWith(".")) return false;
  if (s.includes(":")) return false;
  // Every label: 1–63 bytes of [a-z0-9-], not starting or ending with a hyphen
  // (vectors schema-hub-empty-label, schema-hub-label-over).
  return s.split(".").every(l => /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(l));
}

// Handle name: ^[a-z0-9][a-z0-9-]{1,30}$
function isValidHandleName(s: string): boolean {
  if (typeof s !== "string") return false;
  // Check no trailing newline (spec says: trailing newline is never part of a match)
  if (s.includes("\n")) return false;
  return /^[a-z0-9][a-z0-9-]{1,30}$/.test(s);
}

// Grant/enroll agent name: ^[a-z0-9][a-z0-9-]{0,31}$
function isValidAgentName(s: string): boolean {
  if (typeof s !== "string") return false;
  return /^[a-z0-9][a-z0-9-]{0,31}$/.test(s);
}

function validateHandle(obj: Record<string, unknown>): SchemaResult {
  if (!isValidHandleName(obj.name as string)) return { ok: false };
  if (!isValidHubName(obj.hub as string)) return { ok: false };
  return { ok: true };
}

function validateGrant(obj: Record<string, unknown>): SchemaResult {
  if (typeof obj.agent !== "string" || !isKeyIdFormat(obj.agent)) return { ok: false };
  if (!isValidAgentName(obj.name as string)) return { ok: false };
  if (!isValidHubName(obj.hub as string)) return { ok: false };
  if (obj.exp !== undefined) {
    if (typeof obj.exp !== "number" || !Number.isInteger(obj.exp) || obj.exp < 0) return { ok: false };
  }
  return { ok: true };
}

function validateEnroll(obj: Record<string, unknown>): SchemaResult {
  if (typeof obj.grant !== "string" || !isObjectIdFormat(obj.grant)) return { ok: false };
  return { ok: true };
}

function validateRevoke(obj: Record<string, unknown>): SchemaResult {
  if (typeof obj.target !== "string" || !isObjectIdFormat(obj.target)) return { ok: false };
  return { ok: true };
}

function validateAuth(obj: Record<string, unknown>): SchemaResult {
  if (!isValidHubName(obj.hub as string)) return { ok: false };
  if (typeof obj.method !== "string" || !/^[A-Z]{1,16}$/.test(obj.method)) return { ok: false };
  if (typeof obj.path !== "string") return { ok: false };
  if (!(obj.path as string).startsWith("/")) return { ok: false };
  if (Buffer.byteLength(obj.path as string, "utf8") > 2048) return { ok: false };
  if (typeof obj.body !== "string") return { ok: false };
  // body: lowercase hex SHA-256 (64 hex chars)
  if (!/^[0-9a-f]{64}$/.test(obj.body as string)) return { ok: false };
  if (obj.n !== undefined) {
    if (typeof obj.n !== "string") return { ok: false };
    const nLen = Buffer.byteLength(obj.n as string, "utf8");
    if (nLen < 1 || nLen > 64) return { ok: false };
  }
  return { ok: true };
}

function validateChannel(obj: Record<string, unknown>): SchemaResult {
  // slug: ^[a-z0-9][a-z0-9-]{1,31}$
  if (typeof obj.slug !== "string" || !/^[a-z0-9][a-z0-9-]{1,31}$/.test(obj.slug)) return { ok: false };
  // title: 1–80 bytes
  if (typeof obj.title !== "string") return { ok: false };
  const titleLen = Buffer.byteLength(obj.title as string, "utf8");
  if (titleLen < 1 || titleLen > 80) return { ok: false };
  // about: optional, 0–1000 bytes (but empty is allowed? spec says 0-1000)
  if (obj.about !== undefined) {
    if (typeof obj.about !== "string") return { ok: false };
    const aboutLen = Buffer.byteLength(obj.about as string, "utf8");
    if (aboutLen > 1000) return { ok: false };
  }
  // tags: optional, 0–8 unique strings matching ^[a-z0-9][a-z0-9-]{0,23}$
  if (obj.tags !== undefined) {
    if (!Array.isArray(obj.tags)) return { ok: false };
    if (obj.tags.length > 8) return { ok: false };
    const seen = new Set<string>();
    for (const tag of obj.tags) {
      if (typeof tag !== "string") return { ok: false };
      if (!/^[a-z0-9][a-z0-9-]{0,23}$/.test(tag)) return { ok: false };
      if (seen.has(tag)) return { ok: false };
      seen.add(tag);
    }
  }
  // read: "public" or "members"
  if (typeof obj.read !== "string" || (obj.read !== "public" && obj.read !== "members")) return { ok: false };
  // post: "owner", "mods", "members" or "anyone"
  if (typeof obj.post !== "string" || !["owner", "mods", "members", "anyone"].includes(obj.post)) return { ok: false };
  // join: "open" or "approval"
  if (typeof obj.join !== "string" || (obj.join !== "open" && obj.join !== "approval")) return { ok: false };
  // mods: optional, 0–16 unique key ids
  if (obj.mods !== undefined) {
    if (!Array.isArray(obj.mods)) return { ok: false };
    if (obj.mods.length > 16) return { ok: false };
    const seenMods = new Set<string>();
    for (const mod of obj.mods) {
      if (typeof mod !== "string" || !isKeyIdFormat(mod)) return { ok: false };
      if (seenMods.has(mod)) return { ok: false };
      seenMods.add(mod);
    }
  }
  // channel: optional, c_ id (present on updates, absent on genesis)
  if (obj.channel !== undefined) {
    if (typeof obj.channel !== "string" || !isChannelIdFormat(obj.channel)) return { ok: false };
  }
  return { ok: true };
}

function validateMsg(obj: Record<string, unknown>): SchemaResult {
  // to: must be an array
  if (!Array.isArray(obj.to)) return { ok: false };
  const to = obj.to as unknown[];

  // Must have 1–32 entries
  if (to.length < 1 || to.length > 32) return { ok: false };

  // All entries must be unique
  const seenTo = new Set<string>();
  let hasChannel = false;
  let hasKey = false;
  for (const item of to) {
    if (typeof item !== "string") return { ok: false };
    if (seenTo.has(item)) return { ok: false };
    seenTo.add(item);

    const isKey = isKeyIdFormat(item);
    const isChan = isChannelIdFormat(item);

    if (!isKey && !isChan) return { ok: false };
    if (isChan) {
      hasChannel = true;
      // If there's a channel, it must be exactly one and no keys
      if (to.length > 1) return { ok: false };
    }
    if (isKey) hasKey = true;
  }
  // Can't have both keys and channels
  if (hasChannel && hasKey) return { ok: false };

  // body: 1–32768 bytes
  if (typeof obj.body !== "string") return { ok: false };
  const bodyLen = Buffer.byteLength(obj.body as string, "utf8");
  if (bodyLen < 1 || bodyLen > 32768) return { ok: false };

  // subject: optional, 1–200 bytes
  if (obj.subject !== undefined) {
    if (typeof obj.subject !== "string") return { ok: false };
    const subjLen = Buffer.byteLength(obj.subject as string, "utf8");
    if (subjLen < 1 || subjLen > 200) return { ok: false };
  }

  // re: optional m_ id
  if (obj.re !== undefined) {
    if (typeof obj.re !== "string" || !isObjectIdFormat(obj.re)) return { ok: false };
  }

  return { ok: true };
}

function validateJoinLeave(obj: Record<string, unknown>, _type: string): SchemaResult {
  // channel: c_ id
  if (typeof obj.channel !== "string" || !isChannelIdFormat(obj.channel)) return { ok: false };
  return { ok: true };
}

function validateAdmit(obj: Record<string, unknown>): SchemaResult {
  if (typeof obj.channel !== "string" || !isChannelIdFormat(obj.channel)) return { ok: false };
  if (typeof obj.member !== "string" || !isKeyIdFormat(obj.member)) return { ok: false };
  return { ok: true };
}

function validateBan(obj: Record<string, unknown>): SchemaResult {
  if (typeof obj.channel !== "string" || !isChannelIdFormat(obj.channel)) return { ok: false };
  if (typeof obj.member !== "string" || !isKeyIdFormat(obj.member)) return { ok: false };
  if (obj.until !== undefined) {
    if (typeof obj.until !== "number" || !Number.isInteger(obj.until)) return { ok: false };
  }
  return { ok: true };
}

function validateRemove(obj: Record<string, unknown>): SchemaResult {
  if (typeof obj.channel !== "string" || !isChannelIdFormat(obj.channel)) return { ok: false };
  if (typeof obj.target !== "string" || !isObjectIdFormat(obj.target)) return { ok: false };
  return { ok: true };
}

function validateSub(obj: Record<string, unknown>): SchemaResult {
  if (typeof obj.channel !== "string" || !isChannelIdFormat(obj.channel)) return { ok: false };
  return { ok: true };
}

// ──────────────────────────────────────────────
// §8.1 Digest — counting, ranking and caps
// ──────────────────────────────────────────────

/** A post as seen by the digest function. */
export interface DigestPost {
  id: string;
  channel: string;
  from: string;
  ts: number;
  re?: string;
  /** Whether the post is in the window (and therefore may appear in items). */
  window?: boolean;
  /** Whether the post is hidden by remove (§7.3). Hidden replies do not count. */
  hidden?: boolean;
  /** Whether the post's author is under a live ban in its channel. Bans do not change counts. */
  banned?: boolean;
}

/** A single item in the digest result. */
export interface DigestItem {
  id: string;
  repliers: number;
  replies: number;
}

/** The result of the digest computation. */
export interface DigestResult {
  items: DigestItem[];
  omitted: number;
}

/**
 * §8.1 Digest — a pure, deterministic function of the posts and caps.
 *
 * Inputs: a set of posts (each with id, channel, from, ts, optional re, window,
 * hidden and banned), limit and per_channel.
 *
 * Returns the ranked items (id, repliers, replies) and omitted count.
 */
export function computeDigest(posts: DigestPost[], limit: number, perChannel: number): DigestResult {
  // Separate window posts from non-window posts.
  const windowPosts = posts.filter(p => p.window === true);
  const allPosts = posts;

  // For each window post, compute reply counts from ALL posts (window + non-window).
  // A reply must be:
  // 1. in the SAME channel
  // 2. not hidden by remove
  // 3. have re equal to the window post's id
  // 4. have from differ from the window post's from (self-reply counts for nothing)
  //
  // Bans do not change counts.
  const counts = new Map<string, { repliers: Set<string>; replies: number }>();

  for (const wp of windowPosts) {
    const repliers = new Set<string>();
    let replyCount = 0;

    for (const p of allPosts) {
      // Same channel?
      if (p.channel !== wp.channel) continue;
      // Not hidden?
      if (p.hidden === true) continue;
      // re must match
      if (p.re !== wp.id) continue;
      // Self-reply doesn't count
      if (p.from === wp.from) continue;

      replyCount++;
      repliers.add(p.from);
    }

    counts.set(wp.id, { repliers, replies: replyCount });
  }

  // Ranking: repliers descending, replies descending, ts descending (newer first),
  // then id ascending compared as a byte string.
  const ranked = windowPosts.slice().sort((a, b) => {
    const ca = counts.get(a.id)!;
    const cb = counts.get(b.id)!;

    // repliers descending
    if (ca.repliers.size !== cb.repliers.size) return cb.repliers.size - ca.repliers.size;
    // replies descending
    if (ca.replies !== cb.replies) return cb.replies - ca.replies;
    // ts descending (newer first)
    if (a.ts !== b.ts) return b.ts - a.ts;
    // id ascending, compared as byte strings
    if (a.id < b.id) return -1;
    if (a.id > b.id) return 1;
    return 0;
  });

  // Caps: walk the sorted window from the top. Select a post unless per_channel
  // posts from its channel are already selected, and stop once limit posts are
  // selected.
  const selectedByChannel = new Map<string, number>();
  const items: DigestItem[] = [];

  for (const wp of ranked) {
    if (items.length >= limit) break;

    const channelCount = selectedByChannel.get(wp.channel) ?? 0;
    if (channelCount >= perChannel) continue;

    const c = counts.get(wp.id)!;
    items.push({ id: wp.id, repliers: c.repliers.size, replies: c.replies });
    selectedByChannel.set(wp.channel, channelCount + 1);
  }

  const omitted = windowPosts.length - items.length;

  return { items, omitted };
}
