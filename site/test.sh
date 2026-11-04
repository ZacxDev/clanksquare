#!/usr/bin/env bash
# Smoke test for the Clanksquare site image (site/Dockerfile).
#
#   bash site/test.sh              # stub hubs only
#   CHECK_LIVE=1 bash site/test.sh # also load the production page against the real hub
#
# Builds the production image and checks its headers, its pinned hub and its
# paths. Then builds the page twice more with HUB_URL pointed at a stub hub
# (an nginx container): once at a port that sends Access-Control-Allow-Origin:
# *, once at a port that sends none (the negative control), and checks both in
# headless Chromium. Needs docker and python3; the browser checks use
# Playwright (nix-shell with python3Packages.playwright +
# playwright-driver.browsers, else a host python3 Playwright with its
# browsers installed). Without either they are reported and the test fails,
# unless ALLOW_NO_BROWSER=1.
#
# Prints one "ok"/"not ok" line per check and a count; exits 0 only if every
# check passed.
set -euo pipefail

here="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null && pwd)"
root="$(dirname "$here")"
id="cs-site-test-$$-$RANDOM"
work="$(mktemp -d)"
prod_image="clanksquare-site:test-$$-prod"
cors_image="clanksquare-site:test-$$-cors"
nocors_image="clanksquare-site:test-$$-nocors"
mcp_image="clanksquare-mcp:test-$$"
stub="$id-stub"
containers=("$stub")
stub_image="nginxinc/nginx-unprivileged:1.29-alpine@sha256:0c79d56aee561a1d81c63f00eee5fb5fe29279560cdc55e91425133104c7fbe6"
PROD_HUB="https://clankmail.zacx.dev"

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok - $1"; }
notok() { fail=$((fail + 1)); echo "not ok - $1"; }
check() { if eval "$1"; then ok "$2"; else notok "$2"; fi; }

cleanup() {
  docker rm -f "${containers[@]}" >/dev/null 2>&1 || true
  docker image rm "$prod_image" "$cors_image" "$nocors_image" "$mcp_image" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

# run_site <name> <image>: start a read-only site container; its base URL is
# left in $site_url (not printed: a $(...) subshell would lose the container
# from the cleanup list).
run_site() {
  local name="$id-$1"
  containers+=("$name")
  docker run -d --name "$name" --read-only --tmpfs /tmp:uid=101,gid=101 \
    --cap-drop ALL --security-opt no-new-privileges -p 127.0.0.1::8080 "$2" >/dev/null
  local port
  port="$(docker port "$name" 8080/tcp | head -1 | sed 's/.*://')"
  for _ in $(seq 1 50); do curl -fs "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
  site_url="http://127.0.0.1:$port"
}

# ── the production image ────────────────────────────────────────────
docker build -q -f "$root/site/Dockerfile" -t "$prod_image" "$root" >/dev/null
run_site prod "$prod_image"; base="$site_url"

H="$work/h"; B="$work/b"
# A curl error must fail a check, not end the script.
get() { curl -s -o "$B" -D "$H" -w '%{http_code}' "$@" || echo " curl-exit-$?"; }
hdr() { { grep -i "^$1:" "$H" || true; } | head -1 | cut -d' ' -f2- | tr -d '\r'; }

code="$(get "$base/")"
check '[ "$code" = 200 ]' "GET / is 200 (got $code)"
csp="$(hdr Content-Security-Policy)"
check '[[ "$csp" == *"default-src '"'self'"'"* ]]' "CSP default-src 'self'"
check '[[ "$csp" == *"frame-ancestors '"'none'"'"* ]]' "CSP frame-ancestors 'none'"
check '[[ "$csp" != *unsafe-inline* && "$csp" != *unsafe-eval* ]]' "CSP has no unsafe-inline / unsafe-eval"
hashes="$(python3 - "$here/index.html" <<'EOF'
import base64, hashlib, re, sys
s = open(sys.argv[1], encoding="utf-8").read()
for tag in ("script", "style"):
    blocks = re.findall(r"<%s>(.*?)</%s>" % (tag, tag), s, re.S)
    assert len(blocks) == 1, (tag, len(blocks))
    print(tag + " 'sha256-" + base64.b64encode(hashlib.sha256(blocks[0].encode()).digest()).decode() + "'")
EOF
)"
script_hash="$(sed -n 's/^script //p' <<<"$hashes")"
style_hash="$(sed -n 's/^style //p' <<<"$hashes")"
check '[[ "$csp" == *"script-src $script_hash;"* ]]' "CSP script-src is the hash of the inline <script> ($script_hash)"
check '[[ "$csp" == *"style-src $style_hash;"* ]]' "CSP style-src is the hash of the inline <style> ($style_hash)"
check '[[ "$csp" == *"connect-src '"'self'"' $PROD_HUB;"* ]]' "the production CSP connect-src is the site itself and $PROD_HUB, nothing else"
meta="$(grep -o '<meta name="clanksquare-hub" content="[^"]*">' "$B" | sed 's/.*content="\([^"]*\)".*/\1/')"
check '[ "$meta" = "$PROD_HUB" ]' "the production page fetches from $PROD_HUB (got '$meta')"
check '! grep -q "__HUB_URL__" "$B"' "no placeholder left in the served page"
check '[ "$(hdr X-Content-Type-Options)" = nosniff ]' "X-Content-Type-Options: nosniff"
check '[ "$(hdr Referrer-Policy)" = no-referrer ]' "Referrer-Policy: no-referrer"
check '[ "$(hdr X-Frame-Options)" = DENY ]' "X-Frame-Options: DENY"
check '[ "$(hdr Cache-Control)" = no-transform ]' "Cache-Control: no-transform (Cloudflare must not inject its beacon)"
check '! grep -qi "^set-cookie:" "$H"' "GET / sets no cookie"
check '! grep -qi "^access-control-" "$H"' "the site itself sends no CORS headers"

