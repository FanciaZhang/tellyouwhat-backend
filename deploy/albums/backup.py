#!/usr/bin/env python3
"""Encrypted Albums snapshots and quarantined, disposable MySQL restore drills."""
import argparse
import gzip
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import tempfile
import tarfile
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tencent"))
from ops_common import OperationError, Runtime as OperationsRuntime, atomic_json, read_environment
from ops_backup import backup_secret, crypt, file_digest, manifest_signature

class Runtime(OperationsRuntime):
    # Share the operations lock, not the active Health deployment's credentials.
    def refresh(self):
        self.environment_file = self.base_environment_file
        self.compose_root = self.root
        self.config = read_environment(self.environment_file)


SCOPE = "albums-quarantined-v1"
RETENTION = 14 * 86400
TABLES = tuple("""album_account_deletions album_account_object_purges album_accounts
album_apple_grants album_apple_identity_state album_asset_lifecycle album_asset_operations
album_asset_versions album_cloud_subscriptions album_deletion_apple_jobs
album_deletion_subscription_jobs album_deletion_ledger album_deletion_video_jobs album_derived_cleanups
album_derived_finalizations album_library_changes album_live_pair_multiparts album_live_pairs
album_login_challenges album_multipart_completions album_multipart_intents album_object_purges
album_original_seals album_photo_jobs album_photo_writes album_push_registrations
album_retention_deliveries album_retention_reminders album_recovery_state album_sessions album_share_claims
album_share_links album_storage_accounts album_subscription_notifications album_uploads
album_video_jobs""".split())
TABLES = tuple(sorted(TABLES))
QUARANTINE = """
START TRANSACTION;
UPDATE album_accounts SET state='disabled' WHERE state='active';
UPDATE album_sessions SET revoked_at=COALESCE(revoked_at,UTC_TIMESTAMP(6));
UPDATE album_share_links SET revoked_at=COALESCE(revoked_at,UTC_TIMESTAMP(6));
UPDATE album_push_registrations SET state='disabled',token_sha256=NULL,encrypted_token=NULL,
 token_nonce=NULL,registered_at=NULL,request_id=NULL,request_sha256=NULL;
COMMIT;
"""


def configure(runtime):
    # Separate key and destination from the shared control-plane backup policy.
    runtime.config["BACKUP_ENCRYPTION_KEY"] = runtime.config.get("ALBUM_BACKUP_ENCRYPTION_KEY", "")
    if backup_secret(runtime).startswith("replace-"):
        raise OperationError("replace the Albums backup key placeholder before use")
    database = runtime.config.get("MYSQL_DATABASE", "")
    if not re.fullmatch(r"[A-Za-z0-9_]+", database) or database in ("mysql", "sys", "information_schema", "performance_schema"):
        raise OperationError("a dedicated application database is required")
    if runtime.backups.is_symlink():
        raise OperationError("backup directory must not be a symlink")
    runtime.backups.mkdir(mode=0o700, parents=True, exist_ok=True)
    if runtime.backups.stat().st_mode & 0o077:
        raise OperationError("backup directory must be private (0700)")
    return database


def connection(runtime):
    if runtime.config.get("MYSQL_SOCKET"):
        args = ["--protocol=socket", "--socket=" + runtime.config["MYSQL_SOCKET"]]
    else:
        args = ["--host=" + runtime.config["MYSQL_HOST"], "--port=" + runtime.config.get("MYSQL_PORT", "3306")]
    return args + ["--user=" + runtime.config["MYSQL_USER"]]


def verify(runtime, path):
    database = configure(runtime)
    path = Path(path)
    if path.parent.resolve() != runtime.backups.resolve() or not re.fullmatch(r"albums-[0-9a-f]{32}\.sql\.gz\.enc", path.name):
        raise OperationError("invalid Albums backup path")
    manifest_path = Path(str(path) + ".json")
    try:
        if path.is_symlink() or manifest_path.is_symlink() or manifest_path.stat().st_size > 65536:
            raise OperationError("invalid Albums backup files")
        envelope = json.loads(manifest_path.read_text())
        m = envelope["manifest"]
        if not hmac.compare_digest(manifest_signature(backup_secret(runtime), m), envelope["hmac_sha256"]):
            raise OperationError("Albums backup authentication failed")
        if (m["scope"] != SCOPE or m["database"] != database or m["filename"] != path.name
                or m["sha256"] != file_digest(path) or set(m["table_rows"]) != set(TABLES) or set(m["table_sha256"]) != set(TABLES)
                or any(not re.fullmatch(r"[0-9a-f]{64}", value) for value in m["table_sha256"].values())
                or any(type(n) is not int or n < 0 for n in m["table_rows"].values())
                or type(m["created_at"]) is not int or m["created_at"] <= 0 or m["created_at"] > time.time() + 300):
            raise OperationError("Albums backup metadata or content mismatch")
        return m
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise OperationError("Albums backup is missing or invalid") from error


