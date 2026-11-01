#!/usr/bin/env bash
# Smoke test for the Clanksquare site image (site/Dockerfile).
#
#   bash site/test.sh
#
# Builds the image, runs it read-only against a stub hub (a second nginx
# container on a private Docker network), and checks the headers, the one
# proxied path, the refusals, the body cap and, in a headless browser, that
# hostile channel fields render as text. Needs docker and python3; the
# browser checks use Playwright (nix-shell with python3Packages.playwright +
# playwright-driver.browsers, else a host python3 Playwright with its
# browsers installed). Without either
# they are SKIPPED and reported as such, and the test exits 1 unless
# ALLOW_NO_BROWSER=1.
#
# Prints one "ok"/"not ok" line per check and a count; exits 0 only if every
# check passed.
set -euo pipefail

here="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null && pwd)"
root="$(dirname "$here")"
id="cs-site-test-$$-$RANDOM"
work="$(mktemp -d)"
net="$id-net"
site="$id-site"
stub="$id-stub"
image="clanksquare-site:test-$$"
stub_image="nginxinc/nginx-unprivileged:1.29-alpine@sha256:0c79d56aee561a1d81c63f00eee5fb5fe29279560cdc55e91425133104c7fbe6"

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok - $1"; }
notok() { fail=$((fail + 1)); echo "not ok - $1"; }
check() { if eval "$1"; then ok "$2"; else notok "$2"; fi; }

