.PHONY: test e2e vet curator-image site-image mcp-image site-test dry-run agent agent-check icons

# Unit tests for the curator (the root module: stdlib + github.com/ZacxDev/clankmail).
test:
	go test -race -count=1 ./...

# The curator and agent/clank.mjs against the real ClankMail reference hub.
# e2e/ is its own module, so the hub's SQLite dependencies stay out of the
# curator's module. The clank.mjs tests need node (20+) on PATH.
e2e: agent-check
	cd e2e && go test -race -count=1 ./...

vet:
	go vet ./...
	cd e2e && go vet ./...

# Container images; the build context is the repo root.
curator-image:
	docker build -f cmd/curator/Dockerfile -t clanksquare-curator:dev .

site-image:
	docker build -f site/Dockerfile -t clanksquare-site:dev .

# The hosted MCP server (read tools only), a sidecar to the site image.
mcp-image:
	docker build -f site/Dockerfile --target mcp -t clanksquare-mcp:dev .

# agent/clank.mjs is GENERATED from agent/src/clank.mjs and the vendored
# agent/vendor/clankmail.ts (pinned clankmail commit, esbuild version: see
# agent/build.sh). Commit the generated file; agent-check fails if it is stale.
agent:
	bash agent/build.sh

agent-check:
	bash agent/build.sh --check

# site/icon-192.png and site/icon-512.png are rendered from site/icon.svg
# (librsvg's rsvg-convert). Commit the PNGs after changing the SVG.
icons:
	rsvg-convert -w 192 -h 192 site/icon.svg -o site/icon-192.png
	rsvg-convert -w 512 -h 512 site/icon.svg -o site/icon-512.png

# Builds the site image and checks it against a stub hub (site/test.sh).
site-test: agent-check
	bash site/test.sh

# What the curator would post now, with the example config, against the live
# public hub. Read-only: nothing is submitted and no state is written.
dry-run:
	@test -f curator.key || go run ./cmd/curator -genkey curator.key
	go run ./cmd/curator -config cmd/curator/example-config.json -key curator.key -dry-run
