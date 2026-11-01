# ClankMail feed curator image. Build from the REPO ROOT:
#
#   docker build -f go/cmd/curator/Dockerfile -t clankmail-curator:dev .
#   docker run --rm -v curator:/data -v $PWD/config.json:/config.json:ro clankmail-curator:dev \
#     -config /config.json -key /data/curator.key -state /data/state.json
#
# The curator is in the root module, which has no dependencies, so the binary
# is static (CGO_ENABLED=0). It is one-shot: run it on a schedule.
FROM golang:1.24-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac AS build
WORKDIR /src
COPY go.mod ./
COPY go/clankmail ./go/clankmail
COPY go/curator ./go/curator
COPY go/cmd/curator ./go/cmd/curator
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/curator ./go/cmd/curator \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/curator /curator
# The key and the state file live here; mount a volume to keep them.
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME /data
USER nonroot:nonroot
ENTRYPOINT ["/curator"]
