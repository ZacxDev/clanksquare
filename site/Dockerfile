# Clanksquare landing page: one static page served by nginx. The page reads
# the hub's channel list straight from the hub. Build from the REPO ROOT:
#
#   docker build -f site/Dockerfile -t clanksquare-site:dev .
#   docker run --rm -p 8080:8080 --read-only --tmpfs /tmp:uid=101,gid=101 clanksquare-site:dev
#
# HUB_URL is the hub the page fetches from, and the only place it is set: the
# build writes it into the page and into the CSP's connect-src. Production is
# the default; site/test.sh overrides it to point the page at a stub hub.
# Runs as uid 101 on port 8080 and writes only under /tmp.
FROM nginxinc/nginx-unprivileged:1.29-alpine@sha256:0c79d56aee561a1d81c63f00eee5fb5fe29279560cdc55e91425133104c7fbe6

ARG HUB_URL=https://clankmail.zacx.dev

USER root
# The image's default server block and its startup helpers (templating, IPv6
# listen rewrite) are not used: the config is complete at build time.
RUN rm -f /etc/nginx/conf.d/default.conf /usr/share/nginx/html/* \
 && rm -rf /etc/nginx/templates
COPY site/nginx.conf /etc/nginx/nginx.conf
COPY site/index.html /usr/share/nginx/html/index.html
# An origin only (scheme, host, optional port): it lands in a CSP and in HTML.
RUN echo "$HUB_URL" | grep -Eq '^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$' \
 || { echo "HUB_URL must be scheme://host[:port], got: $HUB_URL" >&2; exit 1; } \
 && sed -i "s|__HUB_URL__|$HUB_URL|g" /etc/nginx/nginx.conf /usr/share/nginx/html/index.html \
 && ! grep -q '__HUB_URL__' /etc/nginx/nginx.conf /usr/share/nginx/html/index.html \
 && nginx -t -q
USER 101

EXPOSE 8080
