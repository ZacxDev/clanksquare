#!/usr/bin/env bash
# Checks the root Dockerfile: the stdio MCP image that automated checkers
# (Glama) build and start.
#
#   bash agent/docker-test.sh
#   IMAGE=clanksquare:dev bash agent/docker-test.sh   # check an already-built image
#
# Builds the image, then pipes MCP introspection (initialize,
# notifications/initialized, tools/list) into `docker run -i --rm --network
# none` and checks: the server answers both requests, stdout is JSON-RPC and
# nothing else, the tool set is exactly the pinned one, and no key file (so
# no subscription: that needs a key) is written to /data, nor is a
# #recommendations subscription attempted. Controls: a tool call that creates
# a key DOES leave one in /data, and its auto-subscription attempt DOES show
# on stderr (it fails: no network), so both "nothing happened" checks can see
# the thing they rule out. Needs docker and python3; no network after the
# build.
#
# Prints one "ok"/"not ok" line per check and a count; exits 0 only if every
# check passed.
set -euo pipefail

here="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null && pwd)"
root="$(dirname "$here")"
id="cs-docker-test-$$-$RANDOM"
work="$(mktemp -d)"
built=""
volumes=()

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok - $1"; }
notok() { fail=$((fail + 1)); echo "not ok - $1"; }
check() { if eval "$1"; then ok "$2"; else notok "$2"; fi; }

cleanup() {
  [ "${#volumes[@]}" = 0 ] || docker volume rm -f "${volumes[@]}" >/dev/null 2>&1 || true
  [ -z "$built" ] || docker image rm "$built" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

if [ -n "${IMAGE:-}" ]; then
  image="$IMAGE"
else
  image="clanksquare:test-$$"
  docker build -q -t "$image" "$root" >/dev/null
  built="$image"
fi

# The tool set clank.mjs mcp serves, in order. Pinned here on purpose: a tool
# added or removed must change this line.
EXPECTED="clank_channels clank_read clank_join clank_post clank_create_channel clank_subscribe clank_digest clank_whoami clank_set_name clank_recs clank_interests_set clank_interests_show"

INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"docker-test","version":"0"}}}'
INITIALIZED='{"jsonrpc":"2.0","method":"notifications/initialized"}'
LIST='{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
WHOAMI='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"clank_whoami","arguments":{}}}'

# mcp <name> <stdin-file> [docker run args...]: run the image on a fresh named
# volume at /data, sandboxed and with no network; stdout, stderr and the exit
# code land in $work/<name>.{out,err,rc}.
mcp() {
  local name="$1" in="$2"; shift 2
  local vol="$id-$name"
  volumes+=("$vol")
  docker volume create "$vol" >/dev/null
  set +e
  timeout 60 docker run -i --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges \
    -v "$vol:/data" "$@" "$image" <"$in" >"$work/$name.out" 2>"$work/$name.err"
  echo $? >"$work/$name.rc"
  set -e
}
# data <name>: what the run left in /data (one name per line).
data() {
  docker run --rm --network none -v "$id-$1:/data:ro" --entrypoint ls "$image" -A /data
}

user="$(docker image inspect --format '{{.Config.User}}' "$image")"
check '[ "$user" = node ]' "the image runs as the node user, not root (got '$user')"

# ── introspection ───────────────────────────────────────────────────
printf '%s\n' "$INIT" "$INITIALIZED" "$LIST" >"$work/intro.in"
mcp intro "$work/intro.in"
rc="$(cat "$work/intro.rc")"
check '[ "$rc" = 0 ]' "introspection with --network none exits 0 once stdin closes (got $rc; stderr: $(head -c 200 "$work/intro.err" | tr '\n' ' '))"
verdict="$(python3 - "$work/intro.out" "$EXPECTED" <<'EOF'
import json, sys
lines = open(sys.argv[1], encoding="utf-8").read().split("\n")
if lines and lines[-1] == "":
    lines.pop()
msgs = []
for i, l in enumerate(lines):
    try:
        m = json.loads(l)
    except ValueError:
        print("stdout line %d is not JSON: %r" % (i + 1, l[:80])); sys.exit()
    if not isinstance(m, dict) or m.get("jsonrpc") != "2.0":
        print("stdout line %d is not JSON-RPC 2.0: %r" % (i + 1, l[:80])); sys.exit()
    msgs.append(m)
if [m.get("id") for m in msgs] != [1, 2]:
    print("expected exactly the responses to ids 1 and 2, got ids %r" % [m.get("id") for m in msgs]); sys.exit()
init, tools = msgs
r = init.get("result") or {}
if r.get("protocolVersion") != "2025-11-25" or (r.get("serverInfo") or {}).get("name") != "clanksquare" or "tools" not in (r.get("capabilities") or {}):
    print("bad initialize result: %r" % init); sys.exit()
names = " ".join(t["name"] for t in (tools.get("result") or {}).get("tools", []))
if names != sys.argv[2]:
    print("tools/list: %s" % names); sys.exit()
print("ok")
EOF
)"
check '[ "$verdict" = ok ]' "stdout is the initialize and tools/list responses, JSON-RPC only, listing exactly: $EXPECTED$([ "$verdict" = ok ] || echo " ($verdict)")"
left="$(data intro)"
check '[ -z "$left" ]' "introspection writes nothing to /data: no key, so no subscription (found: $(tr '\n' ' ' <<<"$left"))"
check '! grep -q "recommendations" "$work/intro.err"' "introspection attempts no #recommendations subscription (stderr: $(head -c 200 "$work/intro.err" | tr '\n' ' '))"

# ── controls ────────────────────────────────────────────────────────
# A key-creating tool call leaves a key in /data and, as in the CLI, tries to
# subscribe the new key to #recommendations (no network: it fails and says so
# on stderr). These are the positive controls for the two introspection
# checks above: each can see what it says did not happen.
printf '%s\n' "$INIT" "$INITIALIZED" "$WHOAMI" >"$work/whoami.in"
mcp whoami "$work/whoami.in"
left="$(data whoami)"
check 'grep -qx key <<<"$left"' "control: clank_whoami creates /data/key in the image's default config (found: $(tr '\n' ' ' <<<"$left"))"
check 'grep -q "\"id\":3" "$work/whoami.out" && grep -q "k_[a-z2-7]\{52\}" "$work/whoami.out"' "control: clank_whoami answers with the new key id"
check 'grep -q "could not subscribe to #recommendations" "$work/whoami.err"' "control: the new key's #recommendations subscription is attempted, and shows on stderr (stderr: $(head -c 200 "$work/whoami.err" | tr '\n' ' '))"

echo "# $pass passed, $fail failed"
[ "$fail" = 0 ]
