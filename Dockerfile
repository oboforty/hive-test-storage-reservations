# --- build
FROM golang:1.27-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go, so CGO can stay disabled.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o server .


# --- runtime
FROM alpine:3.22

RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=builder /app/server .
RUN mkdir -p /app/data /app/logs && chown -R app:app /app
USER app

ENV ENV=PROD \
    HTTP_ADDR=:8080 \
    DB_PATH=/app/data/hive.db \
    LOG_FILE=/app/logs/app.log \
    HIVE_SETTINGS_FILE=/app/data/hivesettings.json

EXPOSE 8080
CMD ["./server", "serve", "-migrate"]
