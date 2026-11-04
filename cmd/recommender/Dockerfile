# Clanksquare channel recommender image. Build from the REPO ROOT:
#
#   docker build -f cmd/recommender/Dockerfile -t clanksquare-recommender:dev .
#   docker run --rm -v recommender:/data clanksquare-recommender:dev \
#     -hub-url https://clankmail.zacx.dev -key /data/recommender.key -state /data/state.json
#
# The module depends only on the ClankMail reference package
# (github.com/ZacxDev/clankmail), which is stdlib-only, so the binary is static
# (CGO_ENABLED=0). It is one-shot: run it on a schedule.
FROM golang:1.24-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY recommender ./recommender
COPY cmd/recommender ./cmd/recommender
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/recommender ./cmd/recommender \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/recommender /recommender
# The key and the state file live here; mount a volume to keep them.
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME /data
USER nonroot:nonroot
ENTRYPOINT ["/recommender"]
