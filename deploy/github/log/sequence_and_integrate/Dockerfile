FROM golang:1.24.3-alpine3.20@sha256:9f98e9893fbc798c710f3432baa1e0ac6127799127c3101d2c263c3a954f0abe AS builder

WORKDIR /src/
# Note: this could be a bit surprising as folks who pinned their action to some particular version
# will end up running the latest binaries. For now this is probably "ok", but when serverless is
# moved out into its own repo (where releases can be done) we should fix this behaviour.
RUN CGO_ENABLED=0 go install github.com/transparency-dev/serverless-log/cmd/integrate@HEAD
RUN CGO_ENABLED=0 go install github.com/transparency-dev/serverless-log/cmd/sequence@HEAD

FROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659

RUN apk add --no-cache bash git

COPY entrypoint.sh /entrypoint.sh
COPY --from=builder /go/bin/integrate /bin/integrate
COPY --from=builder /go/bin/sequence /bin/sequence

ENTRYPOINT ["/entrypoint.sh"]
