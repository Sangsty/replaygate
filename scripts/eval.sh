#!/usr/bin/env bash
# Measures the gate and writes docs/evaluation.md (and docs/evaluation.json):
# how many seeded bugs it catches, how often it raises a false alarm, and how
# fast it replays and diffs. Needs bash, go and the Postgres from `make up`.
#
#   ./scripts/eval.sh                 # about a minute
#   ./scripts/eval.sh -fp-runs 100    # extra flags go to cmd/eval
#
# It creates (and drops) its own database, rg_eval, so your data is untouched.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
[ -f .env ] && { set -a; . ./.env; set +a; }

PSQL_CMD="${PSQL_CMD:-docker compose -f "$ROOT/docker-compose.yml" exec -T postgres psql -U ${POSTGRES_USER:-replaygate}}"
DB_BASE="${E2E_DB_BASE:-postgres://${POSTGRES_USER:-replaygate}:${POSTGRES_PASSWORD:-change-me}@localhost:5432}"
psql_() { $PSQL_CMD "$@"; }

drop() { psql_ -d postgres -qc 'DROP DATABASE IF EXISTS rg_eval' >/dev/null 2>&1; }
trap drop EXIT

psql_ -d postgres -qc 'DROP DATABASE IF EXISTS rg_eval' -c 'CREATE DATABASE rg_eval' 2>&1 | grep -v NOTICE
export DATABASE_URL="$DB_BASE/rg_eval?sslmode=disable"

mkdir -p docs
echo "running the evaluation (about a minute)..."
go run ./cmd/eval -strict -out docs/evaluation.md -json docs/evaluation.json "$@"
STATUS=$?
if [ "$STATUS" = 0 ]; then
  echo "wrote docs/evaluation.md"
else
  echo "eval exited with status $STATUS (see the messages above)"
fi
exit "$STATUS"