# The files for agents: exact bytes, exact types, and the same headers.
for spec in "agents.md|text/markdown; charset=utf-8|site/agents.md" "llms.txt|text/plain; charset=utf-8|site/llms.txt" "clank.mjs|text/javascript; charset=utf-8|agent/clank.mjs"; do
  IFS='|' read -r file ctype src <<<"$spec"
  code="$(get "$base/$file")"
  check '[ "$code" = 200 ] && cmp -s "$B" "$root/$src"' "GET /$file is 200 and serves $src byte for byte (got $code)"
  check '[ "$(hdr Content-Type)" = "$ctype" ]' "  ... Content-Type: $ctype (got '$(hdr Content-Type)')"
  check '[ -n "$(hdr Content-Security-Policy)" ] && [ "$(hdr Cache-Control)" = no-transform ] && [ "$(hdr X-Content-Type-Options)" = nosniff ]' "  ... with the CSP, Cache-Control: no-transform and nosniff"
  code="$(get -X POST --data x "$base/$file")"
  check '[ "$code" = 405 ]' "  ... POST /$file is 405 (got $code)"
done

# The served agent guide carries the onboarding interview and its commands
# (their wording is pinned by e2e/agent_docs_test.go), and llms.txt points to it.
get "$base/agents.md" >/dev/null
check 'grep -qx "## Onboarding interview" "$B" && grep -qx "1. Which topics are you interested in?" "$B" && grep -q "^node clank.mjs interests set --topics " "$B" && grep -qx "node clank.mjs recs" "$B"' "GET /agents.md has the onboarding interview (questions, interests set, recs)"
get "$base/llms.txt" >/dev/null
check 'grep -q "onboarding interview" "$B"' "GET /llms.txt mentions the onboarding interview"

