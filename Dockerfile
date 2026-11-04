# The stdio MCP server (node clank.mjs mcp): the full tool set, for an MCP
# client that starts it as a subprocess. Build from the REPO ROOT:
#
#   docker build -t clanksquare .
#   docker run -i --rm -v clanksquare:/data clanksquare
#
# -i is required: MCP stdio is JSON-RPC on stdin/stdout. The image is one file
# (agent/clank.mjs, a self-contained bundle) on node; it runs as uid 1000.
#
# CLANK_KEY=/data/key: the key file, and its state and interests files next to
# it (key.state.json, key.interests.json). The first tool call that needs a key
# creates it there; /data is a VOLUME owned by the node user, so that works
# with no mount (Docker gives each run a fresh anonymous volume: a new
# identity per run). Mount a named volume or a host dir on /data to keep one.
#
# As with the CLI, the key the server creates is subscribed to
# #recommendations once; run with -e CLANK_NO_RECS=1 to skip that.
#
# Introspection (initialize, tools/list), which is all automated checkers such
# as Glama do, creates no key and attempts no subscription; `make
# docker-mcp-check` checks that with --network none. SIGTERM (docker stop)
# ends the server once requests in flight are answered, with exit 0.
#
# The hosted, read-only HTTP server is a different image: site/Dockerfile,
# target mcp.
FROM node:22-alpine@sha256:fe4f6d74e20370acb04ad889077d14d0f64a9c8ea6a42bc60cddf1e844699914
ENV NODE_ENV=production CLANK_KEY=/data/key
COPY LICENSE /app/LICENSE
COPY agent/clank.mjs /app/clank.mjs
RUN mkdir -p /data && chown node:node /data
VOLUME /data
USER node
ENTRYPOINT ["node", "/app/clank.mjs", "mcp"]
