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


TRANSIENT_DATA_TABLES = ("ai_jobs", "job_dispatch_outbox")


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
    timestamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    destination = runtime.backups / ("mysql-" + timestamp + "-" + secrets.token_hex(3) + ".sql.gz.enc")
    image = runtime.config.get("MYSQL_BACKUP_IMAGE", "mysql:8.4")
    counts = {}
    command = ["docker", "run", "--rm", "--network", "host", "--env", "MYSQL_PWD", image, "mysqldump",
               "--host=" + runtime.config["MYSQL_HOST"], "--port=" + runtime.config.get("MYSQL_PORT", "3306"),
               "--user=" + runtime.config["MYSQL_USER"], "--single-transaction", "--quick", "--hex-blob",
               "--no-tablespaces", "--set-gtid-purged=OFF", "--column-statistics=0", "--skip-extended-insert",
               "--skip-comments"]
    commands = [
        command + ["--ignore-table=" + database + "." + table for table in TRANSIENT_DATA_TABLES] + [database],
        command + ["--no-data", database, *TRANSIENT_DATA_TABLES],
    ]
    with tempfile.TemporaryDirectory(prefix=".backup-", dir=runtime.backups) as directory:
        temporary = Path(directory)
        compressed = temporary / "snapshot.sql.gz"
        errors = temporary / "dump.stderr"
        child_env = os.environ.copy()
        child_env["MYSQL_PWD"] = runtime.config["MYSQL_PASSWORD"]
        with errors.open("wb") as diagnostic, compressed.open("wb") as output:
            os.chmod(errors, 0o600)
            os.chmod(compressed, 0o600)
            with gzip.GzipFile(fileobj=output, mode="wb", mtime=0) as archive:
                for dump_command in commands:
                    process = subprocess.Popen(dump_command, stdout=subprocess.PIPE, stderr=diagnostic, env=child_env)
                    try:
                        for line in process.stdout:
                            create = re.match(rb"CREATE TABLE `([A-Za-z0-9_]+)`", line)
                            insert = re.match(rb"INSERT INTO `([A-Za-z0-9_]+)`", line)
                            if create:
                                counts[create[1].decode()] = 0
                            if insert:
                                table = insert[1].decode()
                                if table in TRANSIENT_DATA_TABLES:
                                    raise OperationError("database export included excluded transient data; no backup published")
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
        if any(counts.get(table) != 0 for table in TRANSIENT_DATA_TABLES):
            raise OperationError("database export is missing transient table definitions")
        encrypted = temporary / "snapshot.enc"
        crypt(runtime, compressed, encrypted)
        manifest = {"version": 1, "created_at": int(time.time()), "database": database,
                    "filename": destination.name, "image": image, "table_rows": counts,
                    "excluded_data_tables": list(TRANSIENT_DATA_TABLES),
                    "sha256": file_digest(encrypted)}
        signature = manifest_signature(backup_secret(runtime), manifest)
        os.replace(encrypted, destination)
        atomic_json(str(destination) + ".json", {"manifest": manifest, "hmac_sha256": signature})
    result = runtime.record("backup", filename=destination.name, sha256=manifest["sha256"], tables=len(counts))
    cutoff = time.time() - 14 * 86400
    for path in runtime.backups.glob("mysql-*.sql.gz.enc"):
        if path != destination and path.stat().st_mtime < cutoff:
            verify_backup(runtime, path)
            Path(str(path) + ".json").unlink()
            path.unlink()
    return result


def verify_backup(runtime, path):
    path = Path(path)
    try:
        envelope = json.loads(Path(str(path) + ".json").read_text())
        manifest = envelope["manifest"]
        expected = manifest_signature(backup_secret(runtime), manifest)
        if not hmac.compare_digest(expected, envelope["hmac_sha256"]):
            raise OperationError("backup manifest authentication failed")
        if manifest["filename"] != path.name or manifest["sha256"] != file_digest(path):
            raise OperationError("backup content verification failed")
        if manifest["database"] != runtime.config["MYSQL_DATABASE"]:
            raise OperationError("backup belongs to another application database")
        if not manifest["table_rows"] or not all(re.fullmatch(r"[A-Za-z0-9_]+", table) for table in manifest["table_rows"]):
            raise OperationError("invalid backup table manifest")
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
