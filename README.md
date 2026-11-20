# replaygate

Traffic Replay Regression Gate: record sanitized API traffic through a reverse
proxy, replay it against the baseline (main) and candidate (PR) builds, diff
the responses, and fail the pull request with a readable report when a
breaking change is found.

> Status: Phase 2 (replay workers, job queue, diff engine, noise learning).
> The GitHub Action, PR comments, auth and dashboard follow in later phases.

## Quick start

```bash
cp .env.example .env
make up                      # Postgres 16 in Docker
go mod tidy                  # first run only: resolves deps and writes go.sum
make test                    # go test -race ./...
make bootstrap NAME=demo     # prints RG_PROJECT_ID and RG_API_KEY once
# paste both values into .env, and set RG_UPSTREAM_URL to your service
make api                     # terminal 1
make proxy                   # terminal 2
curl -H 'Authorization: Bearer x' 'http://localhost:8081/your/path'
```

Only GET requests are recorded. Authorization, cookies and any configured
headers, query params and JSON paths are redacted before anything leaves the
proxy. Production response bodies are never stored.

## Phase 2: replay and diff

A run replays the most-hit recorded requests three times: `baseline_a` and
`baseline_b` against the baseline (main) build, and `candidate` against the PR
build. Fields that differ between A and B are learned as noise and ignored;
everything else in A is diffed against the candidate.

```bash
./scripts/e2e.sh        # full demo with seeded bugs; needs `make up` first
```

Manual flow (three terminals plus the API):

```bash
make api                                                    # also runs the diff worker
curl -s -X POST -H "Authorization: Bearer $RG_API_KEY" \
  -H 'Content-Type: application/json' \
  "$RG_API_URL/v1/projects/$RG_PROJECT_ID/runs" -d '{"commit_sha":"abc1234"}'
RG_VARIANT=baseline_a RG_TARGET_URL=http://localhost:3000 RG_IDLE_EXIT=10s make worker
RG_VARIANT=baseline_b RG_TARGET_URL=http://localhost:3000 RG_IDLE_EXIT=10s make worker
RG_VARIANT=candidate  RG_TARGET_URL=http://localhost:3001 RG_IDLE_EXIT=10s make worker
curl -s -H "Authorization: Bearer $RG_API_KEY" \
  "$RG_API_URL/v1/projects/$RG_PROJECT_ID/runs/<run_id>"
```

Replay credentials for the target go in `RG_REPLAY_AUTH` on the worker (for
example `Bearer <token>`). They stay in that process and are never uploaded.
A run is `errored`, not `passed`, when the baseline did not answer.
