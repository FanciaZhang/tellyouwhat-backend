#!/usr/bin/env bash
# Certbot deploy hook. Runs as root; private keys stay on the server.
set -euo pipefail

lineage=${RENEWED_LINEAGE:?Certbot must supply RENEWED_LINEAGE}
case "$lineage" in
  /etc/letsencrypt/live/journal-ip) ;;
  *) printf 'Unexpected certificate lineage\n' >&2; exit 1 ;;
esac

exec 9>/run/lock/tellyouwhat-journal-ip-certificate.lock
flock 9
certificate_dir=/etc/caddy/journal-ip
install -d -o root -g caddy -m 750 "$certificate_dir"
openssl x509 -in "$lineage/fullchain.pem" -checkend 86400 -noout

# Verify the key pair before replacing either file. No private material is logged.
certificate_public=$(openssl x509 -in "$lineage/fullchain.pem" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum)
key_public=$(openssl pkey -in "$lineage/privkey.pem" -pubout -outform DER | sha256sum)
test "$certificate_public" = "$key_public"

backup_dir=$(mktemp -d "$certificate_dir/.previous.XXXXXX")
trap 'rm -rf "$backup_dir"' EXIT
for name in fullchain.pem privkey.pem; do
  if test -f "$certificate_dir/$name"; then
    cp -p "$certificate_dir/$name" "$backup_dir/$name"
  fi
  install -o root -g caddy -m 640 "$lineage/$name" "$certificate_dir/$name.next"
done
mv "$certificate_dir/fullchain.pem.next" "$certificate_dir/fullchain.pem"
mv "$certificate_dir/privkey.pem.next" "$certificate_dir/privkey.pem"

if caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile && systemctl reload caddy; then
  printf 'Journal IP certificate deployed and Caddy reloaded\n'
else
  for name in fullchain.pem privkey.pem; do
    if test -f "$backup_dir/$name"; then
      cp -p "$backup_dir/$name" "$certificate_dir/$name"
    fi
  done
  printf 'Caddy reload failed; restored the previous certificate files\n' >&2
  exit 1
fi
