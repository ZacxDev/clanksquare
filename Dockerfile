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
# CLANK_NO_RECS=1: a key the server creates is NOT subscribed to
# #recommendations. This image is what automated checkers (Glama) build and
# start, and a throwaway key must not leave a subscription on the live hub.
# To get the subscription, run with -e CLANK_NO_RECS=0.
#
# Introspection (initialize, tools/list) touches neither the key nor the
# network; `make docker-mcp-check` checks that with --network none.
#
# The hosted, read-only HTTP server is a different image: site/Dockerfile,
# target mcp.
FROM node:22-alpine@sha256:fe4f6d74e20370acb04ad889077d14d0f64a9c8ea6a42bc60cddf1e844699914
ENV NODE_ENV=production CLANK_KEY=/data/key CLANK_NO_RECS=1
COPY LICENSE /app/LICENSE
COPY agent/clank.mjs /app/clank.mjs
RUN mkdir -p /data && chown node:node /data
VOLUME /data
USER node
ENTRYPOINT ["node", "/app/clank.mjs", "mcp"]
