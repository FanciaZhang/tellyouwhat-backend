#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
mkdir -p "$test_dir/bin"

cat > "$test_dir/bin/curl" <<'SCRIPT'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PUBLIC_TEST_LOG"
response_file=''
while (($#)); do
  case "$1" in
    -k|--insecure|--resolve|--connect-to|-L|--location)
      echo 'Public verification changed TLS trust, DNS resolution, or redirect handling.' >&2
      exit 91 ;;
    --output) response_file="$2"; shift ;;
  esac
  shift
done
case "$PUBLIC_TEST_RESULT" in
  certificate-error) exit 60 ;;
  redirect) printf '302'; exit ;;
  unavailable) exit 22 ;;
  wrong-body) printf '{"status":"ok"}' > "$response_file" ;;
  ready) printf '{"status":"ready"}' > "$response_file" ;;
esac
printf '200'
SCRIPT
chmod +x "$test_dir/bin/curl"

run_verification() {
  : > "$test_dir/requests"
  PATH="$test_dir/bin:$PATH" READINESS_ATTEMPTS=1 \
    PUBLIC_TEST_RESULT="$1" PUBLIC_TEST_LOG="$test_dir/requests" \
    bash "$script_dir/verify-public.sh" api.health.example api.journal.example admin.example
}

run_verification ready
[[ $(wc -l < "$test_dir/requests") -eq 3 ]]
for failure in certificate-error redirect unavailable wrong-body; do
  if run_verification "$failure" > "$test_dir/output" 2>&1; then
    printf 'Public verification accepted failure: %s\n' "$failure" >&2
    exit 1
  fi
  [[ $(wc -l < "$test_dir/requests") -eq 3 ]]
done
echo 'Public HTTPS trust and response validation tests passed.'
