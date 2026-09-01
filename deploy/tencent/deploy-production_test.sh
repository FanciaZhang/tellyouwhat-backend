#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT

backend_dir="$test_dir/backend"
fake_bin="$test_dir/bin"
command_log="$test_dir/commands.log"

mkdir -p "$backend_dir/secrets" "$fake_bin"
printf '%s\n' \
  'HEALTH_API_DOMAIN=api.health.tellyouwhat.cn' \
  'HEALTH_ADMIN_DOMAIN=admin.health.tellyouwhat.cn' \
  > "$backend_dir/.env.production"
touch "$backend_dir/compose.production.yaml"
printf 'test\n' > "$backend_dir/secrets/SubscriptionKey.p8"
printf 'test\n' > "$backend_dir/secrets/MarketingKey.p8"
printf 'test\n' > "$backend_dir/secrets/apple-app-store-roots.pem"

cat > "$fake_bin/docker" <<'SCRIPT'
#!/usr/bin/env bash
printf 'docker %s\n' "$*" >> "$DEPLOY_TEST_COMMAND_LOG"
SCRIPT

cat > "$fake_bin/curl" <<'SCRIPT'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$DEPLOY_TEST_COMMAND_LOG"
SCRIPT

chmod +x "$fake_bin/docker" "$fake_bin/curl"

PATH="$fake_bin:$PATH" \
DEPLOY_TEST_COMMAND_LOG="$command_log" \
HEALTH_BACKEND_DIR="$backend_dir" \
  "$script_dir/deploy-production.sh" test-tag test-prefix

grep -Fq \
  'docker compose --env-file .env.production -f compose.production.yaml pull gateway worker admin adminctl migrate maintenance' \
  "$command_log"
grep -Fq \
  'docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps migrate' \
  "$command_log"
grep -Fq \
  'docker compose --env-file .env.production -f compose.production.yaml up -d --no-build gateway worker admin caddy' \
  "$command_log"
grep -Fq \
  'curl -fsS http://127.0.0.1:18080/readyz' \
  "$command_log"
grep -Fq \
  'curl -kfsS --max-time 8 --resolve api.health.tellyouwhat.cn:443:127.0.0.1 https://api.health.tellyouwhat.cn/readyz' \
  "$command_log"
grep -Fq \
  'curl -kfsS --max-time 8 --resolve admin.health.tellyouwhat.cn:443:127.0.0.1 https://admin.health.tellyouwhat.cn/healthz' \
  "$command_log"
grep -Fq \
  'docker compose --env-file .env.production -f compose.production.yaml ps' \
  "$command_log"
