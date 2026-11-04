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
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /out/home-media /app/home-media
COPY web /app/web
EXPOSE 8080
CMD ["/app/home-media"]
