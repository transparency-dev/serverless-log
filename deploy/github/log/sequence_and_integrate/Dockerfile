FROM golang:1.24.2-alpine3.20@sha256:00f149d5963f415a8a91943531b9092fde06b596b276281039604292d8b2b9c8 AS builder

WORKDIR /src/
# Note: this could be a bit surprising as folks who pinned their action to some particular version
# will end up running the latest binaries. For now this is probably "ok", but when serverless is
# moved out into its own repo (where releases can be done) we should fix this behaviour.
RUN CGO_ENABLED=0 go install github.com/transparency-dev/serverless-log/cmd/integrate@HEAD
RUN CGO_ENABLED=0 go install github.com/transparency-dev/serverless-log/cmd/sequence@HEAD

FROM alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c

RUN apk add --no-cache bash git

COPY entrypoint.sh /entrypoint.sh
COPY --from=builder /go/bin/integrate /bin/integrate
COPY --from=builder /go/bin/sequence /bin/sequence

ENTRYPOINT ["/entrypoint.sh"]