# The icon: exact bytes, exact types, and the same headers.
for spec in "icon.svg|image/svg+xml" "icon-512.png|image/png"; do
  IFS='|' read -r file ctype <<<"$spec"
  code="$(get "$base/$file")"
  check '[ "$code" = 200 ] && cmp -s "$B" "$here/$file" && [ "$(wc -c <"$B")" -gt 400 ]' "GET /$file is 200 and serves site/$file byte for byte, $(wc -c <"$B") bytes (got $code)"
  check '[ "$(hdr Content-Type)" = "$ctype" ]' "  ... Content-Type: $ctype (got '$(hdr Content-Type)')"
  check '[ -n "$(hdr Content-Security-Policy)" ] && [ "$(hdr Cache-Control)" = no-transform ] && [ "$(hdr X-Content-Type-Options)" = nosniff ]' "  ... with the CSP, Cache-Control: no-transform and nosniff"
  code="$(get -X POST --data x "$base/$file")"
  check '[ "$code" = 405 ]' "  ... POST /$file is 405 (got $code)"
done
code="$(get "$base/")"
check 'grep -qF "<link rel=\"icon\" href=\"/icon.svg\" type=\"image/svg+xml\">" "$B" && grep -qF "<link rel=\"apple-touch-icon\" href=\"/icon-512.png\">" "$B" && ! grep -qF "href=\"data:" "$B"' "the page's icon link is /icon.svg (apple-touch-icon /icon-512.png)"

for path in /api/channels "/api/channels?sort=active" /api/anything-else /api/ /v1/channels /index.html /nope /.git/config /agents /clank.js /agent/clank.mjs /mcp/x /favicon.ico /icon.png /icon-192.png /site/icon.svg /icon.svg/ /icon.svgx /icon-512.pngx /icon-512.png/x; do
  code="$(get "$base$path")"
  check '[ "$code" = 404 ]' "GET $path is 404 (got $code)"
  check '[ -n "$(hdr Content-Security-Policy)" ]' "  ... and still carries the CSP"
done
code="$(get -X POST --data x "$base/")"
check '[ "$code" = 405 ]' "POST / is 405 (got $code)"
code="$(get "$base/healthz")"
check '[ "$code" = 200 ] && [ "$(cat "$B")" = ok ]' "GET /healthz is 200 ok"
check '! grep -nE "innerHTML|outerHTML|insertAdjacentHTML|document\.write|\.html\(|eval\(|new Function" "$here/index.html"' "index.html has no innerHTML/outerHTML/insertAdjacentHTML/document.write/eval"
check 'grep -q "__HUB_URL__" "$here/index.html" && grep -q "connect-src '"'self'"' __HUB_URL__;" "$here/nginx.conf" && ! grep -q "$PROD_HUB" "$here/index.html" "$here/nginx.conf"' \
  "the hub URL is set only in site/Dockerfile (index.html and nginx.conf carry the placeholder)"

# Build arguments that are not what they claim are refused at build time, and
# refused BY THE GUARD (its own message), not by a later step failing by
# accident: a newline value, for one, also breaks sed.
NL=$'\n'
LAUNCH_KEY="k_ildi4gc4czoegaf4b526pmc2pmymrj7tqm5zbp7w7pvd57s3zvya"
# The guard prints "<ARG> must be ..., got [<value>]" to stderr. In the plain
# build log the RUN step's header echoes the whole command, build args
# EXPANDED, so the message text appears there whether or not the guard fired.
# Only the process's own output lines carry a "#<step> <seconds> " prefix, so
# the check reads those lines alone.
refused() { # refused <ARG> <value>
  local log="$work/build.log"
  if docker build --progress=plain -f "$root/site/Dockerfile" --build-arg "$1=$2" -t "$id-bad" "$root" >"$log" 2>&1; then
    docker image rm "$id-bad" >/dev/null 2>&1 || true
    notok "$1=$(printf %q "$2") is refused at build time (it built)"
  elif grep -E '^#[0-9]+ [0-9]+\.[0-9]+ ' "$log" | grep -qF -- "$1 must be"; then
    ok "$1=$(printf %q "$2") is refused at build time by its guard"
  else
    notok "$1=$(printf %q "$2") is refused, but not by its guard: $(grep -m1 -iE 'error|unterminated' "$log" | cut -c1-120)"
  fi
}
for bad in "https://x.example/path" "https://x.example https://evil.example" "https://x.example${NL}; script-src *" "https://x.example${NL}"; do
  refused HUB_URL "$bad"
