#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: render-production-env.sh TEMPLATE OUTPUT" >&2
  exit 2
fi

template_path=$1
output_path=$2
required_secrets=(
  MYSQL_PASSWORD
  REDIS_PASSWORD
  PAYLOAD_ENCRYPTION_KEY
  WORKER_INTERNAL_SECRET
  JOB_CAPABILITY_SECRET
  APP_STORE_ISSUER_ID
  APP_STORE_KEY_ID
  APP_STORE_APP_APPLE_ID
  APP_STORE_CONNECT_ISSUER_ID
  APP_STORE_CONNECT_KEY_ID
  APP_STORE_CONNECT_SUBSCRIPTION_ID
  ADMIN_PREVIEW_SIGNING_KEY
  ARK_API_KEY
  TOS_ACCESS_KEY
  TOS_SECRET_KEY
)

for secret_name in "${required_secrets[@]}"; do
  secret_value=${!secret_name-}
  while [[ $secret_value == *$'\n' || $secret_value == *$'\r' ]]; do
    if [[ $secret_value == *$'\n' ]]; then
      secret_value=${secret_value%$'\n'}
    else
      secret_value=${secret_value%$'\r'}
    fi
  done
  if [[ -z $secret_value ]]; then
    echo "missing GitHub Environment secret: $secret_name" >&2
    exit 1
  fi
  if [[ $secret_value == *$'\n'* || $secret_value == *$'\r'* ]]; then
    echo "GitHub Environment secret contains a newline: $secret_name" >&2
    exit 1
  fi
  printf -v "$secret_name" '%s' "$secret_value"
done

if [[ ! $MYSQL_PASSWORD =~ ^[A-Za-z0-9_-]+$ ]]; then
  echo "MYSQL_PASSWORD contains characters that are unsafe in DATABASE_DSN" >&2
  exit 1
fi
if [[ ! $REDIS_PASSWORD =~ ^[A-Za-z0-9_-]+$ ]]; then
  echo "REDIS_PASSWORD contains characters that are unsafe in REDIS_URL" >&2
  exit 1
fi
if [[ ! $APP_STORE_APP_APPLE_ID =~ ^[0-9]+$ ]]; then
  echo "APP_STORE_APP_APPLE_ID must be numeric" >&2
  exit 1
fi
if (( ${#JOB_CAPABILITY_SECRET} < 32 )); then
  echo "JOB_CAPABILITY_SECRET must be at least 32 bytes" >&2
  exit 1
fi
if [[ ! $ADMIN_PREVIEW_SIGNING_KEY =~ ^[A-Za-z0-9+/]+$ ]]; then
  echo "ADMIN_PREVIEW_SIGNING_KEY must be unpadded base64" >&2
  exit 1
fi
admin_preview_padding=
case $(( ${#ADMIN_PREVIEW_SIGNING_KEY} % 4 )) in
  0) ;;
  2) admin_preview_padding='==' ;;
  3) admin_preview_padding='=' ;;
  *)
    echo "ADMIN_PREVIEW_SIGNING_KEY must be unpadded base64" >&2
    exit 1
    ;;
esac
admin_preview_key_bytes=$(printf '%s%s' \
  "$ADMIN_PREVIEW_SIGNING_KEY" "$admin_preview_padding" \
  | openssl base64 -d -A \
  | wc -c \
  | tr -d ' ')
if (( admin_preview_key_bytes < 32 )); then
  echo "ADMIN_PREVIEW_SIGNING_KEY must decode to at least 32 bytes" >&2
  exit 1
fi
payload_key_bytes=$(printf '%s' "$PAYLOAD_ENCRYPTION_KEY" \
  | openssl base64 -d -A \
  | wc -c \
  | tr -d ' ')
if [[ $payload_key_bytes != 32 ]]; then
  echo "PAYLOAD_ENCRYPTION_KEY must decode to exactly 32 bytes" >&2
  exit 1
fi

umask 077
while IFS= read -r line || [[ -n $line ]]; do
  key=${line%%=*}
  case $key in
    MYSQL_PASSWORD|REDIS_PASSWORD|PAYLOAD_ENCRYPTION_KEY|WORKER_INTERNAL_SECRET|JOB_CAPABILITY_SECRET|APP_STORE_ISSUER_ID|APP_STORE_KEY_ID|APP_STORE_APP_APPLE_ID|APP_STORE_CONNECT_ISSUER_ID|APP_STORE_CONNECT_KEY_ID|APP_STORE_CONNECT_SUBSCRIPTION_ID|ADMIN_PREVIEW_SIGNING_KEY|ARK_API_KEY|TOS_ACCESS_KEY|TOS_SECRET_KEY)
      printf '%s=%s\n' "$key" "${!key}"
      ;;
    *)
      printf '%s\n' "$line"
      ;;
  esac
done < "$template_path" > "$output_path"

if grep -q '__GITHUB_ENVIRONMENT_SECRET__' "$output_path"; then
  echo "production environment contains unresolved secret placeholders" >&2
  exit 1
fi

chmod 600 "$output_path"
