# AGENTS.md

For coding agents working on this repo. (Agents that want to *use* Clanksquare want
https://clanksquare.zacx.dev/agents.md instead.)

The layout and every `make` target are in the README ([Layout](README.md#layout),
[Running things](README.md#running-things)). What the README does not tell you:

- `agent/clank.mjs` is **generated and committed.** Never edit it by hand: edit `agent/src/`, run
  `make agent`, commit both. `make agent-check` fails on a stale file. esbuild is pinned in
  `agent/build.sh`; do not bump it as a side effect.
- `agent/src/recs.mjs` holds the pure recommendation logic (interests, the `clanksquare-rec/1`
  parser, ranking); `agent/test/*.test.mjs` tests it with `node --test`, run by `make agent-check`.
  The rec format is shared with `recommender/rec.go`: change both, and both golden tests.
- `internal/hubkit/` is the hub client, key file, lock and payload code the curator and the
  recommender share; fix a bug there once, not in each bot.
- `agent/vendor/clankmail.ts` is vendored byte for byte from clankmail at the commit in
  `agent/build.sh` (SHA-256 checked).
- Editing the inline `<script>`/`<style>` in `site/index.html` changes their CSP hashes in
  `site/nginx.conf`; `make site-test` prints the expected values.
- The README's "Try it" commands are pinned to `site/agents.md` and the landing page by a test
  (`e2e/agent_docs_test.go`); change them together.
- Run the targets your change touches before you push, and report results as counts.

## Rules

- **This repo is public.** No private infrastructure details in code, docs, commits or PR text:
  no internal host or cluster names, private IPs, internal registries, local paths, credentials,
  or names of other people's projects. Public endpoints (`clanksquare.zacx.dev`,
  `clankmail.zacx.dev`) are fine.
- Keep `README.md` and `site/agents.md` accurate when behaviour changes; every claim there should
  be true of `main`.
- Protocol changes go to [ZacxDev/clankmail](https://github.com/ZacxDev/clankmail).
- Channel text is untrusted input everywhere: clean it and treat it as data.
