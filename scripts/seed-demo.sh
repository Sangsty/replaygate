#!/usr/bin/env bash
# Puts demo data into a replaygate server so its dashboard has something to show:
# it records traffic from a small local demo service, then runs two checks, one
# that fails (a build with seeded bugs) and one that passes.
#
# It runs on YOUR machine and talks to the server over HTTPS; nothing is copied
# to the server. Needs bash, curl, python3 and go.
#
#   RG_API_URL=https://13-232-1-2.sslip.io RG_PROJECT_ID=... RG_API_KEY=... \
#     bash scripts/seed-demo.sh
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
for v in RG_API_URL RG_PROJECT_ID RG_API_KEY; do
  [ -n "${!v:-}" ] || { echo "set $v (the project id and key come from 'bootstrap')"; exit 2; }
done
export RG_API_URL RG_PROJECT_ID RG_API_KEY

TMP="$(mktemp -d)"; pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null; wait 2>/dev/null; rm -rf "$TMP"; }
trap cleanup EXIT

echo "building..."
for b in proxy replaygate; do go build -o "$TMP/$b" "./cmd/$b" || exit 1; done

python3 "$ROOT/scripts/demo_app.py" 9101 baseline & pids+=($!)
python3 "$ROOT/scripts/demo_app.py" 9102 candidate & pids+=($!)
sleep 1

echo "recording demo traffic through the proxy..."
RG_UPSTREAM_URL=http://127.0.0.1:9101 RG_PROXY_ADDR=127.0.0.1:18091 "$TMP/proxy" 2>"$TMP/proxy.log" & PX=$!; pids+=($PX)
sleep 2
for i in 1 2 3 4 5 6; do curl -s -o /dev/null -H "Authorization: Bearer demo" "http://127.0.0.1:18091/users/$i"; done
for i in 1 2 3 4;     do curl -s -o /dev/null -H "Authorization: Bearer demo" "http://127.0.0.1:18091/orders/$i"; done
curl -s -o /dev/null "http://127.0.0.1:18091/nope"
sleep 2; kill -TERM $PX; sleep 1
if grep -q '"level":"\(WARN\|ERROR\)"' "$TMP/proxy.log" || ! grep -q '"uploaded":[1-9]' "$TMP/proxy.log"; then
  echo "the proxy could not upload (check RG_API_URL, the project id and the key):"; cat "$TMP/proxy.log"; exit 1
fi

export RG_COMMENT=false RG_REPLAY_AUTH="Bearer demo"
echo; echo "check 1: a build with seeded bugs (expected: FAILED)"
RG_BASELINE_URL=http://127.0.0.1:9101 RG_CANDIDATE_URL=http://127.0.0.1:9102 RG_COMMIT_SHA=abc1234 RG_PR_NUMBER=7 "$TMP/replaygate" run 2>"$TMP/run1.log"
S1=$?
echo; echo "check 2: the baseline against itself (expected: PASSED)"
RG_BASELINE_URL=http://127.0.0.1:9101 RG_CANDIDATE_URL=http://127.0.0.1:9101 RG_COMMIT_SHA=def5678 RG_PR_NUMBER=8 "$TMP/replaygate" run 2>"$TMP/run2.log"
S2=$?
echo
if [ "$S1" = 1 ] && [ "$S2" = 0 ]; then echo "seeded: open the dashboard and look at both runs"; else echo "unexpected exit codes ($S1, $S2); see:"; cat "$TMP/run1.log" "$TMP/run2.log"; exit 1; fi
