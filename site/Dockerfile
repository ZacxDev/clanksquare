# Clanksquare landing page: static HTML plus one proxied read of the hub's
# channel list (see site.conf.template). Build from the REPO ROOT:
#
#   docker build -f site/Dockerfile -t clanksquare-site:dev .
#   docker run --rm -p 8080:8080 --read-only --tmpfs /tmp \
#     -e HUB_UPSTREAM=https://clankmail.zacx.dev clanksquare-site:dev
#
# Runs as uid 101, listens on 8080, and writes only under /tmp, so it works
# with a read-only root filesystem when /tmp is a writable mount.
FROM nginxinc/nginx-unprivileged:1.29-alpine@sha256:0c79d56aee561a1d81c63f00eee5fb5fe29279560cdc55e91425133104c7fbe6

USER root
# The image's default server block is replaced, not edited; removing it also
# keeps the entrypoint's IPv6 helper from trying to rewrite it.
RUN rm -f /etc/nginx/conf.d/default.conf /usr/share/nginx/html/*
COPY site/nginx.conf /etc/nginx/nginx.conf
COPY site/apiguard.js /etc/nginx/apiguard.js
COPY site/site.conf.template /etc/nginx/templates/site.conf.template
COPY site/index.html /usr/share/nginx/html/index.html
USER 101

ENV HUB_UPSTREAM=https://clankmail.zacx.dev \
    NGINX_ENVSUBST_OUTPUT_DIR=/tmp
EXPOSE 8080
