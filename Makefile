.PHONY: test e2e vet curator-image recommender-image editor-image editor-retest site-image mcp-image site-test docker-mcp-check dry-run agent agent-check icons brand-check

# Unit tests for the curator and the recommender (the root module: stdlib + github.com/ZacxDev/clankmail).
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

recommender-image:
	docker build -f cmd/recommender/Dockerfile -t clanksquare-recommender:dev .

editor-image:
	docker build -f cmd/editor/Dockerfile -t clanksquare-editor:dev .

# Re-test of the decider, due around 2026-11-22 (two weeks of preview): copy
# the editor's truth.json and preview/ off the cluster. With PREVIEW_DIR, eval
# scores the default decider's rank pick from the preview files (no model
# calls); without it, it decides the hub window with the config's decider (set
# "openrouter_url" etc. and OPENROUTER_API_KEY). Compare precision@k and AUC
# across candidates (see README, "Re-test"). No labels file is needed.
editor-retest:
	@test -n "$(EDITOR_CONFIG)" -a -n "$(TRUTH)" || { echo "usage: make editor-retest EDITOR_CONFIG=c.json TRUTH=truth.json [PREVIEW_DIR=dir/preview] [SINCE_HOURS=336] [PREDICTIONS=out.jsonl]"; exit 2; }
	go run ./cmd/editor eval -config $(EDITOR_CONFIG) -truth $(TRUTH) $(if $(PREVIEW_DIR),-preview-dir $(PREVIEW_DIR)) $(if $(SINCE_HOURS),-since-hours $(SINCE_HOURS)) $(if $(PREDICTIONS),-predictions $(PREDICTIONS))

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
	node --test agent/test/*.test.mjs

# site/icon-512.png is rendered from site/icon.svg, and site/og.png (the social
# preview card) from site/og.svg, with librsvg's rsvg-convert; og.svg's text
# needs the DejaVu Sans Mono font. site/brand.sha256 records the sources and
# their renders, and brand-check fails if any has changed since. Commit all
# five files after changing an SVG, e.g.
#   nix-shell -p librsvg dejavu_fonts --run 'make icons'
icons:
	rsvg-convert -w 512 -h 512 site/icon.svg -o site/icon-512.png
	rsvg-convert -w 1200 -h 630 site/og.svg -o site/og.png
	sha256sum site/icon.svg site/icon-512.png site/og.svg site/og.png > site/brand.sha256

# The brand assets: the mark, the PNG sizes, brand.sha256, the og: meta tags,
# and (with rsvg-convert on PATH) a fresh render of each SVG (site/brand_check.py).
brand-check:
	python3 site/brand_check.py

# Builds the site image and checks it against a stub hub (site/test.sh).
site-test: agent-check brand-check
	bash site/test.sh

# Builds the root Dockerfile (the stdio MCP server, what Glama builds) and
# checks MCP introspection over stdio with --network none
# (agent/docker-test.sh). It checks the committed agent/clank.mjs as the image
# ships it; agent-check (in e2e) is what keeps that file fresh.
docker-mcp-check:
	bash agent/docker-test.sh

# What the curator would post now, with the example config, against the live
# public hub. Read-only: nothing is submitted and no state is written.
dry-run:
	@test -f curator.key || go run ./cmd/curator -genkey curator.key
	go run ./cmd/curator -config cmd/curator/example-config.json -key curator.key -dry-run
