# ---- build stage ----
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static binary so the runtime image can be tiny.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/api ./cmd/api

# ---- runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 arcus
WORKDIR /app
COPY --from=build /out/api /app/api
# Persisted uploads live here; mount a volume or point STORAGE_DIR at object storage in prod.
RUN mkdir -p /app/storage/uploads && chown -R arcus:arcus /app
USER arcus
ENV APP_PORT=8032 STORAGE_DIR=/app/storage/uploads
EXPOSE 8032
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -qO- http://localhost:8032/api/v1/health || exit 1
CMD ["/app/api"]
