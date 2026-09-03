#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 3 || ( "$3" != internal && "$3" != public ) ]]; then
  echo "usage: $0 <image-tag> <image-registry-prefix> <internal|public>" >&2
  exit 64
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$script_dir/readiness.sh"
backend_dir="${TELLYOUWHAT_BACKEND_DIR:-/opt/tellyouwhat/backend}"
image_tag="$1"
image_registry_prefix="$2"
deployment_mode="$3"
response_file=$(mktemp)
deployment_env=''

cd "$backend_dir"

cleanup_registry_login() {
  rm -f "$response_file"
  if [[ -n "$deployment_env" ]]; then rm -f "$deployment_env"; fi
  docker logout ghcr.io >/dev/null 2>&1 || true
}
trap cleanup_registry_login EXIT

test -s .env.production
test -s secrets/health-subscription.p8
test -s secrets/journal-subscription.p8
test -s secrets/health-marketing.p8
test -s secrets/journal-marketing.p8

if grep -Eiq '=(REPLACE_WITH_|ep-replace-|.*your-account/)' .env.production; then
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

health_api_domain=$(sed -n 's/^HEALTH_API_DOMAIN=//p' .env.production)
journal_api_domain=$(sed -n 's/^JOURNAL_API_DOMAIN=//p' .env.production)
admin_domain=$(sed -n 's/^ADMIN_DOMAIN=//p' .env.production)
if [[ -z "$health_api_domain" || -z "$journal_api_domain" || -z "$admin_domain" ]]; then
  echo 'HEALTH_API_DOMAIN, JOURNAL_API_DOMAIN, and ADMIN_DOMAIN are required.' >&2
  exit 1
fi

attempts="${READINESS_ATTEMPTS:-18}"
if [[ ! "$attempts" =~ ^[1-9][0-9]*$ ]]; then
  echo 'READINESS_ATTEMPTS must be a positive integer.' >&2
  exit 64
fi

"${compose[@]}" config --quiet
"${compose[@]}" pull gateway worker admin adminctl migrate maintenance
if [[ "$deployment_mode" == public ]]; then "${compose[@]}" pull caddy; fi
"${compose[@]}" run --rm --no-deps migrate
"${compose[@]}" up -d --no-build gateway worker admin

internal_ready=0
for ((attempt = 1; attempt <= attempts; attempt++)); do
  if check_readiness http://127.0.0.1:18080/readyz "$health_api_domain" ready "$response_file" &&
    check_readiness http://127.0.0.1:18080/readyz "$journal_api_domain" ready "$response_file" &&
    check_readiness http://127.0.0.1:18081/healthz localhost ok "$response_file" &&
    check_readiness http://127.0.0.1:18082/readyz "$admin_domain" ready "$response_file"; then
    internal_ready=1
    break
  fi
  if ((attempt < attempts)); then sleep 5; fi
done

if [[ "$internal_ready" -ne 1 ]]; then
  echo 'Internal gateway, worker, or admin readiness failed.' >&2
  "${compose[@]}" ps
  "${compose[@]}" logs --tail=100 gateway worker admin
  exit 1
fi

deployment_env=$(mktemp "$backend_dir/.env.production.XXXXXX")
awk -v tag="$image_tag" -v prefix="$image_registry_prefix" '
  /^(IMAGE_TAG|IMAGE_REGISTRY_PREFIX)=/ { next }
  { print }
  END {
    print "IMAGE_TAG=" tag
    print "IMAGE_REGISTRY_PREFIX=" prefix
  }
' .env.production > "$deployment_env"
mv "$deployment_env" .env.production
deployment_env=''

printf 'Internal deployment verified: %s\n' "$image_tag"
if [[ "$deployment_mode" == public ]]; then
  "${compose[@]}" up -d --no-build caddy
  echo 'Public proxy started. Public HTTPS verification is required separately.'
else
  echo 'Public proxy activation and public HTTPS verification remain pending.'
fi

"${compose[@]}" ps
