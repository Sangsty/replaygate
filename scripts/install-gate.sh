#!/usr/bin/env bash
# Installs a released replaygate binary for the GitHub Action: downloads it,
# checks its SHA-256 against the release's checksums.txt, and copies it to
# $RG_DEST. Used when the Action is given a `version`.
#
#   RG_REPO=Sangsty/replaygate RG_VERSION=v0.1.0 RG_DEST=/tmp/replaygate \
#     scripts/install-gate.sh
#
# RG_RELEASE_BASE overrides the download location (the tests point it at a
# local directory).
set -euo pipefail

: "${RG_REPO:?set RG_REPO, e.g. Sangsty/replaygate}"
: "${RG_VERSION:?set RG_VERSION, e.g. v0.1.0}"
: "${RG_DEST:?set RG_DEST to the path to install to}"
BASE="${RG_RELEASE_BASE:-https://github.com/$RG_REPO/releases/download}"

# Both values end up in a URL, so accept only what a release tag and a
# repository name can look like.
if ! [[ "$RG_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
  echo "replaygate: version must look like v1.2.3, got '$RG_VERSION'" >&2
  exit 2
fi
if ! [[ "$RG_REPO" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]]; then
  echo "replaygate: repository must look like owner/name, got '$RG_REPO'" >&2
  exit 2
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in linux | darwin) ;; *)
  echo "replaygate: no prebuilt binary for $os; leave 'version' empty to build from source" >&2
  exit 2
  ;;
esac
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *)
    echo "replaygate: no prebuilt binary for $arch; leave 'version' empty to build from source" >&2
    exit 2
    ;;
esac
name="replaygate_${os}_${arch}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 3 -o "$tmp/$name" "$BASE/$RG_VERSION/$name"
curl -fsSL --retry 3 -o "$tmp/checksums.txt" "$BASE/$RG_VERSION/checksums.txt"

want="$(awk -v n="$name" '$2 == n { print $1 }' "$tmp/checksums.txt")"
if [ -z "$want" ]; then
  echo "replaygate: checksums.txt has no entry for $name" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$name" | awk '{ print $1 }')"
else
  got="$(shasum -a 256 "$tmp/$name" | awk '{ print $1 }')"
fi
if [ "$got" != "$want" ]; then
  echo "replaygate: checksum mismatch for $name (expected $want, got $got); refusing to install" >&2
  exit 1
fi

install -m 0755 "$tmp/$name" "$RG_DEST"
echo "installed replaygate $RG_VERSION ($os/$arch) to $RG_DEST"
