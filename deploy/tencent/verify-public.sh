#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo 'usage: verify-public.sh <public-targets-json>' >&2
  exit 64
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$script_dir/readiness.sh"
response_file=$(mktemp)
trap 'rm -f "$response_file"' EXIT

attempts="${READINESS_ATTEMPTS:-12}"
if [[ ! "$attempts" =~ ^[1-9][0-9]*$ ]]; then
  echo 'READINESS_ATTEMPTS must be a positive integer.' >&2
  exit 64
fi

if ! jq -e 'type == "array" and length > 0 and
  all(.[]; type == "object" and keys == ["domain","field","value"] and
      (.domain | type == "string" and test("^[a-zA-Z0-9][a-zA-Z0-9.-]*[a-zA-Z0-9]\\z")) and
      ((.field == "status" and .value == "ready") or (.field == "code" and .value == "ok"))) and
  ([.[].domain] | length == (unique | length))' <<< "$1" >/dev/null; then
  echo 'Public verification requires the selected release targets.' >&2
  exit 64
fi

failed=0
while IFS=$'\t' read -r domain field expected_status; do
  ready=0
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if check_readiness "https://$domain/readyz" "$domain" "$expected_status" "$response_file" "$field"; then
      ready=1
      break
    fi
    if ((attempt < attempts)); then sleep 5; fi
  done
  if [[ "$ready" == 1 ]]; then
    printf 'Public HTTPS verified: %s\n' "$domain"
  else
    printf 'Public HTTPS verification failed: %s\n' "$domain" >&2
    failed=1
  fi
done < <(jq -r '.[] | [.domain, .field, .value] | @tsv' <<< "$1")
exit "$failed"
