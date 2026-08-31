#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
backend_dir=$(CDPATH= cd -- "$script_dir/../.." && pwd)
compose_file=${HEALTH_COMPOSE_FILE:-$backend_dir/compose.production.yaml}
environment_file=${HEALTH_ENV_FILE:-$backend_dir/.env.production}
backup_dir=${HEALTH_BACKUP_DIR:-/var/backups/health-ai}
timestamp=$(date -u +%Y%m%dT%H%M%SZ)
temporary="$backup_dir/mysql-$timestamp.sql.gz.partial"
destination="$backup_dir/mysql-$timestamp.sql.gz"

umask 077
mkdir -p "$backup_dir"
docker compose --env-file "$environment_file" -f "$compose_file" exec -T mysql \
    sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" exec mysqldump --user="$MYSQL_USER" --single-transaction --quick --hex-blob --no-tablespaces "$MYSQL_DATABASE"' \
    | gzip -9 > "$temporary"
mv "$temporary" "$destination"
sha256sum "$destination" > "$destination.sha256"
find "$backup_dir" -type f \( -name 'mysql-*.sql.gz' -o -name 'mysql-*.sql.gz.sha256' \) -mtime +14 -delete
printf '%s\n' "$destination"
