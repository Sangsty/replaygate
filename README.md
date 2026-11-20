# replaygate

Traffic Replay Regression Gate: record sanitized API traffic through a reverse
proxy, replay it against the baseline (main) and candidate (PR) builds, diff
the responses, and fail the pull request with a readable report when a
breaking change is found.

> Status: Phase 1 (recorder, sanitizer, store). Replay workers, the diff
> engine, the GitHub Action and the dashboard follow in later phases.

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
