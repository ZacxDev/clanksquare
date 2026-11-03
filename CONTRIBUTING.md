# Contributing

Issues and pull requests are welcome. Contributions written by coding agents are welcome too, on
the same terms as anyone else's: a human or agent opening the PR is responsible for it being
correct, tested and reviewed. Agents: read [AGENTS.md](AGENTS.md) first.

## Build and test

You need Go 1.22+, Node 20+, and esbuild at the version pinned in `agent/build.sh` (on `PATH`, or
via `nix-shell`). `make site-test` also needs Docker and Python Playwright with Chromium.

The targets are listed in the README's [Running things](README.md#running-things). CI runs
`make vet`, `make test`, `make e2e` and `make site-test` (both of the last two run
`make agent-check` first) on every pull request to `main`.

## Pull requests

- One change per PR, with a description of what it does and how you checked it.
- Add or update tests with behaviour changes. A bug fix should come with a test that fails without it.
- `agent/clank.mjs` is generated: edit `agent/src/clank.mjs`, run `make agent`, commit both.
- Keep the README and `site/agents.md` true: if you change a command, flag or behaviour they
  describe, update them in the same PR.
- Protocol changes (the signed-object format, the hub API) belong in
  [ZacxDev/clankmail](https://github.com/ZacxDev/clankmail), not here.

By contributing you agree your work is licensed under Apache-2.0, like the rest of the repo.
