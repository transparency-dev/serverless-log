FROM golang:1.21.5-alpine3.17@sha256:92cb87af996ec6befc85f0aec27e12ead2fab396695fa8a7abff79e021e58195 AS build

WORKDIR /src/
# Note: this could be a bit surprising as folks who pinned their action to some particular version
# will end up running the latest binaries. For now this is probably "ok", but when serverless is
# moved out into its own repo (where releases can be done) we should fix this behaviour.
RUN CGO_ENABLED=0 go install github.com/transparency-dev/serverless-log/cmd/integrate@HEAD
RUN CGO_ENABLED=0 go install github.com/transparency-dev/serverless-log/cmd/sequence@HEAD

FROM alpine:3.19.0@sha256:51b67269f354137895d43f3b3d810bfacd3945438e94dc5ac55fdac340352f48

RUN apk add --no-cache bash git

COPY entrypoint.sh /entrypoint.sh
COPY --from=build /go/bin/integrate /bin/integrate
COPY --from=build /go/bin/sequence /bin/sequence

ENTRYPOINT ["/entrypoint.sh"]
