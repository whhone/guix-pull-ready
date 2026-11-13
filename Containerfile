# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/guix-pull-ready ./cmd/guix-pull-ready

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 guix
ENV GOGC=50 GOMEMLIMIT=256MiB
USER guix
WORKDIR /data
COPY --from=builder /out/guix-pull-ready /usr/local/bin/guix-pull-ready
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/guix-pull-ready"]
CMD ["--listen=:8080", "--db-path=/data/guix-pull-ready.db"]
