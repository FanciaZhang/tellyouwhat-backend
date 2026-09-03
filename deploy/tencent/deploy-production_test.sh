#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/../.." && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT

for certificate in deploy/Apple_App_Attestation_Root_CA.pem deploy/apple-app-store-roots.pem; do
  test -s "$repo_dir/$certificate"
  grep -Fxq "!$certificate" "$repo_dir/.dockerignore"
done

backend_dir="$test_dir/backend"
fake_bin="$test_dir/bin"
command_log="$test_dir/commands.log"
mkdir -p "$backend_dir/secrets" "$fake_bin"
reset_environment() {
  printf '%s\n' \
  'IMAGE_TAG=previous-tag' \
  'IMAGE_REGISTRY_PREFIX=previous-prefix' \
  'HEALTH_API_DOMAIN=api.health.tellyouwhat.cn' \
  'JOURNAL_API_DOMAIN=api.journal.tellyouwhat.cn' \
  'ADMIN_DOMAIN=admin.tellyouwhat.cn' > "$backend_dir/.env.production"
}
reset_environment
touch "$backend_dir/compose.production.yaml"
for key in health-subscription journal-subscription health-marketing journal-marketing; do
  printf 'test\n' > "$backend_dir/secrets/$key.p8"
done

cat > "$fake_bin/docker" <<'SCRIPT'
#!/usr/bin/env bash
printf 'docker %s\n' "$*" >> "$DEPLOY_TEST_COMMAND_LOG"
if [[ "${DEPLOY_TEST_FAILURE:-}" == migration && "$*" == *'run --rm --no-deps migrate'* ]]; then exit 1; fi
SCRIPT

cat > "$fake_bin/curl" <<'SCRIPT'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$DEPLOY_TEST_COMMAND_LOG"
response_file=''
url=''
while (($#)); do
  case "$1" in
    --output) response_file="$2"; shift ;;
    http*) url="$1" ;;
  esac
  shift
done
if [[ "${DEPLOY_TEST_FAILURE:-}" == admin && "$url" == *18082* ]]; then exit 22; fi
if [[ "${DEPLOY_TEST_FAILURE:-}" == worker && "$url" == *18081* ]]; then exit 7; fi
if [[ "${DEPLOY_TEST_FAILURE:-}" == redirect ]]; then printf '302'; exit; fi
status=ready
if [[ "$url" == */healthz ]]; then status=ok; fi
if [[ "${DEPLOY_TEST_FAILURE:-}" == not-ready ]]; then status=starting; fi
printf '{"status":"%s"}' "$status" > "$response_file"
printf '200'
SCRIPT
chmod +x "$fake_bin/docker" "$fake_bin/curl"

run_deploy() {
  : > "$command_log"
  PATH="$fake_bin:$PATH" READINESS_ATTEMPTS=1 \
    DEPLOY_TEST_COMMAND_LOG="$command_log" DEPLOY_TEST_FAILURE="${2:-}" \
    TELLYOUWHAT_BACKEND_DIR="$backend_dir" \
    bash "$script_dir/deploy-production.sh" test-tag test-prefix "$1"
}

run_deploy internal
[[ $(grep -c '^IMAGE_TAG=' "$backend_dir/.env.production") -eq 1 ]]
grep -Fxq 'IMAGE_TAG=test-tag' "$backend_dir/.env.production"
grep -Fxq 'IMAGE_REGISTRY_PREFIX=test-prefix' "$backend_dir/.env.production"
grep -Fxq 'HEALTH_API_DOMAIN=api.health.tellyouwhat.cn' "$backend_dir/.env.production"
grep -Fq 'up -d --no-build gateway worker admin' "$command_log"
for port in 18080 18081 18082; do grep -Fq "127.0.0.1:$port" "$command_log"; done
if grep -Eq 'https://|up .*caddy|pull caddy' "$command_log"; then
  echo 'Internal deployment must not activate the public proxy or require HTTPS.' >&2
  exit 1
fi

run_deploy public
grep -Fq 'pull caddy' "$command_log"
grep -Fq 'up -d --no-build caddy' "$command_log"

reset_environment
printf 'PUBLIC_PROXY_MODE=external\n' >> "$backend_dir/.env.production"
run_deploy public
grep -Fq 'up -d --no-build gateway worker admin' "$command_log"
for port in 18080 18081 18082; do grep -Fq "127.0.0.1:$port" "$command_log"; done
if grep -Eq 'up .*caddy|pull caddy' "$command_log"; then
  echo 'External proxy deployment must not start a competing Caddy container.' >&2
  exit 1
fi

reset_environment
printf 'PUBLIC_PROXY_MODE=invalid\n' >> "$backend_dir/.env.production"
if run_deploy public > "$test_dir/output" 2>&1; then
  echo 'Deployment accepted an invalid public proxy mode.' >&2
  exit 1
fi
if grep -Eq 'pull |run --rm|up -d' "$command_log"; then
  echo 'Invalid proxy mode changed running services.' >&2
  exit 1
fi

for failure in admin worker redirect not-ready migration; do
  reset_environment
  if run_deploy public "$failure" > "$test_dir/output" 2>&1; then
    printf 'Deployment accepted failure: %s\n' "$failure" >&2
    exit 1
  fi
  if grep -Fq 'up -d --no-build caddy' "$command_log"; then
    echo 'Public proxy started before internal readiness passed.' >&2
    exit 1
  fi
  grep -Fxq 'IMAGE_TAG=previous-tag' "$backend_dir/.env.production"
  grep -Fxq 'IMAGE_REGISTRY_PREFIX=previous-prefix' "$backend_dir/.env.production"
done

printf 'HEALTH_ARK_ENDPOINT_MEAL_PHOTO_CAPTURE=ep-replace-mini\n' >> "$backend_dir/.env.production"
if run_deploy internal > "$test_dir/output" 2>&1; then
  echo 'Deployment accepted a placeholder model endpoint.' >&2
  exit 1
fi
if grep -Eq 'pull |run --rm|up -d' "$command_log"; then
  echo 'Invalid configuration changed running services.' >&2
  exit 1
fi

if run_deploy invalid > "$test_dir/output" 2>&1; then
  echo 'Deployment accepted an invalid acceptance stage.' >&2
  exit 1
fi
echo 'Deployment stage and failure-path tests passed.'
