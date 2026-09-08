"""Encrypted MySQL snapshots and disposable restore verification."""

import gzip
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import time

from ops_common import OperationError, atomic_json


# App data is local-first. Disaster recovery retains only the backend control
# plane, opaque deletion-completion receipts, and the identity-free project
# cost ledger. Every current or future table outside this allowlist is restored
# with its schema and no rows, so an older backup cannot recreate an App Attest
# identity, consent, entitlement, quota, media record, AI request, or
# purchase-derived user state.
RECOVERY_DATA_TABLES = (
    "schema_migrations",
    "apps",
    "privacy_deletion_receipts",
    "ai_cost_control_state",
    "ai_cost_months",
    "ai_cost_attempts",
    "admin_control_state",
    "admin_users",
    "admin_user_apps",
    "admin_webauthn_credentials",
    "admin_bootstrap_tokens",
    "admin_invitations",
    "admin_invitation_apps",
    "admin_audit_events",
    "admin_operations",
    "operations_collection",
    "prompt_config_revisions",
    "prompt_config_current",
    "prompt_config_mutations",
    "platform_ops_revisions",
    "platform_ops_current",
    "platform_ops_mutations",
    "health_ai_config_revisions",
    "health_ai_config_current",
    "health_ai_rollout_commands",
    "health_ai_endpoint_prices",
    "health_ai_model_attempts",
)
BACKUP_RETENTION_SECONDS = 14 * 86400


def backup_secret(runtime):
    value = runtime.config.get("BACKUP_ENCRYPTION_KEY", "")
    if len(value) < 43:
        raise OperationError("BACKUP_ENCRYPTION_KEY must contain at least 256 bits of generated key material")
    return value


def manifest_signature(secret, manifest):
    payload = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
    key = hmac.new(secret.encode(), b"tellyouwhat-backup-manifest-v1", hashlib.sha256).digest()
    return hmac.new(key, payload, hashlib.sha256).hexdigest()