def prune(runtime):
    configure(runtime)
    removed = 0
    for path in sorted(runtime.backups.glob("albums-*.sql.gz.enc")):
        m = verify(runtime, path)
        if m["created_at"] <= time.time() - RETENTION:
            path.unlink()
            Path(str(path) + ".json").unlink()
            removed += 1
    return removed


def create(runtime, binaries):
    database = configure(runtime)
    prune(runtime)
    credentials = {"MYSQL_PWD": runtime.config.get("MYSQL_PASSWORD", "")}
    sql = [str(binaries / "mysql"), "--no-defaults", *connection(runtime), "--database=" + database,
           "--batch", "--skip-column-names"]
    inventory = runtime.execute("albums-backup-inventory", sql + ["--execute=SELECT TABLE_NAME,ENGINE FROM information_schema.tables WHERE TABLE_SCHEMA=DATABASE() AND LEFT(TABLE_NAME,6)='album_' ORDER BY TABLE_NAME"], env=credentials).decode().splitlines()
    if inventory != [table + "\tInnoDB" for table in TABLES]:
        raise OperationError("Albums schema inventory differs; review coverage before backup")
    counts = {}
    digests = {}
    with tempfile.TemporaryDirectory(prefix=".albums-backup-", dir=runtime.backups) as directory:
        folder = Path(directory)
        raw = folder / "snapshot.sql"
        compressed = folder / "snapshot.sql.gz"
        encrypted = folder / "snapshot.enc"
        command = [str(binaries / "mysqldump"), "--no-defaults", *connection(runtime), "--single-transaction", "--quick",
                   "--hex-blob", "--no-tablespaces", "--set-gtid-purged=OFF", "--column-statistics=0",
                   "--skip-extended-insert", "--complete-insert", "--order-by-primary", "--skip-comments", "--default-character-set=utf8mb4", database, *TABLES]
        env = dict(os.environ, **credentials)
        with raw.open("xb") as output, (folder / "export.stderr").open("xb") as errors:
            os.chmod(raw, 0o600)
            result = subprocess.run(command, stdout=output, stderr=errors, env=env, timeout=900)
        if result.returncode:
            raise OperationError("Albums export failed; no backup published")
        with raw.open("rb") as source, gzip.open(compressed, "wb") as target:
            for line in source:
                created = re.match(rb"CREATE TABLE `([A-Za-z0-9_]+)`", line)
                inserted = re.match(rb"INSERT INTO `([A-Za-z0-9_]+)`", line)
                if created:
                    table = created[1].decode()
                    if table not in TABLES or table in counts:
                        raise OperationError("unexpected Albums export schema")
                    counts[table] = 0
                    digests[table] = hashlib.sha256()
                if inserted:
                    table = inserted[1].decode()
                    if table not in counts:
                        raise OperationError("unexpected Albums export data")
                    counts[table] += 1
                    digests[table].update(line)
                target.write(line)
        if set(counts) != set(TABLES):
            raise OperationError("incomplete Albums export")
        crypt(runtime, compressed, encrypted)
        filename = "albums-" + secrets.token_hex(16) + ".sql.gz.enc"
        m = {"scope": SCOPE, "created_at": int(time.time()), "database": database, "filename": filename,
             "table_rows": counts, "table_sha256": {t: d.hexdigest() for t, d in digests.items()}, "sha256": file_digest(encrypted)}
        destination = runtime.backups / filename
        with encrypted.open("rb") as handle:
            os.fsync(handle.fileno())
        os.replace(encrypted, destination)
        atomic_json(str(destination) + ".json", {"manifest": m, "hmac_sha256": manifest_signature(backup_secret(runtime), m)})
    return runtime.record("albums-backup", filename=filename, sha256=m["sha256"], tables=len(counts))


