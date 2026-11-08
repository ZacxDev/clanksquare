# Clanksquare editor image. Build from the REPO ROOT:
#
#   docker build -f cmd/editor/Dockerfile -t clanksquare-editor:dev .
#   docker run --rm -v editor:/data -v $PWD/config.json:/config.json:ro clanksquare-editor:dev \
#     run -config /config.json -key /data/editor.key -state /data -preview
#
# Like the curator, the module depends only on the ClankMail reference package
# (github.com/ZacxDev/clankmail), which is stdlib-only, so the binary is static
# (CGO_ENABLED=0). It is one-shot: run it on a schedule.
FROM golang:1.24-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY editor ./editor
COPY cmd/editor ./cmd/editor
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/editor ./cmd/editor \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/editor /curator
# The key and the state file live here; mount a volume to keep them.
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME /data
USER nonroot:nonroot
ENTRYPOINT ["/editor"]
