#!/usr/bin/env bash
# End-to-end demo: record traffic, replay it against a baseline and a candidate
# build with seeded bugs, and print the gate's verdict with and without noise
# learning. Needs: bash, curl, python3, go, and the Postgres from `make up`.
#
#   ./scripts/e2e.sh
#
# It creates (and drops) its own database, rg_e2e, so your data is untouched.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
[ -f .env ] && { set -a; . ./.env; set +a; }

# How to reach Postgres. Defaults match docker-compose.yml.
PSQL_CMD="${PSQL_CMD:-docker compose -f "$ROOT/docker-compose.yml" exec -T postgres psql -U ${POSTGRES_USER:-replaygate}}"
DB_BASE="${E2E_DB_BASE:-postgres://${POSTGRES_USER:-replaygate}:${POSTGRES_PASSWORD:-change-me}@localhost:5432}"
psql_() { $PSQL_CMD "$@"; }

TMP="$(mktemp -d)"; pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null; wait 2>/dev/null; rm -rf "$TMP"; }
trap cleanup EXIT

echo "building..."
for b in api proxy worker replaygate; do go build -o "$TMP/$b" "./cmd/$b" || exit 1; done
psql_ -d postgres -qc 'DROP DATABASE IF EXISTS rg_e2e' -c 'CREATE DATABASE rg_e2e' 2>&1 | grep -v NOTICE

cd "$TMP"   # no .env here, so only the variables set below apply
export DATABASE_URL="$DB_BASE/rg_e2e?sslmode=disable"
export RG_API_ADDR=127.0.0.1:18090 RG_API_URL=http://127.0.0.1:18090

OUT=$("$TMP/api" bootstrap -name e2e 2>/dev/null) || { echo "bootstrap failed (is Postgres up? try: make up)"; exit 1; }
export RG_PROJECT_ID=$(echo "$OUT" | sed -n 's/^RG_PROJECT_ID=//p') RG_API_KEY=$(echo "$OUT" | sed -n 's/^RG_API_KEY=//p')
AUTH="Authorization: Bearer $RG_API_KEY"; P="$RG_API_URL/v1/projects/$RG_PROJECT_ID"

python3 "$ROOT/scripts/demo_app.py" 9101 baseline & pids+=($!)
python3 "$ROOT/scripts/demo_app.py" 9102 candidate & pids+=($!)
"$TMP/api" serve 2>"$TMP/api.log" & pids+=($!)
sleep 2

echo; echo "== 1. record traffic through the proxy =="
RG_UPSTREAM_URL=http://127.0.0.1:9101 RG_PROXY_ADDR=127.0.0.1:18091 "$TMP/proxy" 2>"$TMP/proxy.log" & PX=$!; pids+=($PX)
sleep 2
for i in 1 2 3 4 5 6; do curl -s -o /dev/null -H "Authorization: Bearer prodsecret" "http://127.0.0.1:18091/users/$i"; done
for i in 1 2 3 4;     do curl -s -o /dev/null -H "Authorization: Bearer prodsecret" "http://127.0.0.1:18091/orders/$i"; done
curl -s -o /dev/null "http://127.0.0.1:18091/nope"
sleep 2; kill -TERM $PX; sleep 1
echo "recorded requests: $(psql_ -U "${POSTGRES_USER:-replaygate}" -d rg_e2e -Atc 'select count(*) from recorded_requests')"

replay() { # $1 = noise_learning true|false
  local RUN RID W=() ST
  RUN=$(curl -s -X POST -H "$AUTH" -H 'Content-Type: application/json' "$P/runs" \
        -d "{\"commit_sha\":\"abc1234\",\"pr_number\":7,\"batch_size\":4,\"noise_learning\":$1}")
  RID=$(echo "$RUN" | python3 -c 'import sys,json;print(json.load(sys.stdin)["run_id"])') || { echo "create run failed: $RUN"; return; }
  for spec in "baseline_a http://127.0.0.1:9101" "baseline_b http://127.0.0.1:9101" "candidate http://127.0.0.1:9102"; do
    set -- $spec
    RG_VARIANT=$1 RG_TARGET_URL=$2 RG_IDLE_EXIT=3s RG_REPLAY_AUTH="Bearer replaytoken" "$TMP/worker" 2>"$TMP/worker_$1.log" & W+=($!)
  done
  wait "${W[@]}"
  for _ in $(seq 1 20); do
    ST=$(curl -s -H "$AUTH" "$P/runs/$RID" | python3 -c 'import sys,json;print(json.load(sys.stdin)["run"]["status"])')
    case "$ST" in passed|failed|errored) break;; esac; sleep 0.5
  done
  curl -s -H "$AUTH" "$P/runs/$RID" | python3 -c '
