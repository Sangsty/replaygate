# syntax=docker/dockerfile:1
#
# Two images from one file:
#   docker build --target api -t replaygate-api .   the API (also runs `bootstrap`)
#   docker build --target web -t replaygate-web .   the dashboard behind Caddy
# docker-compose.prod.yml builds both.

# ---- Go binaries (static, no CGO) ----
FROM golang:1.23-alpine AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# ---- the API ----
FROM alpine:3.20 AS api
# ca-certificates: the API calls GitHub over TLS for sign-in.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 replaygate
COPY --from=gobuild /out/api /usr/local/bin/api
USER replaygate
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=5 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["api"]
CMD ["serve"]

# ---- the dashboard bundle ----
FROM node:22-alpine AS webbuild
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- Caddy: serves the bundle, proxies /v1 and /auth to the API, handles TLS ----
FROM caddy:2-alpine AS web
COPY deploy/Caddyfile /etc/caddy/Caddyfile
COPY --from=webbuild /web/dist /srv/web