done
for bad in "localhost" "127.0.0.1:8090/x" "a b:1" "127.0.0.1:8090${NL}x"; do
  refused MCP_UPSTREAM "$bad"
done
for bad in "k_short" "$LAUNCH_KEY," "$LAUNCH_KEY,,$LAUNCH_KEY" "K_ILDI4GC4CZOEGAF4B526PMC2PMYMRJ7TQM5ZBP7W7PVD57S3ZVYA" "$LAUNCH_KEY${NL}x" "$LAUNCH_KEY\"><script>" ""; do
  refused LAUNCH_OWNERS "$bad"
done

# ── the hosted MCP sidecar, through the site's nginx ───────────────
# MCP 2026-07-28: every request carries _meta (protocol version, client
# capabilities) and the MCP-Protocol-Version / Mcp-Method (/ Mcp-Name) headers.
MCP_META='"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}'
mcp_rpc() { # mcp_rpc <base> <method> <json> [Mcp-Name]
  local extra=()
  [ -n "${4:-}" ] && extra=(-H "Mcp-Name: $4")
  get -X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H 'MCP-Protocol-Version: 2026-07-28' -H "Mcp-Method: $2" "${extra[@]}" --data "$3" "$1/mcp"
}
DISCOVER='{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{'"$MCP_META"'}}'
# Negative control: before the sidecar runs, /mcp is nginx's 502.
code="$(mcp_rpc "$base" server/discover "$DISCOVER")"
check '[ "$code" = 502 ]' "POST /mcp with no sidecar running is 502 (got $code)"
code="$(get "$base/api/latest?c=bad")"
check '[ "$code" = 502 ]' "GET /api/latest with no sidecar running is 502 (got $code)"
docker build -q -f "$root/site/Dockerfile" --target mcp -t "$mcp_image" "$root" >/dev/null
mcp_name="$id-mcp"
containers+=("$mcp_name")
docker run -d --name "$mcp_name" --read-only --cap-drop ALL --security-opt no-new-privileges \
  --network "container:$id-prod" "$mcp_image" >/dev/null
for _ in $(seq 1 50); do
  [ "$(mcp_rpc "$base" server/discover "$DISCOVER")" = 200 ] && break; sleep 0.2
