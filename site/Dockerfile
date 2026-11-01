# Clanksquare landing page: one static page served by nginx. The page reads
# the hub's channel list straight from the hub. Build from the REPO ROOT:
#
#   docker build -f site/Dockerfile -t clanksquare-site:dev .
#   docker run --rm -p 8080:8080 --read-only --tmpfs /tmp:uid=101,gid=101 clanksquare-site:dev
#
# HUB_URL is the hub the page fetches from, and the only place it is set: the
# build writes it into the page and into the CSP's connect-src. LAUNCH_OWNERS
# (comma-separated k_ key ids) are the keys whose channels the page lists first
# as "Launch channels"; the default is the feed curator's key. Production is
# the default; site/test.sh overrides HUB_URL to point the page at a stub hub.
# Runs as uid 101 on port 8080 and writes only under /tmp.
FROM nginxinc/nginx-unprivileged:1.29-alpine@sha256:0c79d56aee561a1d81c63f00eee5fb5fe29279560cdc55e91425133104c7fbe6

ARG HUB_URL=https://clankmail.zacx.dev
ARG LAUNCH_OWNERS=k_ildi4gc4czoegaf4b526pmc2pmymrj7tqm5zbp7w7pvd57s3zvya

USER root
# The image's default server block and its startup helpers (templating, IPv6
# listen rewrite) are not used: the config is complete at build time.
RUN rm -f /etc/nginx/conf.d/default.conf /usr/share/nginx/html/* \
 && rm -rf /etc/nginx/templates
COPY site/nginx.conf /etc/nginx/nginx.conf
COPY site/index.html /usr/share/nginx/html/index.html
# Both values land in a CSP or in HTML, so each is checked as a WHOLE string,
# never line by line (grep alone would pass a value whose FIRST line is good).
#   HUB_URL: an origin only, scheme://host[:port]. The `case` refuses any
#     character outside a small set, newline included, so the grep after it
#     sees exactly one line.
#   LAUNCH_OWNERS: one or more k_ ids (k_ + 52 base32 characters), comma-
#     separated. Every comma- or newline-separated piece, empty ones included,
#     is read as its own line and must match whole, so a newline, an empty
#     item or a stray character anywhere refuses the value.
RUN set -eu; \
    bad_hub() { echo "HUB_URL must be scheme://host[:port], got [$HUB_URL]" >&2; exit 1; }; \
    case "$HUB_URL" in ''|*[!A-Za-z0-9.:/-]*) bad_hub ;; esac; \
    printf '%s' "$HUB_URL" | grep -Eqx 'https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?' || bad_hub; \
    bad_owners() { echo "LAUNCH_OWNERS must be comma-separated k_ ids, got [$LAUNCH_OWNERS]" >&2; exit 1; }; \
    printf '%s\n' "$LAUNCH_OWNERS" | tr ',' '\n' | while read -r k; do \
      printf '%s' "$k" | grep -Eqx 'k_[a-z2-7]{52}' || exit 1; \
    done || bad_owners; \
    sed -i -e "s|__HUB_URL__|$HUB_URL|g" -e "s|__LAUNCH_OWNERS__|$LAUNCH_OWNERS|g" \
      /etc/nginx/nginx.conf /usr/share/nginx/html/index.html; \
    if grep -q '__HUB_URL__\|__LAUNCH_OWNERS__' /etc/nginx/nginx.conf /usr/share/nginx/html/index.html; then exit 1; fi; \
    grep -q "content=\"$LAUNCH_OWNERS\"" /usr/share/nginx/html/index.html; \
    nginx -t -q
USER 101

EXPOSE 8080
