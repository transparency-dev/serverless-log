FROM golang:1.25.5-alpine3.21@sha256:b4dbd292a0852331c89dfd64e84d16811f3e3aae4c73c13d026c4d200715aff6 AS builder

WORKDIR /src/
# Note: this could be a bit surprising as folks who pinned their action to some particular version
# will end up running the latest binaries. For now this is probably "ok", but when serverless is
# moved out into its own repo (where releases can be done) we should fix this behaviour.
RUN CGO_ENABLED=0 go install github.com/transparency-dev/serverless-log/cmd/integrate@HEAD
RUN CGO_ENABLED=0 go install github.com/transparency-dev/serverless-log/cmd/sequence@HEAD

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

RUN apk add --no-cache bash git

COPY entrypoint.sh /entrypoint.sh
COPY --from=builder /go/bin/integrate /bin/integrate
COPY --from=builder /go/bin/sequence /bin/sequence

ENTRYPOINT ["/entrypoint.sh"]
