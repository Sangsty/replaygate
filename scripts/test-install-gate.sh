#!/usr/bin/env bash
# Tests scripts/install-gate.sh against a fake release directory. No network.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
FAILED=0
ok()   { echo "ok    $1"; }
fail() { echo "FAIL  $1"; FAILED=1; }

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; esac
name="replaygate_${os}_${arch}"
sum() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }

# A release with one binary and its checksum file.
REL="$TMP/rel/v1.2.3"; mkdir -p "$REL"
printf '#!/bin/sh\necho fake replaygate\n' > "$REL/$name"
echo "$(sum "$REL/$name")  $name" > "$REL/checksums.txt"
echo "0000000000000000000000000000000000000000000000000000000000000000  replaygate_plan9_amd64" >> "$REL/checksums.txt"

install() { # install <version> [extra env...]; returns the script's status
  local v="$1"; shift
  env RG_REPO=Sangsty/replaygate RG_VERSION="$v" RG_DEST="$TMP/out" RG_RELEASE_BASE="file://$TMP/rel" "$@" \
    bash "$ROOT/scripts/install-gate.sh" >"$TMP/log" 2>&1
}

rm -f "$TMP/out"
if install v1.2.3 && [ -x "$TMP/out" ] && [ "$("$TMP/out")" = "fake replaygate" ]; then ok "installs a release whose checksum matches"; else fail "good install ($(cat "$TMP/log"))"; fi

# Tamper with the binary: the checksum no longer matches, nothing is installed.
printf '#!/bin/sh\necho evil\n' > "$REL/$name"; rm -f "$TMP/out"
if ! install v1.2.3 && [ ! -e "$TMP/out" ] && grep -q "checksum mismatch" "$TMP/log"; then ok "refuses a binary whose checksum differs"; else fail "tampered binary was accepted"; fi
printf '#!/bin/sh\necho fake replaygate\n' > "$REL/$name"

# No entry for this platform.
grep -v " $name\$" "$REL/checksums.txt" > "$TMP/c" && cp "$TMP/c" "$REL/checksums.txt"; rm -f "$TMP/out"
if ! install v1.2.3 && grep -q "no entry for $name" "$TMP/log"; then ok "refuses when checksums.txt lacks the platform"; else fail "missing checksum entry"; fi
echo "$(sum "$REL/$name")  $name" >> "$REL/checksums.txt"

# A release that does not exist.
if ! install v9.9.9; then ok "fails when the release does not exist"; else fail "missing release succeeded"; fi

# Versions that are not release tags never reach a URL.
for bad in latest v1 1.2.3 "v1.2.3/../../x" 'v1.2.3;rm -rf /' "v1.2.3 " '$(id)' "v1.2.3?x=1"; do
  rm -f "$TMP/out"
  if install "$bad" >/dev/null 2>&1; then fail "accepted version '$bad'"; else
    if grep -q "version must look like" "$TMP/log" && [ ! -e "$TMP/out" ]; then ok "rejects version '$bad'"; else fail "version '$bad' failed for the wrong reason: $(cat "$TMP/log")"; fi
  fi
done
if install v1.2.3-rc.1 >/dev/null 2>&1; then fail "rc without a release should fail on download"; else grep -q "version must look like" "$TMP/log" && fail "rejected a valid pre-release tag" || ok "accepts pre-release tags (then fails: not in the fake release)"; fi

# Bad repository names.
for bad in "evil" "a/b/c" "a b/c" 'a/$(id)'; do
  if install v1.2.3 RG_REPO="$bad" >/dev/null 2>&1; then fail "accepted repo '$bad'"; else grep -q "owner/name" "$TMP/log" && ok "rejects repo '$bad'" || fail "repo '$bad' failed for the wrong reason: $(cat "$TMP/log")"; fi
done

# Required variables.
if ! env -u RG_VERSION RG_REPO=a/b RG_DEST="$TMP/o" bash "$ROOT/scripts/install-gate.sh" >/dev/null 2>&1; then ok "requires RG_VERSION"; else fail "ran without RG_VERSION"; fi

[ "$FAILED" = 0 ] && echo "install-gate: all checks passed" || { echo "install-gate: FAILED"; exit 1; }