done
code="$(mcp_rpc "$base" server/discover "$DISCOVER")"
check '[ "$code" = 200 ] && python3 -c "import json,sys; r=json.load(open(sys.argv[1]))[\"result\"]; assert r[\"resultType\"]==\"complete\" and r[\"supportedVersions\"]==[\"2026-07-28\",\"2025-11-25\",\"2025-06-18\"] and r[\"_meta\"][\"io.modelcontextprotocol/serverInfo\"][\"name\"]==\"clanksquare\"" "$B"' "POST /mcp server/discover reaches the sidecar (got $code: $(head -c 120 "$B"))"
check '[[ "$(hdr Content-Type)" == application/json* ]] && [ "$(hdr Cache-Control)" = no-transform ] && [ -n "$(hdr Content-Security-Policy)" ]' "  ... JSON, with the site's headers"
code="$(mcp_rpc "$base" tools/list '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{'"$MCP_META"'}}')"
check '[ "$code" = 200 ] && [ "$(python3 -c "import json,sys; print(\" \".join(t[\"name\"] for t in json.load(open(sys.argv[1]))[\"result\"][\"tools\"]))" "$B")" = "clank_channels clank_read" ]' "the hosted MCP lists only clank_channels and clank_read ($(head -c 200 "$B" | tr -d '\n'))"
code="$(mcp_rpc "$base" tools/call '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"clank_post","arguments":{"channel":"#x","text":"x"},'"$MCP_META"'}}' clank_post)"
check '[ "$code" = 200 ] && grep -q "\"code\":-32602" "$B"' "the hosted MCP refuses clank_post as an unknown tool"
# The Mcp-Method header reaches the sidecar through nginx: a mismatch is its 400 HeaderMismatch.
code="$(mcp_rpc "$base" tools/list "$DISCOVER")"
check '[ "$code" = 400 ] && grep -q "\"code\":-32020" "$B"' "an Mcp-Method that does not match the body is 400 HeaderMismatch through nginx (got $code)"
# /api/latest (the landing page's previews) through nginx to the sidecar.
code="$(get "$base/api/latest?c=bad")"
check '[ "$code" = 400 ] && grep -q "c_ channel ids" "$B"' "GET /api/latest with a bad id reaches the sidecar and is refused 400 (got $code: $(head -c 80 "$B"))"
check '[[ "$(hdr Content-Type)" == application/json* ]] && [ "$(hdr Cache-Control)" = no-transform ] && [ -n "$(hdr Content-Security-Policy)" ]' "  ... JSON, with the site's headers"
code="$(get -X POST --data x "$base/api/latest?c=bad")"
check '[ "$code" = 405 ]' "POST /api/latest is 405 at nginx (got $code)"
if [ "${CHECK_LIVE:-0}" = 1 ]; then
  live_cid="$(curl -fsS -A clanksquare-site-test "$PROD_HUB/v1/channels/%23llm-papers" | python3 -c 'import json,sys; print(json.load(sys.stdin)["channel"])')"
  code="$(get "$base/api/latest?c=$live_cid")"
  check '[ "$code" = 200 ] && python3 -c "import json,sys; it=json.load(open(sys.argv[1]))[\"items\"][0]; assert it[\"title\"] and it[\"slug\"]==\"llm-papers\"" "$B" && [ "$(hdr Cache-Control)" = no-transform ]' "live: /api/latest returns the newest #llm-papers post, Cache-Control no-transform only (got $code: $(head -c 120 "$B"))"
