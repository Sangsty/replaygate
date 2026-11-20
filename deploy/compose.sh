#!/usr/bin/env bash
# Runs docker compose for the production stack from anywhere, with the right
# files, so you do not type them every time:
#
#   deploy/compose.sh ps
#   deploy/compose.sh logs -f api
#   deploy/compose.sh up -d --build
#   deploy/compose.sh run --rm api bootstrap -name demo -owner <github login>
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
[ -f "$ROOT/.env.prod" ] || { echo "no .env.prod yet: run deploy/init-env.sh <public IP> first" >&2; exit 1; }
cd "$ROOT"
exec docker compose --env-file .env.prod -f docker-compose.prod.yml "$@"
