#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
test_dir=$(mktemp -d)

cleanup() {
  find "$test_dir" -type f -delete
  rmdir "$test_dir"
}
trap cleanup EXIT

export MYSQL_PASSWORD=Database_Test_123
export REDIS_PASSWORD=Redis_Test_123
export PAYLOAD_ENCRYPTION_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
export WORKER_INTERNAL_SECRET=worker-internal-secret-for-tests
export JOB_CAPABILITY_SECRET=job-capability-secret-at-least-32-bytes
export APP_STORE_ISSUER_ID=00000000-0000-0000-0000-000000000000
export APP_STORE_KEY_ID=TESTKEY123
export APP_STORE_APP_APPLE_ID=1234567890
export APP_STORE_CONNECT_ISSUER_ID=11111111-1111-1111-1111-111111111111
export APP_STORE_CONNECT_KEY_ID=MARKETING1
export APP_STORE_CONNECT_SUBSCRIPTION_ID=subscription-resource-id
export ADMIN_PREVIEW_SIGNING_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
export ARK_API_KEY=ark-test-key
export TOS_ACCESS_KEY=tos-test-access-key
export TOS_SECRET_KEY=tos-test-secret-key

output_path="$test_dir/.env.production"
"$script_dir/render-production-env.sh" \
  "$script_dir/production.env.template" \
  "$output_path"

grep -Fqx 'MYSQL_PASSWORD=Database_Test_123' "$output_path"
grep -Fqx 'APP_STORE_APP_APPLE_ID=1234567890' "$output_path"
grep -Fqx 'APP_STORE_CONNECT_KEY_ID=MARKETING1' "$output_path"
grep -Fqx 'ADMIN_PREVIEW_SIGNING_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' \
  "$output_path"
grep -Fqx 'TOS_SECRET_KEY=tos-test-secret-key' "$output_path"
if grep -q '__GITHUB_ENVIRONMENT_SECRET__' "$output_path"; then
  echo "rendered environment contains a placeholder" >&2
  exit 1
fi

export TOS_ACCESS_KEY=$'tos-test-access-key\r\n'
"$script_dir/render-production-env.sh" \
  "$script_dir/production.env.template" \
  "$test_dir/trailing-newline.env"
grep -Fqx 'TOS_ACCESS_KEY=tos-test-access-key' \
  "$test_dir/trailing-newline.env"

export TOS_ACCESS_KEY=$'tos-test\naccess-key'
if "$script_dir/render-production-env.sh" \
  "$script_dir/production.env.template" \
  "$test_dir/embedded-newline.env" >/dev/null 2>&1; then
  echo "renderer accepted an embedded secret newline" >&2
  exit 1
fi

export TOS_ACCESS_KEY=tos-test-access-key
unset ARK_API_KEY
if "$script_dir/render-production-env.sh" \
  "$script_dir/production.env.template" \
  "$test_dir/missing.env" >/dev/null 2>&1; then
  echo "renderer accepted a missing required secret" >&2
  exit 1
fi

export ARK_API_KEY=ark-test-key
export MYSQL_PASSWORD='invalid/password'
if "$script_dir/render-production-env.sh" \
  "$script_dir/production.env.template" \
  "$test_dir/invalid.env" >/dev/null 2>&1; then
  echo "renderer accepted an unsafe MySQL password" >&2
  exit 1
fi

export MYSQL_PASSWORD=Database_Test_123
export PAYLOAD_ENCRYPTION_KEY=not-valid-base64
if "$script_dir/render-production-env.sh" \
  "$script_dir/production.env.template" \
  "$test_dir/invalid-key.env" >/dev/null 2>&1; then
  echo "renderer accepted an invalid payload encryption key" >&2
  exit 1
fi

export PAYLOAD_ENCRYPTION_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
export ADMIN_PREVIEW_SIGNING_KEY=not_valid_base64
if "$script_dir/render-production-env.sh" \
  "$script_dir/production.env.template" \
  "$test_dir/invalid-admin-key.env" >/dev/null 2>&1; then
  echo "renderer accepted an invalid admin preview signing key" >&2
  exit 1
fi

echo "render production env tests passed"