fi
# MCP 2025-11-25 through nginx: initialize with no MCP headers, then a
# notification (202) and tools/list with the negotiated header.
code="$(get -X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' --data '{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}' "$base/mcp")"
check '[ "$code" = 200 ] && grep -q "\"protocolVersion\":\"2025-11-25\"" "$B" && [ -z "$(hdr Mcp-Session-Id)" ]' "MCP 2025-11-25 initialize through nginx answers 2025-11-25, with no session id (got $code: $(head -c 100 "$B"))"
code="$(get -X POST -H 'Content-Type: application/json' -H 'MCP-Protocol-Version: 2025-11-25' --data '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$base/mcp")"
check '[ "$code" = 202 ]' "MCP 2025-11-25 notifications/initialized is 202 (got $code)"
code="$(get -X POST -H 'Content-Type: application/json' -H 'MCP-Protocol-Version: 2025-11-25' --data '{"jsonrpc":"2.0","id":6,"method":"tools/list"}' "$base/mcp")"
check '[ "$code" = 200 ] && [ "$(python3 -c "import json,sys; print(\" \".join(t[\"name\"] for t in json.load(open(sys.argv[1]))[\"result\"][\"tools\"]))" "$B")" = "clank_channels clank_read" ]' "MCP 2025-11-25 tools/list through nginx lists clank_channels and clank_read"
# The server-wide body limit is 1k; /mcp raises it to 16k. A 4 KB request
# passes (so the raise applies), a 20 KB one is refused.
pad() { python3 -c 'import json,sys; print(json.dumps({"jsonrpc":"2.0","id":4,"method":"server/discover","params":{"pad":"x"*int(sys.argv[1]),"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}))' "$1"; }
code="$(mcp_rpc "$base" server/discover "$(pad 4000)")"
check '[ "$code" = 200 ] && grep -q "\"supportedVersions\"" "$B"' "a 4 KB POST /mcp is answered (got $code)"
code="$(mcp_rpc "$base" server/discover "$(pad 20000)")"
check '[ "$code" = 413 ]' "a 20 KB POST /mcp is refused 413 (got $code)"
code="$(get "$base/mcp")"
check '[ "$code" = 405 ]' "GET /mcp is 405: no GET stream (got $code)"
# CORS: the image runs the sidecar with --allow-origin '*'. A foreign Origin
# is served with Access-Control-Allow-Origin: * (once: nginx adds none of its
# own), and the preflight OPTIONS reaches the sidecar through nginx.
FOREIGN="https://foreign.example"
acao_count() { { grep -ci '^access-control-allow-origin:' "$H" || true; }; }
code="$(get -X POST -H "Origin: $FOREIGN" -H 'Content-Type: application/json' -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: server/discover' --data "$DISCOVER" "$base/mcp")"
check '[ "$code" = 200 ] && grep -q "\"supportedVersions\"" "$B" && [ "$(hdr Access-Control-Allow-Origin)" = "*" ] && [ "$(acao_count)" = 1 ]' "POST /mcp from Origin $FOREIGN is 200 with one Access-Control-Allow-Origin: * (got $code, '$(hdr Access-Control-Allow-Origin)' x$(acao_count))"
check '[ -z "$(hdr Access-Control-Allow-Credentials)" ] && [ -n "$(hdr Content-Security-Policy)" ] && [ "$(hdr Cache-Control)" = no-transform ]' "  ... no Allow-Credentials, and the site's headers"
code="$(get -X OPTIONS -H "Origin: $FOREIGN" -H 'Access-Control-Request-Method: POST' -H 'Access-Control-Request-Headers: content-type,mcp-method,mcp-name,mcp-protocol-version' "$base/mcp")"
allow_h="$(hdr Access-Control-Allow-Headers | tr 'A-Z' 'a-z')"
check '[ "$code" = 204 ] && [ "$(hdr Access-Control-Allow-Origin)" = "*" ] && [ "$(acao_count)" = 1 ] && [[ "$(hdr Access-Control-Allow-Methods)" == *POST* ]]' "OPTIONS /mcp (CORS preflight) through nginx is 204 with Access-Control-Allow-Origin: * and POST (got $code, '$(hdr Access-Control-Allow-Origin)', '$(hdr Access-Control-Allow-Methods)')"
check '( for h in content-type mcp-protocol-version mcp-method mcp-name; do [[ ",${allow_h// /}," == *",$h,"* ]] || exit 1; done )' "  ... Access-Control-Allow-Headers covers content-type, mcp-protocol-version, mcp-method, mcp-name ('$allow_h')"
check '[ -n "$(hdr Access-Control-Max-Age)" ]' "  ... with an Access-Control-Max-Age"
# Controls: no Origin, no CORS header; /api/latest sends none even with one.
code="$(mcp_rpc "$base" server/discover "$DISCOVER")"
check '[ "$code" = 200 ] && [ "$(acao_count)" = 0 ]' "POST /mcp with no Origin carries no Access-Control-Allow-Origin (got $code)"
code="$(get -H "Origin: $FOREIGN" "$base/api/latest?c=bad")"
check '[ "$code" = 400 ] && [ "$(acao_count)" = 0 ]' "GET /api/latest from Origin $FOREIGN carries no Access-Control-Allow-Origin (got $code)"

# ── stub hubs ───────────────────────────────────────────────────────
XSS='<img src=x onerror=alert(1)>'
python3 - "$work" "$XSS" <<'EOF'
import base64, json, sys
work, xss = sys.argv[1], sys.argv[2]
LAUNCH = "k_ildi4gc4czoegaf4b526pmc2pmymrj7tqm5zbp7w7pvd57s3zvya"   # the curator (site/Dockerfile default)
OTHER = "k_" + "o" * 52                                             # anyone else
def tok(m):
    p = base64.urlsafe_b64encode(json.dumps(m).encode()).rstrip(b"=").decode()
    return "v1.%s.%s.c2ln" % (m["from"], p)
def ch(cid, members, posts, frm, slug, title, about, tags):
    return {"channel": cid, "members": members, "posts_7d": posts, "manifest": tok(
        {"v": 1, "t": "channel", "from": frm, "slug": slug, "title": title, "about": about, "tags": tags})}
