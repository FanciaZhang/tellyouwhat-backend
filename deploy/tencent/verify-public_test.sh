#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
mkdir -p "$test_dir/bin"

cat > "$test_dir/bin/curl" <<'CURL'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PUBLIC_TEST_LOG"
response_file=''
url=''
while (($#)); do
  case "$1" in
    -k|--insecure|--resolve|--connect-to|-L|--location)
      echo 'Public verification changed TLS trust, DNS resolution, or redirect handling.' >&2
      exit 91 ;;
    --output) response_file="$2"; shift ;;
    https://*) url="$1" ;;
  esac
  shift
done
case "$PUBLIC_TEST_RESULT" in
  certificate-error) exit 60 ;;
  redirect) printf '302'; exit ;;
  unavailable) exit 22 ;;
  wrong-body) printf '{"status":"ok"}' > "$response_file"; printf '200'; exit ;;
  birthday-unavailable) [[ "$url" != https://api.birthday.example/* ]] || exit 22 ;;
  albums-unavailable) [[ "$url" != https://api.albums.example/* ]] || exit 22 ;;
esac
if [[ "$url" == https://api.albums.example/* ]]; then
  printf '{"code":"ok"}' > "$response_file"
else
  printf '{"status":"ready"}' > "$response_file"
fi
printf '200'
CURL
chmod +x "$test_dir/bin/curl"

targets='[{"domain":"api.health.example","field":"status","value":"ready"},
{"domain":"api.journal.example","field":"status","value":"ready"},
{"domain":"admin.example","field":"status","value":"ready"}]'
all_targets=$(jq -c '. + [{"domain":"api.birthday.example","field":"status","value":"ready"},
{"domain":"api.albums.example","field":"code","value":"ok"}]' <<< "$targets")

run_verification() {
  : > "$test_dir/requests"
  PATH="$test_dir/bin:$PATH" READINESS_ATTEMPTS=1 \
    PUBLIC_TEST_RESULT="$1" PUBLIC_TEST_LOG="$test_dir/requests" \
    bash "$script_dir/verify-public.sh" "$2"
}

run_verification ready "$targets"
[[ $(wc -l < "$test_dir/requests") -eq 3 ]]
run_verification ready "$all_targets"
[[ $(wc -l < "$test_dir/requests") -eq 5 ]]
for failure in certificate-error redirect unavailable wrong-body birthday-unavailable albums-unavailable; do
  if run_verification "$failure" "$all_targets" > "$test_dir/output" 2>&1; then
    printf 'Public verification accepted failure: %s\n' "$failure" >&2
    exit 1
  fi
  [[ $(wc -l < "$test_dir/requests") -eq 5 ]]
done
for invalid in '' '[]' '{}' 'null' '[1]' '[{}]' \
  "$(jq -c '. + [.[0]]' <<< "$targets")" \
  "$(jq -c '.[0].field = "code"' <<< "$targets")" \
  "$(jq -c '.[0].domain = 42' <<< "$targets")" \
  "$(jq -c '.[1].domain = "journal.example\nInjected: value"' <<< "$targets")" \
  "$(jq -c '.[1].domain = "journal.example\n"' <<< "$targets")" \
  "$(jq -c '.[1].domain = "https://journal.example/path"' <<< "$targets")"; do
  if run_verification ready "$invalid" > "$test_dir/output" 2>&1; then
    echo 'Public verification accepted invalid targets.' >&2
    exit 1
  fi
  [[ ! -s "$test_dir/requests" ]]
done
echo 'Public HTTPS target, trust and response validation tests passed.'
