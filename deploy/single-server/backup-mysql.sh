#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
backend_dir=$(CDPATH= cd -- "$script_dir/../.." && pwd)
environment_file=${TELLYOUWHAT_ENV_FILE:-$backend_dir/.env.production}
backup_dir=${TELLYOUWHAT_BACKUP_DIR:-/var/backups/tellyouwhat}
timestamp=$(date -u +%Y%m%dT%H%M%SZ)
temporary="$backup_dir/mysql-$timestamp.sql.gz.partial"
destination="$backup_dir/mysql-$timestamp.sql.gz"

umask 077
mkdir -p "$backup_dir"
set -a
. "$environment_file"
set +a
docker run --rm --network host -e MYSQL_PWD="$MYSQL_PASSWORD" mysql:8.4 \
    mysqldump --host="$MYSQL_HOST" --port="${MYSQL_PORT:-3306}" --user="$MYSQL_USER" \
    --single-transaction --quick --hex-blob --no-tablespaces "$MYSQL_DATABASE" \
    | gzip -9 > "$temporary"
mv "$temporary" "$destination"
sha256sum "$destination" > "$destination.sha256"
find "$backup_dir" -type f \( -name 'mysql-*.sql.gz' -o -name 'mysql-*.sql.gz.sha256' \) -mtime +14 -delete
printf '%s\n' "$destination"
