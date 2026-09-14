# ---------- base ----------
FROM golang:1.26-alpine AS base
WORKDIR /app
RUN apk add --no-cache git curl
COPY go.mod go.sum* ./
RUN go mod download

# ---------- dev: hot reload + migrate CLI ----------
FROM base AS dev
RUN go install github.com/air-verse/air@v1.67.4 \
 && go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.18.1
COPY . .
EXPOSE 8080
CMD ["air", "-c", ".air.toml"]

# ---------- build ----------
FROM base AS build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# ---------- prod: distroless-ish, non-root ----------
FROM alpine:3.20 AS prod
RUN apk add --no-cache ca-certificates curl \
 && adduser -D -u 10001 appuser
COPY --from=build /out/api /usr/local/bin/api
USER appuser
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD curl -fsS http://localhost:8080/health || exit 1
ENTRYPOINT ["/usr/local/bin/api"]