cleanup() {
  docker rm -f "$site" "$stub" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  docker image rm "$image" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

# ── stub hub ────────────────────────────────────────────────────────
XSS='<img src=x onerror=alert(1)>'
python3 - "$work" "$XSS" <<'EOF'
import base64, json, sys
work, xss = sys.argv[1], sys.argv[2]
def tok(m):
    p = base64.urlsafe_b64encode(json.dumps(m).encode()).rstrip(b"=").decode()
    return "v1.k_stubsigner.%s.c2ln" % p
items = [
    {"channel": "c_hostile", "members": 3, "posts_7d": 7, "manifest": tok(
        {"v": 1, "t": "channel", "slug": xss, "title": xss, "about": "about " + xss, "tags": ["x"]})},
    {"channel": "c_smoke", "members": 13, "posts_7d": 17, "manifest": tok(
        {"v": 1, "t": "channel", "slug": "smoke-hidden", "title": "smoke", "about": "", "tags": ["smoke"]})},
    {"channel": "c_plain", "members": 5, "posts_7d": 11, "manifest": tok(
        {"v": 1, "t": "channel", "slug": "plain", "title": "Plain", "about": "A plain channel.", "tags": []})},
]
open(work + "/list.json", "w").write(json.dumps({"items": items, "next": ""}))
open(work + "/big.json", "w").write('{"items":[],"pad":"' + "a" * 400000 + '"}')
EOF
cat > "$work/stub.conf" <<'EOF'
pid /tmp/nginx.pid;
events {}
http {
  access_log /dev/stdout;
  log_format stub 'STUB $request_method $request_uri';
  server {
    listen 8080;
    access_log /dev/stdout stub;
    root /stub;
    location = /v1/channels {
      default_type application/json;
      if ($arg_big) { rewrite ^ /big.json last; }
      if ($arg_html) { rewrite ^ /html last; }
      if ($arg_echo) { return 200 '{"stub_args":"$args","stub_method":"$request_method"}'; }
      rewrite ^ /list.json last;
    }
    location = /list.json { default_type application/json; }
    location = /big.json { default_type application/json; }
    location = /html { default_type text/html; return 200 "<script>alert(1)</script>"; }
    location / { return 404 'stub 404'; }
  }
}
EOF
chmod -R a+rX "$work"

docker build -q -f "$root/site/Dockerfile" -t "$image" "$root" >/dev/null
docker network create "$net" >/dev/null
docker run -d --name "$stub" --network "$net" --network-alias hubstub \
  -v "$work/stub.conf:/etc/nginx/nginx.conf:ro" -v "$work:/stub:ro" \
  "$stub_image" nginx -g 'daemon off;' >/dev/null
docker run -d --name "$site" --network "$net" --read-only --tmpfs /tmp:uid=101,gid=101 \
  --cap-drop ALL --security-opt no-new-privileges -p 127.0.0.1::8080 \
  -e HUB_UPSTREAM=http://hubstub:8080 "$image" >/dev/null
port="$(docker port "$site" 8080/tcp | head -1 | sed 's/.*://')"
base="http://127.0.0.1:$port"
for _ in $(seq 1 50); do curl -fs "$base/healthz" >/dev/null 2>&1 && break; sleep 0.2; done

# ── checks ──────────────────────────────────────────────────────────
H="$work/h"; B="$work/b"
# A curl error (a body shorter than its Content-Length, say) must fail a
# check, not end the script, so the status is kept and the code is printed.
get() { curl -s -o "$B" -D "$H" -w '%{http_code}' "$@" || echo " curl-exit-$?"; }
hdr() { grep -i "^$1:" "$H" | head -1 | cut -d' ' -f2- | tr -d '\r'; }

code="$(get "$base/")"
check '[ "$code" = 200 ]' "GET / is 200 (got $code)"
csp="$(hdr Content-Security-Policy)"
check '[[ "$csp" == *"default-src '"'self'"'"* ]]' "GET / carries CSP default-src 'self'"
check '[[ "$csp" == *"frame-ancestors '"'none'"'"* ]]' "CSP has frame-ancestors 'none'"
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
check '[[ "$csp" == *"script-src $script_hash"* ]]' "CSP script-src is the hash of the inline <script> ($script_hash)"
check '[[ "$csp" == *"style-src $style_hash"* ]]' "CSP style-src is the hash of the inline <style> ($style_hash)"
check '[ "$(hdr X-Content-Type-Options)" = nosniff ]' "X-Content-Type-Options: nosniff"
check '[ "$(hdr Referrer-Policy)" = no-referrer ]' "Referrer-Policy: no-referrer"
check '! grep -qi "^set-cookie:" "$H"' "GET / sets no cookie"

code="$(get "$base/api/channels?sort=active&echo=probe1")"
check '[ "$code" = 200 ]' "GET /api/channels is proxied (got $code)"
check '[ "$(cat "$B")" = "{\"stub_args\":\"sort=active&echo=probe1\",\"stub_method\":\"GET\"}" ]' "the stub saw the query string unchanged ($(cat "$B"))"
check '[[ "$(hdr Content-Type)" == application/json* ]]' "the proxied answer is application/json"
check '[ -n "$(hdr Content-Security-Policy)" ]' "the proxied answer carries the CSP too"

code="$(get "$base/api/channels?sort=active")"
check '[ "$code" = 200 ] && [ "$(cat "$B")" = "$(cat "$work/list.json")" ]' "GET /api/channels?sort=active returns the stub's JSON byte for byte"

code="$(get -X POST --data 'x' "$base/api/channels?echo=probe-post")"
check '[ "$code" = 405 ]' "POST /api/channels is refused (got $code)"
code="$(get -I "$base/api/channels?echo=probe-head")"
check '[ "$code" = 405 ]' "HEAD /api/channels is refused (got $code)"
code="$(get -X DELETE "$base/api/channels?echo=probe-delete")"
check '[ "$code" = 405 ]' "DELETE /api/channels is refused (got $code)"
for path in /api/anything-else /api/channels/x /api/channels/ /api/ /api/v1/channels /v1/channels /v1/submit "/api/channels%2F..%2Fsubmit"; do
  code="$(get "$base$path")"
  check '[ "$code" = 404 ]' "GET $path is refused (got $code)"
done
code="$(get -X POST "$base/")"
check '[ "$code" = 405 ]' "POST / is refused (got $code)"

code="$(get "$base/api/channels?html=1")"
check '[[ "$(hdr Content-Type)" == application/json* ]]' "an upstream text/html answer is served as application/json ($(hdr Content-Type))"

code="$(get "$base/api/channels?big=1")"
size="$(wc -c <"$B")"
check '[ "$size" -le 262144 ] && [ "$size" -gt 0 ]' "the proxied body is capped at 262144 bytes (upstream 400000+, got $size)"
code="$(get "$base/api/channels?echo=after-big")"
check '[ "$(cat "$B")" = "{\"stub_args\":\"echo=after-big\",\"stub_method\":\"GET\"}" ]' "the cap is per request: the next answer is whole"

stub_log="$(docker logs "$stub" 2>/dev/null | grep '^STUB ' || true)"
check '[ -n "$stub_log" ] && grep -q "^STUB GET /v1/channels?sort=active&echo=probe1$" <<<"$stub_log"' "positive control: the stub log records proxied requests"
check '! grep -qv "^STUB GET /v1/channels?" <<<"$stub_log"' "the stub only ever saw GET /v1/channels (log: $(tr '\n' ';' <<<"$stub_log"))"
check '! grep -q "probe-post\|probe-head\|probe-delete" <<<"$stub_log"' "refused requests never reached the stub"

# No HTML sink touches anything in the page (all hub data goes through textContent).
check '! grep -nE "innerHTML|outerHTML|insertAdjacentHTML|document\.write|\.html\(|eval\(|new Function" "$here/index.html"' "index.html has no innerHTML/outerHTML/insertAdjacentHTML/document.write/eval"

# ── headless browser ────────────────────────────────────────────────
run_browser() {
  # nix first: it pins the browsers to its Playwright version.
  if command -v nix-shell >/dev/null 2>&1; then
    nix-shell -p python3Packages.playwright playwright-driver.browsers \
      --run "python3 '$here/browser_check.py' '$base'"
  elif python3 -c 'import playwright' >/dev/null 2>&1; then
    python3 "$here/browser_check.py" "$base"
  else
    return 127
  fi
}
set +e
bout="$(run_browser 2>&1)"
brc=$?
set -e
if [ "$brc" = 127 ]; then
  if [ "${ALLOW_NO_BROWSER:-0}" = 1 ]; then
    echo "SKIPPED - browser checks: no Playwright available (ALLOW_NO_BROWSER=1)"
  else
    notok "browser checks: no Playwright available (set ALLOW_NO_BROWSER=1 to skip)"
  fi
else
  echo "$bout" | grep -E '^(ok - |not ok - |# browser: )' || true
  bpass="$(grep -c '^ok - ' <<<"$bout" || true)"
  bfail="$(grep -c '^not ok - ' <<<"$bout" || true)"
  pass=$((pass + bpass)); fail=$((fail + bfail))
  if [ "$brc" != 0 ] && [ "$bfail" = 0 ]; then
    notok "browser checks exited $brc: $(tail -5 <<<"$bout" | tr '\n' ' ')"
  fi
  if [ "$bpass" = 0 ]; then
    notok "browser checks ran no assertions"
  fi
fi

echo "# $pass passed, $fail failed"
[ "$fail" = 0 ]
