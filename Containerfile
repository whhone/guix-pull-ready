# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/guix-sync-server ./cmd/guix-sync-server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 guixsync
USER guixsync
WORKDIR /data
COPY --from=builder /out/guix-sync-server /usr/local/bin/guix-sync-server
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/guix-sync-server"]
CMD ["--listen=:8080", "--db-path=/data/guix-sync.db"]