def file_digest(path):
    digest = hashlib.sha256()
    with open(path, "rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def crypt(runtime, source, destination, *, decrypt=False):
    command = ["openssl", "enc", "-aes-256-cbc", "-pbkdf2", "-iter", "200000", "-md", "sha256",
               "-pass", "env:TELLYOUWHAT_BACKUP_PASSPHRASE", "-in", str(source), "-out", str(destination)]
    if decrypt:
        command.append("-d")
    runtime.execute("backup-crypto", command,
                    env={"TELLYOUWHAT_BACKUP_PASSPHRASE": backup_secret(runtime)}, timeout=900)
    os.chmod(destination, 0o600)


def create_backup(runtime):
    backup_secret(runtime)
    database = runtime.config.get("MYSQL_DATABASE", "")
    if not re.fullmatch(r"[A-Za-z0-9_]+", database) or database in ("mysql", "sys", "information_schema", "performance_schema"):
        raise OperationError("a dedicated application database is required")
    runtime.backups.mkdir(parents=True, exist_ok=True, mode=0o700)
    prune_backups(runtime)
    timestamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    destination = runtime.backups / ("mysql-" + timestamp + "-" + secrets.token_hex(3) + ".sql.gz.enc")
    image = runtime.config.get("MYSQL_BACKUP_IMAGE", "mysql:8.4")
    counts = {}
    connection = ["--host=" + runtime.config["MYSQL_HOST"], "--port=" + runtime.config.get("MYSQL_PORT", "3306"),
                  "--user=" + runtime.config["MYSQL_USER"]]
    child_env = {"MYSQL_PWD": runtime.config["MYSQL_PASSWORD"]}
    table_output = runtime.execute("backup-table-inventory", [
        "docker", "run", "--rm", "--network", "host", "--env", "MYSQL_PWD", image, "mysql", *connection,
        "--database=" + database, "--batch", "--skip-column-names",
        "--execute=SELECT TABLE_NAME FROM information_schema.tables WHERE table_schema = DATABASE() AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME",
    ], env=child_env, timeout=300)
    database_tables = []
    for value in table_output.decode().splitlines():
        table = value.strip()
        if not re.fullmatch(r"[A-Za-z0-9_]+", table):
            raise OperationError("database returned an invalid table name; no backup published")
        database_tables.append(table)
    if len(database_tables) != len(set(database_tables)):
        raise OperationError("database returned duplicate table names; no backup published")
    if not database_tables or "schema_migrations" not in database_tables or "apps" not in database_tables:
        raise OperationError("database does not contain the required application schema; no backup published")
    included_data_tables = [table for table in RECOVERY_DATA_TABLES if table in database_tables]
    excluded_data_tables = sorted(set(database_tables) - set(included_data_tables))
    command = ["docker", "run", "--rm", "--network", "host", "--env", "MYSQL_PWD", image, "mysqldump",
               *connection, "--single-transaction", "--quick", "--hex-blob",
               "--no-tablespaces", "--set-gtid-purged=OFF", "--column-statistics=0", "--skip-extended-insert",
               "--skip-comments"]
    commands = [
        command + ["--no-data", database],
        command + ["--no-create-info", database, *included_data_tables],
    ]
    with tempfile.TemporaryDirectory(prefix=".backup-", dir=runtime.backups) as directory:
        temporary = Path(directory)
        compressed = temporary / "snapshot.sql.gz"
        errors = temporary / "dump.stderr"
        process_env = os.environ.copy()
        process_env.update(child_env)
        with errors.open("wb") as diagnostic, compressed.open("wb") as output:
            os.chmod(errors, 0o600)
            os.chmod(compressed, 0o600)
            with gzip.GzipFile(fileobj=output, mode="wb", mtime=0) as archive:
                for dump_command in commands:
                    process = subprocess.Popen(dump_command, stdout=subprocess.PIPE, stderr=diagnostic, env=process_env)
                    try:
                        for line in process.stdout:
                            create = re.match(rb"CREATE TABLE `([A-Za-z0-9_]+)`", line)
                            insert = re.match(rb"INSERT INTO `([A-Za-z0-9_]+)`", line)
                            if create:
                                counts[create[1].decode()] = 0
                            if insert:
                                table = insert[1].decode()
                                if table not in included_data_tables:
                                    raise OperationError("database export included excluded application data; no backup published")
                                counts[table] = counts.get(table, 0) + 1
                            archive.write(line)
                        returncode = process.wait(timeout=900)
                    finally:
                        process.stdout.close()
                        if process.poll() is None:
                            process.kill()
                            process.wait()
                    if returncode != 0:
                        raise OperationError(f"database export failed (exit {returncode}); no backup published")
        if not counts or "schema_migrations" not in counts:
            raise OperationError("database export contains no valid application schema")
        if set(counts) != set(database_tables):
            raise OperationError("database export table inventory is incomplete; no backup published")
        if any(counts.get(table) != 0 for table in excluded_data_tables):
            raise OperationError("database export included excluded application data; no backup published")
        encrypted = temporary / "snapshot.enc"
        crypt(runtime, compressed, encrypted)
        manifest = {"version": 1, "created_at": int(time.time()), "database": database,
                    "filename": destination.name, "image": image, "table_rows": counts,
                    "included_data_tables": included_data_tables,
                    "excluded_data_tables": excluded_data_tables,
                    "sha256": file_digest(encrypted)}
        signature = manifest_signature(backup_secret(runtime), manifest)
        os.replace(encrypted, destination)
        atomic_json(str(destination) + ".json", {"manifest": manifest, "hmac_sha256": signature})
    return runtime.record("backup", filename=destination.name, sha256=manifest["sha256"], tables=len(counts))


def prune_backups(runtime):
    cutoff = time.time() - BACKUP_RETENTION_SECONDS
    removed = 0
    failed = 0
    for path in runtime.backups.glob("mysql-*.sql.gz.enc"):
        try:
            manifest = verify_backup(runtime, path)
            if manifest["created_at"] <= cutoff:
                path.unlink()
                Path(str(path) + ".json").unlink()
                removed += 1
        except (OperationError, OSError):
            failed += 1
    if failed:
        raise OperationError(f"backup retention verification failed for {failed} snapshots; operator review required")
    return removed


def verify_backup(runtime, path):
    path = Path(path)
    try:
        if path.is_symlink() or Path(str(path) + ".json").is_symlink():
            raise OperationError("backup symlinks are not permitted")
        envelope = json.loads(Path(str(path) + ".json").read_text())
        manifest = envelope["manifest"]
        expected = manifest_signature(backup_secret(runtime), manifest)
        if not hmac.compare_digest(expected, envelope["hmac_sha256"]):
            raise OperationError("backup manifest authentication failed")
        if manifest["filename"] != path.name or manifest["sha256"] != file_digest(path):
            raise OperationError("backup content verification failed")
        if manifest["database"] != runtime.config["MYSQL_DATABASE"]:
            raise OperationError("backup belongs to another application database")
        if type(manifest["created_at"]) is not int or manifest["created_at"] <= 0:
            raise OperationError("invalid backup creation time")
        if not manifest["table_rows"] or not all(re.fullmatch(r"[A-Za-z0-9_]+", table) for table in manifest["table_rows"]):
            raise OperationError("invalid backup table manifest")
        included = manifest.get("included_data_tables")
        excluded = manifest.get("excluded_data_tables")
        if included is not None:
            if (not isinstance(included, list) or not isinstance(excluded, list)
                    or any(not isinstance(table, str) or not re.fullmatch(r"[A-Za-z0-9_]+", table)
                           for table in included + excluded)
                    or len(included) != len(set(included)) or len(excluded) != len(set(excluded))
                    or set(included) & set(excluded)
                    or set(included) | set(excluded) != set(manifest["table_rows"])
                    or any(manifest["table_rows"].get(table) != 0 for table in excluded)):
                raise OperationError("invalid backup data retention manifest")
        return manifest
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise OperationError("backup manifest is missing or invalid") from error


def restore_drill(runtime, filename=None):
    if filename:
        if Path(filename).name != filename:
            raise OperationError("restore drill accepts only a backup filename")
        source = runtime.backups / filename
    else:
        try:
            source = runtime.backups / json.loads((runtime.state / "backup.json").read_text())["filename"]
        except (OSError, KeyError, ValueError) as error:
            raise OperationError("a successful backup is required before a restore drill") from error
    manifest = verify_backup(runtime, source)
    if manifest["created_at"] <= time.time() - BACKUP_RETENTION_SECONDS:
        raise OperationError("backup is outside the retention period")
    container = "tellyouwhat-restore-" + secrets.token_hex(8)
    password = secrets.token_urlsafe(32)
    database = "tellyouwhat_restore_test"
    with tempfile.TemporaryDirectory(prefix="restore-", dir=runtime.state) as directory:
        compressed = Path(directory) / "snapshot.sql.gz"
        crypt(runtime, source, compressed, decrypt=True)
        try:
            runtime.execute("restore-start", ["docker", "run", "--detach", "--rm", "--name", container,
                            "--label", "cn.tellyouwhat.operation=restore-drill", "--network", "none",
                            "--memory", "768m", "--pids-limit", "256",
                            "--tmpfs", "/var/lib/mysql:rw,noexec,nosuid,size=768m",
                            "--env", "MYSQL_ROOT_PASSWORD", "--env", "MYSQL_DATABASE", manifest["image"],
                            "--innodb-buffer-pool-size=128M", "--performance-schema=OFF"],
                            env={"MYSQL_ROOT_PASSWORD": password, "MYSQL_DATABASE": database}, timeout=180)
            sql = ["docker", "exec", "--env", "MYSQL_PWD", "--interactive", container,
                   "mysql", "--user=root", "--batch", "--skip-column-names", "--database=" + database]
            child_env = os.environ.copy()
            child_env["MYSQL_PWD"] = password
            for attempt in range(90):
                ready = subprocess.run(sql + ["--execute=SELECT 1"], capture_output=True, env=child_env, timeout=10)
                if ready.returncode == 0:
                    break
                time.sleep(2)
            else:
                raise OperationError("isolated restore database did not become ready")
            with (Path(directory) / "restore.stderr").open("wb") as diagnostic:
                process = subprocess.Popen(sql, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                                           stderr=diagnostic, env=child_env)
                try:
                    with gzip.open(compressed, "rb") as source_stream:
                        for block in iter(lambda: source_stream.read(1024 * 1024), b""):
                            process.stdin.write(block)
                    process.stdin.close()
                    returncode = process.wait(timeout=900)
                finally:
                    if process.poll() is None:
                        process.kill()
                        process.wait()
            if returncode:
                raise OperationError(f"isolated database restore failed (exit {returncode})")
            table_names = runtime.execute("restore-tables", sql + [
                "--execute=SELECT TABLE_NAME FROM information_schema.tables WHERE TABLE_SCHEMA=DATABASE() AND TABLE_TYPE='BASE TABLE'"],
                env={"MYSQL_PWD": password}).decode().splitlines()
            if set(table_names) != set(manifest["table_rows"]):
                raise OperationError("restored database tables differ from the snapshot")
            for table, count in manifest["table_rows"].items():
                actual = runtime.execute("restore-count", sql + ["--execute=SELECT COUNT(*) FROM `" + table + "`"],
                                         env={"MYSQL_PWD": password}).decode().strip()
                if int(actual) != count:
                    raise OperationError("restored row counts differ from the snapshot")
        finally:
            runtime.execute("restore-cleanup", ["docker", "rm", "--force", container], timeout=60)
    return runtime.record("restore", filename=source.name, sha256=manifest["sha256"], tables=len(manifest["table_rows"]))