# Every character the page must strip, once each (bidi, zero-width, line/para
# separators, BOM, soft hyphen, invisible operators, C0 and C1 controls).
INVISIBLE = ("\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u200b\u200c\u200d\u200e\u200f"
             "\u2028\u2029\ufeff\u00ad\u2060\u2061\u2062\u2063\u2064\u0000\u0001\u0009\u000a\u001f\u007f\u0080\u009f")
items = [
    ch("c_hostile", 3, 7, OTHER, xss, xss, "about " + xss, ["x"]),
    ch("c_smoke", 13, 17, OTHER, "smoke-hidden", "smoke", "", ["smoke"]),
    ch("c_smoke_zw", 12, 16, OTHER, "smoke-zw", "smoke", "", ["smo\u200bke"]),
    ch("c_plain", 5, 11, OTHER, "plain", "Plain", "A plain channel.", []),
    ch("c_bidi", 4, 6, OTHER, "bi\u200bdi", "invoice\u202efdp.exe\u200b\u200d!", "a" + INVISIBLE + "b", []),
    ch("c_lookalike", 2, 5, OTHER, "agent-tooling", "Agent tooling releases", "Looks like the launch channel.", []),
    ch("c_" + "l" * 52, 1, 2, LAUNCH, "agent-tooling", "Agent tooling releases", "Releases of agent tooling.", ["agents"]),
    # A quiet launch channel: still shown (launch channels are not filtered).
    ch("c_" + "m" * 52, 1, 0, LAUNCH, "llm-papers", "LLM papers", "Papers.", []),
    # Community channels the page leaves out, one per rule.
    ch("c_sandbox", 9, 9, OTHER, "sandbox", "Sandbox", "Test posting here.", []),
    ch("c_tagged", 4, 4, OTHER, "tagged-test", "Tagged test", "", ["test"]),
    ch("c_quiet", 3, 0, OTHER, "quiet", "Quiet", "No posts this week.", []),
]
open(work + "/list.json", "w").write(json.dumps({"items": items, "next": ""}))
EOF
# One stub, two ports: 8080 answers with Access-Control-Allow-Origin: *, 8081
# serves the same data with no CORS header at all.
cat > "$work/stub.conf" <<'EOF'
pid /tmp/nginx.pid;
events {}
http {
  log_format stub 'STUB $server_port $request_method $request_uri $status';
  access_log /dev/stdout stub;
  server {
    listen 8080;
    root /stub;
    location = /v1/channels {
      default_type application/json;
      add_header Access-Control-Allow-Origin "*" always;
      try_files /list.json =404;
    }
    location / { return 404; }
  }
  server {
    listen 8081;
    root /stub;
    location = /v1/channels {
      default_type application/json;
      try_files /list.json =404;
    }
    location / { return 404; }
  }
}
EOF
chmod -R a+rX "$work"
docker run -d --name "$stub" -p 127.0.0.1::8080 -p 127.0.0.1::8081 \
  -v "$work/stub.conf:/etc/nginx/nginx.conf:ro" -v "$work:/stub:ro" \
  "$stub_image" nginx -g 'daemon off;' >/dev/null
cors_hub="http://127.0.0.1:$(docker port "$stub" 8080/tcp | head -1 | sed 's/.*://')"
nocors_hub="http://127.0.0.1:$(docker port "$stub" 8081/tcp | head -1 | sed 's/.*://')"
for _ in $(seq 1 50); do curl -fs "$cors_hub/v1/channels?ready" >/dev/null 2>&1 && break; sleep 0.2; done

# Controls on the stub itself: it serves the data, with and without the header.
code="$(get "$cors_hub/v1/channels?sort=active")"
check '[ "$code" = 200 ] && [ "$(hdr Access-Control-Allow-Origin)" = "*" ]' "stub control: the CORS port sends Access-Control-Allow-Origin: *"
code="$(get "$nocors_hub/v1/channels?sort=active")"
check '[ "$code" = 200 ] && [ "$(cat "$B")" = "$(cat "$work/list.json")" ] && ! grep -qi "^access-control-" "$H"' "stub control: the no-CORS port serves the same JSON and no CORS header"

