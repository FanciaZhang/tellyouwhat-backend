#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <image-tag> <image-registry-prefix>" >&2
  exit 64
fi

backend_dir="${HEALTH_BACKEND_DIR:-/opt/health/Backend}"
image_tag="$1"
image_registry_prefix="$2"

cd "$backend_dir"

cleanup_registry_login() {
  docker logout ghcr.io >/dev/null 2>&1 || true
}
trap cleanup_registry_login EXIT

test -s .env.production
test -s secrets/SubscriptionKey.p8
test -s secrets/MarketingKey.p8
test -s secrets/apple-app-store-roots.pem

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

ready=0
for _ in $(seq 1 18); do
  if curl -fsS http://127.0.0.1:18080/readyz >/dev/null; then
    ready=1
    break
  fi
  sleep 5
done

if [[ "$ready" -ne 1 ]]; then
  "${compose[@]}" ps
  "${compose[@]}" logs --tail=100 gateway worker caddy
  exit 1
fi

health_api_domain=$(sed -n 's/^HEALTH_API_DOMAIN=//p' .env.production)
if [[ -z "$health_api_domain" ]]; then
  echo 'HEALTH_API_DOMAIN is missing from production configuration.' >&2
  exit 1
fi

https_ready=0
for _ in $(seq 1 24); do
  if curl -kfsS --max-time 8 \
    --resolve "$health_api_domain:443:127.0.0.1" \
    "https://$health_api_domain/readyz" >/dev/null; then
    https_ready=1
    break
  fi
  sleep 5
done

if [[ "$https_ready" -ne 1 ]]; then
  echo 'Caddy HTTPS endpoint did not become ready.' >&2
  "${compose[@]}" ps
  "${compose[@]}" logs --tail=150 gateway worker caddy
  exit 1
fi

health_admin_domain=$(sed -n 's/^HEALTH_ADMIN_DOMAIN=//p' .env.production)
if [[ -z "$health_admin_domain" ]]; then
  echo 'HEALTH_ADMIN_DOMAIN is missing from production configuration.' >&2
  exit 1
fi

admin_ready=0
for _ in $(seq 1 24); do
  if curl -kfsS --max-time 8 \
    --resolve "$health_admin_domain:443:127.0.0.1" \
    "https://$health_admin_domain/healthz" >/dev/null; then
    admin_ready=1
    break
  fi
  sleep 5
done

if [[ "$admin_ready" -ne 1 ]]; then
  echo 'Admin HTTPS endpoint did not become ready.' >&2
  "${compose[@]}" ps
  "${compose[@]}" logs --tail=150 admin caddy
  exit 1
fi

"${compose[@]}" ps