import sys, json
d = json.load(sys.stdin); r = d["run"]
print("verdict: %s | requests=%d breaking=%d additive=%d latency=%d %s" % (r["status"].upper(), r["total_requests"], r["breaking_count"], r["additive_count"], r["latency_count"], r.get("error", "")))
for x in d["diffs"]:
    print("  %-8s %-14s %-13s %-12s %s -> %s" % (x["severity"], x["kind"], x["endpoint"], x["json_path"], x.get("baseline_value"), x.get("candidate_value")))'
  psql_ -U "${POSTGRES_USER:-replaygate}" -d rg_e2e -Atc "select 'learned noise: '||endpoint||' '||json_path from noise_fields where run_id='$RID' order by 1"
}
FAILED=0
expect() { # expect <what> <text> <pattern>
  case "$2" in *"$3"*) ;; *) echo "ASSERTION FAILED: $1 (wanted: $3)"; FAILED=1 ;; esac
}

echo; echo "== 2. replay and diff WITH noise learning =="
OUT=$(replay true); echo "$OUT"
expect "3 seeded bugs found, noise filtered out" "$OUT" "breaking=3 additive=4"
expect "noise was learned" "$OUT" 'learned noise: /users/{id} $.ts'

echo; echo "== 3. replay and diff WITHOUT noise learning (shows the false positives) =="
OUT=$(replay false); echo "$OUT"
expect "without noise learning, the noisy fields are false positives" "$OUT" "breaking=13"

echo; echo "== 4. the CLI, as the GitHub Action runs it =="
RG_BASELINE_URL=http://127.0.0.1:9101 RG_CANDIDATE_URL=http://127.0.0.1:9102 RG_COMMIT_SHA=abc1234 RG_PR_NUMBER=7 \
  RG_REPORT_FILE="$TMP/report.md" RG_REPLAY_AUTH="Bearer replaytoken" "$TMP/replaygate" run 2>"$TMP/cli.log"
CLI_EXIT=$?
echo "exit status: $CLI_EXIT (1 means a regression was found, which is correct here)"
echo "--- the pull request comment would read: ---"
cat "$TMP/report.md"
[ "$CLI_EXIT" = 1 ] || { echo "ASSERTION FAILED: CLI exit status was $CLI_EXIT, wanted 1"; FAILED=1; }
expect "report names the breaking changes" "$(cat "$TMP/report.md")" "Breaking changes (3)"

echo; echo "== 5. secrets =="
SECRETS_DB="$(psql_ -U "${POSTGRES_USER:-replaygate}" -d rg_e2e -Atc "select (select count(*) from recorded_requests where headers::text ~ 'prodsecret') + (select count(*) from replay_responses where convert_from(body,'UTF8') ~ 'replaytoken')" 2>&1)"
case "$SECRETS_DB" in
  ''|*[!0-9]*) echo "tokens found in database: CHECK FAILED -> $SECRETS_DB" ;;
  *) echo "tokens found in database: $SECRETS_DB" ;;
esac
LOG_HITS="$(cat "$TMP"/*.log | grep -c -E 'prodsecret|replaytoken')"
echo "tokens found in logs: $LOG_HITS"
[ "$SECRETS_DB" = 0 ] || { echo "ASSERTION FAILED: tokens in the database"; FAILED=1; }
[ "$LOG_HITS" = 0 ] || { echo "ASSERTION FAILED: tokens in the logs"; FAILED=1; }
psql_ -d postgres -qc 'DROP DATABASE IF EXISTS rg_e2e' >/dev/null 2>&1

echo
if [ "$FAILED" = 0 ]; then echo "e2e: all checks passed"; else echo "e2e: FAILED"; exit 1; fi