docker build -q -f "$root/site/Dockerfile" --build-arg "HUB_URL=$cors_hub" -t "$cors_image" "$root" >/dev/null
docker build -q -f "$root/site/Dockerfile" --build-arg "HUB_URL=$nocors_hub" -t "$nocors_image" "$root" >/dev/null
run_site cors "$cors_image"; cors_base="$site_url"
run_site nocors "$nocors_image"; nocors_base="$site_url"
get "$cors_base/" >/dev/null
check '[[ "$(hdr Content-Security-Policy)" == *"connect-src '"'self'"' $cors_hub;"* ]]' "a stub build's CSP connect-src follows HUB_URL ($cors_hub)"

# ── headless browser ────────────────────────────────────────────────
browser() {
  # nix first: it pins the browsers to its Playwright version.
  if command -v nix-shell >/dev/null 2>&1; then
    nix-shell -p python3Packages.playwright playwright-driver.browsers \
      --run "python3 '$here/browser_check.py'$(printf ' %q' "$@")"
  elif python3 -c 'import playwright' >/dev/null 2>&1; then
    python3 "$here/browser_check.py" "$@"
  else
    return 127
  fi
}
run_browser() {
  local out rc
  set +e
  out="$(browser "$@" 2>&1)"
  rc=$?
  set -e
  if [ "$rc" = 127 ]; then
    if [ "${ALLOW_NO_BROWSER:-0}" = 1 ]; then
      echo "SKIPPED - browser checks ($1): no Playwright available (ALLOW_NO_BROWSER=1)"
    else
      notok "browser checks ($1): no Playwright available (set ALLOW_NO_BROWSER=1 to skip)"
    fi
    return
  fi
  echo "$out" | grep -E '^(ok - |not ok - |# browser: )' || true
  local bp bf
  bp="$(grep -c '^ok - ' <<<"$out" || true)"
  bf="$(grep -c '^not ok - ' <<<"$out" || true)"
  pass=$((pass + bp)); fail=$((fail + bf))
  if [ "$rc" != 0 ] && [ "$bf" = 0 ]; then
    notok "browser checks ($1) exited $rc: $(tail -5 <<<"$out" | tr '\n' ' ')"
  fi
  if [ "$bp" = 0 ] && [ "$bf" = 0 ]; then
    notok "browser checks ($1) ran no assertions"
  fi
}
# The stub's log so far holds the curl controls' own requests; only lines
# written after this point are the browser's.
stub_log_before="$(docker logs "$stub" 2>/dev/null | grep -c '^STUB ' || true)"
run_browser stub "$cors_base"
run_browser nocors "$nocors_base"

stub_log="$(docker logs "$stub" 2>/dev/null | grep '^STUB ' | tail -n +"$((stub_log_before + 1))" || true)"
check 'grep -q "^STUB 8080 GET /v1/channels?sort=active 200$" <<<"$stub_log"' "the page fetched the CORS stub (GET /v1/channels?sort=active, 200)"
check 'grep -q "^STUB 8081 GET /v1/channels?sort=active 200$" <<<"$stub_log"' "the page fetched the no-CORS stub and it answered 200: the browser refused the answer"
check '[ -n "$stub_log" ] && ! grep -qv " GET /v1/channels?" <<<"$stub_log"' "the page only ever asked the hub for GET /v1/channels (no preflight, no other path)"

# A browser MCP client on another origin (a page served by the stub) calling
# the production image's /mcp (its sidecar is still running). After the stub
# log check above: this loads a page from the stub.
run_browser mcpcors "$cors_hub" "$base"

if [ "${CHECK_LIVE:-0}" = 1 ]; then
  run_browser live "$base"
else
  echo "SKIPPED - live: the production page against $PROD_HUB (set CHECK_LIVE=1)"
fi

echo "# $pass passed, $fail failed"
[ "$fail" = 0 ]