def drill(runtime, binaries, filename, ledger_path=None, ledger_digest=None):
    m = verify(runtime, runtime.backups / filename)
    if m["created_at"] <= time.time() - RETENTION:
        raise OperationError("Albums backup is outside retention")
    entries = None
    if ledger_path is not None or ledger_digest is not None:
        from deletion_ledger import verify as verify_ledger
        entries = verify_ledger(runtime, ledger_path, ledger_digest, m["created_at"])
    # No live restore target is accepted. Only this newly created, socket-only
    # server receives SQL; quarantine never substitutes for a deletion ledger.
    with tempfile.TemporaryDirectory(prefix="albums-restore-", dir=runtime.state) as directory:
        folder = Path(directory)
        compressed = folder / "snapshot.sql.gz"
        raw = folder / "snapshot.sql"
        crypt(runtime, runtime.backups / filename, compressed, decrypt=True)
        total = 0
        with gzip.open(compressed, "rb") as source, raw.open("xb") as output:
            os.chmod(raw, 0o600)
            while block := source.read(1024 * 1024):
                total += len(block)
                if total > 32 * 1024 ** 3:
                    raise OperationError("Albums restore exceeds the 32 GiB drill limit")
                output.write(block)
        data = folder / "data"
        data.mkdir(mode=0o700)
        sock = folder / "mysql.sock"
        with (folder / "mysql.stderr").open("xb") as diagnostic:
            initialized = subprocess.run([str(binaries / "mysqld"), "--no-defaults", "--initialize-insecure", "--datadir=" + str(data)], stdout=diagnostic, stderr=subprocess.STDOUT, timeout=120)
            if initialized.returncode:
                raise OperationError("isolated Albums database initialization failed")
            server = subprocess.Popen([str(binaries / "mysqld"), "--no-defaults", "--datadir=" + str(data), "--socket=" + str(sock), "--skip-networking", "--mysqlx=OFF", "--performance-schema=OFF", "--innodb-buffer-pool-size=67108864", "--pid-file=" + str(folder / "mysql.pid")], stdout=diagnostic, stderr=subprocess.STDOUT)
            try:
                sql = [str(binaries / "mysql"), "--no-defaults", "--protocol=socket", "--socket=" + str(sock), "--user=root", "--batch", "--skip-column-names"]
                for _ in range(300):
                    if server.poll() is not None:
                        raise OperationError("isolated Albums database stopped")
                    ready = subprocess.run(sql + ["--execute=SELECT 1"], capture_output=True, timeout=10)
                    if ready.returncode == 0:
                        break
                    time.sleep(0.1)
                else:
                    raise OperationError("isolated Albums database startup timed out")
                runtime.execute("albums-restore-create", sql + ["--execute=CREATE DATABASE albums_restore_test CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"])
                sql += ["--database=albums_restore_test"]
                with raw.open("rb") as source:
                    restored = subprocess.run(sql, stdin=source, stdout=subprocess.DEVNULL, stderr=diagnostic, timeout=900)
                if restored.returncode:
                    raise OperationError("isolated Albums restore failed")
                for table, count in m["table_rows"].items():
                    actual = runtime.execute("albums-restore-count", sql + ["--execute=SELECT COUNT(*) FROM `" + table + "`"]).decode().strip()
                    if int(actual) != count:
                        raise OperationError("Albums restored row count mismatch")
                # Compare exported row bytes too, including JSON and binary fields.
                restored_dump = folder / "restored.sql"
                command = [str(binaries / "mysqldump"), "--no-defaults", "--protocol=socket", "--socket=" + str(sock), "--user=root",
                           "--single-transaction", "--quick", "--hex-blob", "--no-tablespaces", "--set-gtid-purged=OFF", "--column-statistics=0",
                           "--skip-extended-insert", "--complete-insert", "--order-by-primary", "--skip-comments", "--default-character-set=utf8mb4",
                           "--no-create-info", "albums_restore_test", *TABLES]
                with restored_dump.open("xb") as output:
                    compared = subprocess.run(command, stdout=output, stderr=diagnostic, timeout=900)
                if compared.returncode:
                    raise OperationError("Albums restored content export failed")
                hashes = {table: hashlib.sha256() for table in TABLES}
                with restored_dump.open("rb") as source:
                    for line in source:
                        row = re.match(rb"INSERT INTO `([A-Za-z0-9_]+)`", line)
                        if row:
                            hashes[row[1].decode()].update(line)
                if any(hashes[t].hexdigest() != m["table_sha256"][t] for t in TABLES):
                    raise OperationError("Albums restored content differs from snapshot")
                runtime.execute("albums-restore-fence", sql + ["--execute=UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=UTC_TIMESTAMP(6) WHERE singleton_id=1"])
                if entries is not None:
                    from deletion_ledger import restore_fences
                    restore_fences(runtime, sql, entries)
                    restored_intents = runtime.execute("albums-restore-ledger-count", sql + ["--execute=SELECT COUNT(*) FROM album_deletion_ledger"]).decode().strip()
                    unblocked = runtime.execute("albums-restore-ledger-fences", sql + ["--execute=SELECT COUNT(*) FROM album_accounts a JOIN album_deletion_ledger l ON l.account_id=a.account_id LEFT JOIN album_storage_accounts s ON s.owner_id=a.account_id WHERE a.state<>'deleting' OR s.deletion_requested_at IS NULL OR s.deletion_requested_at<>l.requested_at OR s.subscription_checked_revision IS NOT NULL OR s.subscription_checked_at IS NOT NULL"]).decode().strip()
                    if restored_intents != str(len(entries)) or unblocked != "0":
                        raise OperationError("restored deletion intent or storage fence mismatch")
                runtime.execute("albums-restore-quarantine", sql, input=QUARANTINE.encode())
                exposed = runtime.execute("albums-restore-check", sql + ["--execute=SELECT IF((SELECT COUNT(*) FROM album_recovery_state WHERE singleton_id=1 AND quarantined=TRUE AND quarantined_at IS NOT NULL)=1,0,1)+(SELECT COUNT(*) FROM album_accounts WHERE state='active')+(SELECT COUNT(*) FROM album_sessions WHERE revoked_at IS NULL)+(SELECT COUNT(*) FROM album_share_links WHERE revoked_at IS NULL)+(SELECT COUNT(*) FROM album_push_registrations WHERE state<>'disabled')"]).decode().strip()
                if exposed != "0":
                    raise OperationError("Albums restore quarantine failed")
            finally:
                if server.poll() is None:
                    server.terminate()
                try:
                    server.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait(timeout=10)
    return runtime.record("albums-restore-drill", filename=filename, sha256=m["sha256"], tables=len(TABLES), quarantined=True, recovery_quarantined=True, deletion_ledger_applied=entries is not None, deletion_intents=len(entries) if entries is not None else 0, disposable_database_removed=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["backup", "drill", "prune", "ledger", "publish", "fetch", "publish-receipt"])
    parser.add_argument("--backend-dir", required=True)
    parser.add_argument("--environment-file", required=True)
    parser.add_argument("--backup-dir", required=True)
    parser.add_argument("--mysql-bin-dir", default="/usr/bin")
    parser.add_argument("--backup")
    parser.add_argument("--deletion-ledger")
    parser.add_argument("--deletion-ledger-sha256")
    parser.add_argument("--transfer-bin")
    parser.add_argument("--cos-config")
    parser.add_argument("--receipt")
    parser.add_argument("--receipt-sha256")
    args = parser.parse_args()
    os.umask(0o077)
    try:
        runtime = Runtime(args.backend_dir, args.backup_dir, args.environment_file)
        with runtime.lock():
            configure(runtime)
            binaries = Path(args.mysql_bin_dir)
            if args.operation == "backup": result = create(runtime, binaries)
            elif args.operation == "prune": result = {"removed": prune(runtime)}
            elif args.operation == "ledger":
                from deletion_ledger import publish
                sql = [str(binaries / "mysql"), "--no-defaults", *connection(runtime), "--database=" + runtime.config["MYSQL_DATABASE"], "--batch", "--skip-column-names"]
                result = publish(runtime, sql, {"MYSQL_PWD": runtime.config.get("MYSQL_PASSWORD", "")})
            elif args.operation == "publish":
                from offsite import publish
                if not args.backup: raise OperationError("publish requires a backup filename")
                result = publish(runtime, args.transfer_bin, args.cos_config, args.backup, args.deletion_ledger, args.deletion_ledger_sha256)
            elif args.operation == "publish-receipt":
                from offsite import publish_receipt
                result = publish_receipt(runtime, args.transfer_bin, args.cos_config, args.receipt, args.receipt_sha256)
            elif args.operation == "fetch":
                from offsite import fetch
                result = fetch(runtime, args.transfer_bin, args.cos_config, args.receipt, args.receipt_sha256)
            elif not args.backup: raise OperationError("drill requires a backup filename")
            else: result = drill(runtime, binaries, args.backup, args.deletion_ledger, args.deletion_ledger_sha256)
        print(json.dumps({"passed": True, **result}, sort_keys=True))
        return 0
    except OperationError as error:
        print(json.dumps({"passed": False, "error": str(error)}))
        return 1
    except (OSError, ValueError, KeyError, TypeError, tarfile.TarError, subprocess.TimeoutExpired):
        # Never expose a provider diagnostic, SQL content or credential value.
        print(json.dumps({"passed": False, "error": "Albums backup operation failed; no recovery readiness claimed"}))
        return 1


if __name__ == "__main__":
    sys.exit(main())
