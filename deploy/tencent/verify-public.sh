#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo 'usage: verify-public.sh <health-domain> <journal-domain> <admin-domain>' >&2
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

failed=0
for domain in "$@"; do
  if [[ ! "$domain" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*[a-zA-Z0-9]$ ]]; then
    echo 'Each public domain must be a DNS hostname.' >&2
    exit 64
  fi
  ready=0
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if check_readiness "https://$domain/readyz" "$domain" ready "$response_file"; then
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
done
exit "$failed"
