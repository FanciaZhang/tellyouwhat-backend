#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <image-tag> <image-registry-prefix>" >&2
  exit 64
fi

backend_dir="${TELLYOUWHAT_BACKEND_DIR:-/opt/tellyouwhat/backend}"
image_tag="$1"
image_registry_prefix="$2"

cd "$backend_dir"

cleanup_registry_login() {
  docker logout ghcr.io >/dev/null 2>&1 || true
}
trap cleanup_registry_login EXIT

test -s .env.production
test -s secrets/health-subscription.p8
test -s secrets/journal-subscription.p8
test -s secrets/health-marketing.p8
test -s secrets/journal-marketing.p8

if grep -Eq '=REPLACE_WITH_' .env.production; then
  echo 'Production environment still contains placeholder values.' >&2
  exit 1
fi

export IMAGE_TAG="$image_tag"
export IMAGE_REGISTRY_PREFIX="$image_registry_prefix"

compose=(
  docker compose
  --env-file .env.production
  -f compose.production.yaml
)

"${compose[@]}" pull gateway worker admin adminctl migrate maintenance
"${compose[@]}" run --rm --no-deps migrate
"${compose[@]}" up -d --no-build gateway worker admin caddy

health_api_domain=$(sed -n 's/^HEALTH_API_DOMAIN=//p' .env.production)
journal_api_domain=$(sed -n 's/^JOURNAL_API_DOMAIN=//p' .env.production)
admin_domain=$(sed -n 's/^ADMIN_DOMAIN=//p' .env.production)
if [[ -z "$health_api_domain" || -z "$journal_api_domain" || -z "$admin_domain" ]]; then
  echo 'HEALTH_API_DOMAIN, JOURNAL_API_DOMAIN, and ADMIN_DOMAIN are required.' >&2
  exit 1
fi

gateway_ready=0
for _ in $(seq 1 18); do
  if curl -fsS -H "Host: $health_api_domain" http://127.0.0.1:18080/readyz >/dev/null &&
    curl -fsS -H "Host: $journal_api_domain" http://127.0.0.1:18080/readyz >/dev/null; then
    gateway_ready=1
    break
  fi
  sleep 5
done

if [[ "$gateway_ready" -ne 1 ]]; then
  "${compose[@]}" ps
  "${compose[@]}" logs --tail=100 gateway worker caddy
  exit 1
fi

public_ready=0
for _ in $(seq 1 24); do
  if curl -kfsS --max-time 8 \
    --resolve "$health_api_domain:443:127.0.0.1" \
    "https://$health_api_domain/readyz" >/dev/null &&
    curl -kfsS --max-time 8 \
      --resolve "$journal_api_domain:443:127.0.0.1" \
      "https://$journal_api_domain/readyz" >/dev/null &&
    curl -kfsS --max-time 8 \
      --resolve "$admin_domain:443:127.0.0.1" \
      "https://$admin_domain/readyz" >/dev/null; then
    public_ready=1
    break
  fi
  sleep 5
done

if [[ "$public_ready" -ne 1 ]]; then
  echo 'One or more public HTTPS endpoints did not become ready.' >&2
  "${compose[@]}" ps
  "${compose[@]}" logs --tail=150 admin caddy
  exit 1
fi

"${compose[@]}" ps
