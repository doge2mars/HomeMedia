# HomeMedia: Go + SQLite, multi-stage build.
# NOTE: mattn/go-sqlite3 needs CGO, so the builder must carry gcc.
# The binary serves web/ from the directory next to the binary itself
# (see main.go), so the runtime image keeps /app/home-media + /app/web together.
FROM golang:1.23-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=1 go build -trimpath -o /out/home-media .

FROM debian:bookworm-slim
# curl is only for the HEALTHCHECK below.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /out/home-media /app/home-media
COPY web /app/web
RUN chmod -R 755 /app && mkdir -p /data && chmod 777 /data
EXPOSE 8090
# The container is meant to run as a non-root user (see `user:` in docker-compose.yml);
# nothing in the image needs root at runtime, only the mounted /data must be writable.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${HOME_MEDIA_PORT:-8090}/api/ping" || exit 1
CMD ["/app/home-media"]
