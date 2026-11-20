#!/usr/bin/env bash
# Writes .env.prod for the production stack with freshly generated secrets, so
# you never have to invent, type or paste them.
#
#   deploy/init-env.sh 13.232.1.2          # your server's public IP
#
# The hostname is the IP with dashes plus .sslip.io (free; Caddy fetches the
# HTTPS certificate for it). Edit .env.prod afterwards to add the GitHub OAuth
# client id and secret.
set -euo pipefail

ip="${1:-}"
out="${2:-.env.prod}"
if ! [[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
  echo "usage: deploy/init-env.sh <public IPv4 address, e.g. 13.232.1.2> [output file]" >&2
  exit 2
fi
if [ -e "$out" ]; then
  echo "$out already exists; remove it first if you really want new secrets (the database password would no longer match)" >&2
  exit 1
fi
command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 1; }

host="${ip//./-}.sslip.io"
umask 077 # only the owner can read the file
cat > "$out" <<ENV
POSTGRES_PASSWORD=$(openssl rand -hex 24)
RG_HOST=$host
RG_PUBLIC_URL=https://$host
RG_GITHUB_CLIENT_ID=
RG_GITHUB_CLIENT_SECRET=
RG_JWT_SECRET=$(openssl rand -base64 48 | tr -d '\n')
ENV
echo "wrote $out (readable only by you)"
echo "dashboard address:  https://$host"
echo "GitHub OAuth callback to register:  https://$host/auth/github/callback"
