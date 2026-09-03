#!/usr/bin/env bash

check_readiness() {
  local url="$1"
  local host="$2"
  local expected_status="$3"
  local response_file="$4"
  local status_code

  if ! status_code=$(curl --fail --silent --show-error \
    --connect-timeout 2 --max-time 5 \
    --header "Host: $host" --output "$response_file" \
    --write-out '%{http_code}' "$url"); then
    return 1
  fi
  [[ "$status_code" == 200 ]] &&
    jq -e --arg expected "$expected_status" \
      'type == "object" and .status == $expected' "$response_file" >/dev/null
}
