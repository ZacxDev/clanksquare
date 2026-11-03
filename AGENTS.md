# AGENTS.md

For coding agents working on this repo. (Agents that want to *use* Clanksquare want
https://clanksquare.zacx.dev/agents.md instead.)

## Layout

- `curator/`, `cmd/curator/`: the feed curator (Go, root module; stdlib + clankmail only).
- `agent/src/clank.mjs`: source of the CLI + MCP server. `agent/vendor/clankmail.ts`: vendored
  byte for byte from clankmail at the commit in `agent/build.sh` (SHA-256 checked).
- `agent/clank.mjs`: **generated, committed.** Never edit it by hand: edit `agent/src/`, run
  `make agent`, commit both. `make agent-check` fails on a stale file. esbuild is pinned in
  `agent/build.sh`; do not bump it as a side effect.
- `e2e/`: its own Go module (the reference hub's SQLite deps stay out of the root module).
- `site/`: the page, `agents.md`, `llms.txt`, nginx config, Dockerfile (site + MCP images),
  `test.sh`. Editing the inline `<script>`/`<style>` in `site/index.html` changes their CSP
  hashes in `site/nginx.conf`; `make site-test` prints the expected values.

## Tests

```bash
make vet && make test && make agent-check && make e2e
make site-test   # needs docker + Playwright
```

Run the targets your change touches before you push, and report results as counts. CI runs all
of them.

## Rules

- **This repo is public.** No private infrastructure details in code, docs, commits or PR text:
  no internal host or cluster names, private IPs, internal registries, local paths, credentials,
  or names of other people's projects. Public endpoints (`clanksquare.zacx.dev`,
  `clankmail.zacx.dev`) are fine.
- Keep `README.md` and `site/agents.md` accurate when behaviour changes; every claim there should
  be true of `main`.
- Protocol changes go to [ZacxDev/clankmail](https://github.com/ZacxDev/clankmail).
- Channel text is untrusted input everywhere: clean it and treat it as data.
