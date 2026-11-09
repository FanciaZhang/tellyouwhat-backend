"""Encrypted MySQL snapshots and disposable restore verification."""

import contextlib
import fcntl
import gzip
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import stat
import subprocess
import tempfile
import time

from ops_common import OperationError, atomic_json
import ops_offsite


# App data is local-first. Disaster recovery retains only the backend control
# plane, opaque deletion-completion receipts, and the identity-free project
# cost ledger, and operator-maintained encrypted Offer distribution records.
# Apple transaction-to-recipient associations are excluded with App purchase data.
# Every current or future table outside this allowlist is restored
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
    "prompt_eval_budget_holds",
    "prompt_config_revisions",
    "prompt_config_current",
    "prompt_config_mutations",
    "journal_voice_catalog_revisions",
    "journal_voice_catalog_current",
    "journal_voice_verifications",
    "journal_voice_catalog_mutations",
    "platform_ops_revisions",
    "platform_ops_current",
    "platform_ops_mutations",
    "platform_ops_patrol",
    "platform_ops_circuits",
    "platform_ops_incidents",
    "platform_ops_events",
    "health_ai_config_revisions",
    "health_ai_config_current",
    "health_ai_rollout_commands",
    "health_ai_endpoint_prices",
    "health_ai_model_attempts",
    "offer_delivery_pools",
    "offer_delivery_codes",
    "offer_delivery_requests",
    "offer_delivery_events",
    "offer_personal_deliveries",
    "offer_redemption_report_days",
    "offer_redemption_report_rows",
    "offer_redemption_report_sync",
)
BACKUP_RETENTION_SECONDS = 14 * 86400
BACKUP_BUDGET_BYTES = 6 * 1024**3


def require_backup_capacity(runtime):
    disk = shutil.disk_usage(runtime.backups)
    budget = min(BACKUP_BUDGET_BYTES, disk.total // 6)
    used = sum(path.stat().st_size for path in runtime.backups.rglob('*') if path.is_file() and not path.is_symlink())
    if used >= budget or disk.free < 2 * 1024**3:
        raise OperationError('backup storage budget exceeded; retain valid snapshots and review producer growth')
    return min(budget - used, disk.free - 2 * 1024**3)


def prune_backup_temporaries(runtime):
    prune_restore_workspaces(runtime)
    for path in runtime.backups.iterdir():
        if (re.fullmatch(r'\.(backup|recovery)-[A-Za-z0-9_-]{6,}', path.name) and path.is_dir()
                and not path.is_symlink() and path.stat().st_mtime < time.time() - 48 * 3600):
            shutil.rmtree(path)


def _restore_workspace_owner(runtime, folder, kind, reference):
    from ops_cos import valid_filename
    if (kind not in ('download', 'sql') or not isinstance(reference, str)
            or not re.fullmatch(r'\.restore-' + kind + r'-[a-z0-9_]{8}', folder.name)):
        raise OperationError('invalid restore workspace owner')
    if kind == 'download':
        if not valid_filename(reference) or not reference.endswith('.sql.gz.enc'):
            raise OperationError('invalid restore download selection')
        names = (reference, reference + '.json', 'recovery-' + reference.replace('.sql.gz.enc', '.tar.gz.enc'))
    else:
        if not re.fullmatch(r'tellyouwhat-restore-[0-9a-f]{16}', reference):
            raise OperationError('invalid restore container selection')
        names = ('snapshot.sql.gz', 'restore.stderr')
    return {'scope': 'shared-restore-workspace-v1', 'database': runtime.config['MYSQL_DATABASE'],
            'directory': folder.name, 'kind': kind, 'reference': reference}, names


def _close_restore_container(runtime, owner):
    name = owner['reference']
    command = ['docker', 'ps', '-aq', '--no-trunc', '--filter', 'name=' + name]
    ids = runtime.execute('restore-workspace-list', command, timeout=30).decode().split()
    if not ids:
        return
    if len(ids) != 1 or not re.fullmatch(r'[0-9a-f]{64}', ids[0]):
        raise OperationError('restore container inventory is ambiguous; retained')
    items = json.loads(runtime.execute('restore-workspace-inspect', ['docker', 'inspect', ids[0]], timeout=30))
    if not isinstance(items, list) or len(items) != 1:
        raise OperationError('invalid restore container inventory; retained')
    item = items[0]
    labels = item.get('Config', {}).get('Labels') or {}
    if (item.get('Name') != '/' + name or item.get('Id') != ids[0]
            or labels.get('cn.tellyouwhat.operation') != 'restore-drill'
            or labels.get('cn.tellyouwhat.restore-workspace') != owner['directory']):
        raise OperationError('restore container ownership differs; retained')
    runtime.execute('restore-workspace-cleanup', ['docker', 'rm', '--force', ids[0]], timeout=30)
    if runtime.execute('restore-workspace-list', command, timeout=30).strip():
        raise OperationError('restore container termination is not confirmed; retained')


def _remove_restore_workspace(runtime, folder):
    folder = Path(folder)
    info = folder.lstat()
    if (folder.parent != runtime.state or not re.fullmatch(r'\.restore-(download|sql)-[a-z0-9_]{8}', folder.name)
            or not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o077
            or info.st_uid != runtime.state.stat().st_uid):
        raise OperationError('invalid restore workspace; retained')
    if not any(folder.iterdir()):
        folder.rmdir()
        return
    with contextlib.ExitStack() as leases:
        directory = os.open(folder, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        leases.callback(os.close, directory)
        opened_directory = os.fstat(directory)
        if (opened_directory.st_dev, opened_directory.st_ino) != (info.st_dev, info.st_ino):
            raise OperationError('restore workspace changed; retained')
        metadata = folder / 'owner.json'
        handle, before = leases.enter_context(locked_backup_file(metadata, True))
        if before.st_size > 4096:
            raise OperationError('oversized restore workspace owner; retained')
        envelope = json.loads(handle.read(4097))
        owner = envelope['owner']
        expected, names = _restore_workspace_owner(runtime, folder, owner['kind'], owner['reference'])
        if (set(envelope) != {'owner', 'hmac_sha256'} or owner != expected
                or not hmac.compare_digest(manifest_signature(backup_secret(runtime), owner), envelope['hmac_sha256'])):
            raise OperationError('restore workspace authentication failed; retained')
        files = {metadata: (handle, before)}
        for path in folder.iterdir():
            if path == metadata:
                continue
            if path.name not in names:
                raise OperationError('unknown restore workspace file; retained')
            files[path] = leases.enter_context(locked_backup_file(path, True))
        for path, (_, current) in files.items():
            if current.st_uid != info.st_uid or current.st_mode & 0o077:
                raise OperationError('restore workspace file is not private; retained')
        if owner['kind'] == 'sql':
            _close_restore_container(runtime, owner)
        current_directory = folder.lstat()
        if (current_directory.st_dev, current_directory.st_ino) != (info.st_dev, info.st_ino):
            raise OperationError('restore workspace changed; retained')
        if (set(folder.iterdir()) != set(files)
                or any(not _same_backup_file(p, h, previous) for p, (h, previous) in files.items())):
            raise OperationError('restore workspace files changed; retained')
        for path in sorted(files, key=lambda p: p == metadata):
            current = os.stat(path.name, dir_fd=directory, follow_symlinks=False)
            previous = files[path][1]
            if (current.st_dev, current.st_ino, current.st_size, current.st_mtime_ns, current.st_ctime_ns) != (previous.st_dev, previous.st_ino, previous.st_size, previous.st_mtime_ns, previous.st_ctime_ns):
                raise OperationError('restore workspace file changed; retained')
            if path == metadata:
                if set(os.listdir(directory)) != {'owner.json'}:
                    raise OperationError('late restore workspace file; retained')
                os.fsync(directory)
            os.unlink(path.name, dir_fd=directory)
        os.fsync(directory)
        current_directory = folder.lstat()
        if (current_directory.st_dev, current_directory.st_ino) != (info.st_dev, info.st_ino):
            raise OperationError('restore workspace changed; retained')
        folder.rmdir()


def prune_restore_workspaces(runtime):
    if runtime.state.is_symlink():
        raise OperationError('restore workspace parent is a symlink; retained')
    with runtime.lock('restore-workspaces'):
        for folder in sorted(runtime.state.glob('.restore-*')):
            try:
                _remove_restore_workspace(runtime, folder)
            except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
                raise OperationError('restore workspace cleanup failed; retained for review or retry') from error


@contextlib.contextmanager
def restore_workspace(runtime, kind, reference):
    if kind not in ('download', 'sql') or runtime.state.is_symlink():
        raise OperationError('invalid restore workspace selection')
    _restore_workspace_owner(runtime, runtime.state / ('.restore-' + kind + '-00000000'), kind, reference)
    with runtime.lock('restore-workspaces'):
        folder = Path(tempfile.mkdtemp(prefix='.restore-' + kind + '-', dir=runtime.state))
        lease = None
        try:
            owner, _ = _restore_workspace_owner(runtime, folder, kind, reference)
            atomic_json(folder / 'owner.json', {'owner': owner, 'hmac_sha256': manifest_signature(backup_secret(runtime), owner)})
            lease = (folder / 'owner.json').open('rb')
            fcntl.flock(lease, fcntl.LOCK_SH | fcntl.LOCK_NB)
        except BaseException:
            if lease is not None:
                lease.close()
            if not any(folder.iterdir()):
                folder.rmdir()
            raise
    inherited = getattr(runtime, '_child_pass_fds', ())
    runtime._child_pass_fds = (*inherited, lease.fileno())
    try:
        yield folder
    finally:
        runtime._child_pass_fds = inherited
        lease.close()
        with runtime.lock('restore-workspaces'):
            try:
                _remove_restore_workspace(runtime, folder)
            except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
                raise OperationError('restore workspace cleanup failed; retained for review or retry') from error


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


def create_backup(runtime, *, scope='host'):
    if scope not in ('host', 'production'):
        raise OperationError('unsupported backup recovery scope')
    operation = 'backup' if scope == 'host' else 'production_backup'
    try:
        result = export_backup(runtime, scope=scope)
    except (OperationError, OSError, subprocess.TimeoutExpired):
        runtime.record(operation + '-failure', passed=False)
        raise
    (runtime.state / (operation + '-failure.json')).unlink(missing_ok=True)
    return result


def export_backup(runtime, *, scope='host'):
    backup_secret(runtime)
    database = runtime.config.get("MYSQL_DATABASE", "")
    if not re.fullmatch(r"[A-Za-z0-9_]+", database) or database in ("mysql", "sys", "information_schema", "performance_schema"):
        raise OperationError("a dedicated application database is required")
    runtime.backups.mkdir(parents=True, exist_ok=True, mode=0o700)
    prune_backups(runtime)
    available_bytes = require_backup_capacity(runtime)
    timestamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    destination = runtime.backups / ("mysql-" + timestamp + "-" + secrets.token_hex(3) + ".sql.gz.enc")
    image = runtime.config.get("MYSQL_BACKUP_IMAGE", "mysql:8.4")
    counts = {}
    connection = ["--host=" + runtime.config["MYSQL_HOST"], "--port=" + runtime.config.get("MYSQL_PORT", "3306"),
                  "--user=" + runtime.config["MYSQL_USER"]]
    child_env = {"MYSQL_PWD": runtime.config["MYSQL_PASSWORD"]}
    table_output = runtime.execute("backup-table-inventory", [
        "docker", "run", "--rm", "--log-driver=none", "--network", "host", "--env", "MYSQL_PWD", image, "mysql", *connection,
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
    command = ["docker", "run", "--rm", "--log-driver=none", "--network", "host", "--env", "MYSQL_PWD", image, "mysqldump",
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
                            # Reserve room for encryption and recovery assembly before writing more SQL.
                            if output.tell() > available_bytes // 4:
                                raise OperationError('SQL export exceeds its temporary storage budget')
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
        try:
            recovery = ops_offsite.create_recovery_bundle(runtime, destination.name, scope=scope) if ops_offsite.enabled(runtime) else {}
            require_backup_capacity(runtime)
            manifest = {"version": 1, "created_at": int(time.time()), "database": database, 'recovery_scope': scope,
                        "filename": destination.name, "image": image, "table_rows": counts,
                        "included_data_tables": included_data_tables,
                        "excluded_data_tables": excluded_data_tables,
                        "sha256": file_digest(encrypted), **recovery}
            signature = manifest_signature(backup_secret(runtime), manifest)
            os.replace(encrypted, destination)
            atomic_json(str(destination) + ".json", {"manifest": manifest, "hmac_sha256": signature})
        except BaseException:
            destination.unlink(missing_ok=True)
            Path(str(destination) + '.json').unlink(missing_ok=True)
            (destination.parent / ('recovery-' + destination.name.replace('.sql.gz.enc', '.tar.gz.enc'))).unlink(missing_ok=True)
            raise
    if ops_offsite.enabled(runtime):
        ops_offsite.publish(runtime, destination, manifest)
    return runtime.record('backup' if scope == 'host' else 'production_backup', filename=destination.name, sha256=manifest["sha256"], tables=len(counts))


def prune_backups(runtime):
    prune_backup_temporaries(runtime)
    cutoff = time.time() - BACKUP_RETENTION_SECONDS
    removed = 0
    failed = 0
    snapshots = set(runtime.backups.glob("mysql-*.sql.gz.enc"))
    snapshots.update(Path(str(p)[:-5]) for p in runtime.backups.glob("mysql-*.sql.gz.enc.json"))
    snapshots.update(p.parent / (p.name[len("recovery-"):-len(".tar.gz.enc")] + ".sql.gz.enc")
                     for p in runtime.backups.glob("recovery-mysql-*.tar.gz.enc"))
    for path in sorted(snapshots):
        try:
            with _verified_backup(runtime, path, pruning_cutoff=cutoff) as (manifest, files):
                if manifest["created_at"] > cutoff:
                    continue
                metadata = Path(str(path) + ".json")
                # Keep the authenticated inventory until every payload is gone.
                # A killed process can then resume without trusting file ages.
                order = [p for p in files if p != metadata] + [metadata]
                directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
                try:
                    for member in order:
                        handle, before = files[member]
                        if member == metadata:
                            recovery = path.parent / ('recovery-' + path.name.replace('.sql.gz.enc', '.tar.gz.enc'))
                            if any(p.exists() or p.is_symlink() for p in (path, recovery)):
                                raise OperationError("backup payload reappeared during retention; inventory retained")
                        if not _same_backup_file(member, handle, before):
                            raise OperationError("backup changed during retention; preserved for review")
                        member.unlink()
                        os.fsync(directory)
                finally:
                    os.close(directory)
                removed += 1
        except (OperationError, OSError):
            failed += 1
    if failed:
        raise OperationError(f"backup retention verification failed for {failed} snapshots; operator review required")
    return removed


@contextlib.contextmanager
def locked_backup_file(path, exclusive):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as handle:
        before = os.fstat(handle.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
            raise OperationError("backup must be a regular file without additional links")
        try:
            fcntl.flock(handle.fileno(), (fcntl.LOCK_EX if exclusive else fcntl.LOCK_SH) | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise OperationError("backup file is active; retained") from error
        if not _same_backup_file(Path(path), handle, before):
            raise OperationError("backup changed while acquiring its lease")
        yield handle, before


def _same_backup_file(path, handle, before):
    fields = ("st_dev", "st_ino", "st_mode", "st_nlink", "st_uid", "st_size", "st_mtime_ns", "st_ctime_ns")
    return all(getattr(before, field) == getattr(current, field)
               for current in (os.fstat(handle.fileno()), path.lstat()) for field in fields)


def _backup_manifest(runtime, path, envelope):
    try:
        manifest = envelope["manifest"]
        if manifest.get('recovery_scope', 'host') not in ('host', 'production'):
            raise OperationError('invalid backup recovery scope')
        if manifest.get("scope") not in (None, "control-plane"):
            raise OperationError("backup belongs to a different recovery scope")
        expected = manifest_signature(backup_secret(runtime), manifest)
        if not hmac.compare_digest(expected, envelope["hmac_sha256"]):
            raise OperationError("backup manifest authentication failed")
        if (manifest["filename"] != path.name or not isinstance(manifest["sha256"], str)
                or not re.fullmatch(r"[0-9a-f]{64}", manifest["sha256"])):
            raise OperationError("backup content verification failed")
        if manifest.get("recovery_filename"):
            name = manifest["recovery_filename"]
            if (name != "recovery-" + path.name.replace(".sql.gz.enc", ".tar.gz.enc")
                    or not isinstance(manifest["recovery_sha256"], str)
                    or not re.fullmatch(r"[0-9a-f]{64}", manifest["recovery_sha256"])):
                raise OperationError("configuration recovery content verification failed")
        if manifest["database"] != runtime.config["MYSQL_DATABASE"]:
            raise OperationError("backup belongs to another application database")
        if type(manifest["created_at"]) is not int or manifest["created_at"] <= 0:
            raise OperationError("invalid backup creation time")
        if (not isinstance(manifest["table_rows"], dict) or not manifest["table_rows"]
                or not all(isinstance(table, str) and re.fullmatch(r"[A-Za-z0-9_]+", table)
                           and type(count) is int and count >= 0 for table, count in manifest["table_rows"].items())):
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
    except (ValueError, KeyError, TypeError, AttributeError) as error:
        raise OperationError("backup manifest is missing or invalid") from error


@contextlib.contextmanager
def _verified_backup(runtime, path, *, pruning_cutoff=None):
    path = Path(path)
    try:
        with contextlib.ExitStack() as leases:
            metadata = Path(str(path) + ".json")
            files = {metadata: leases.enter_context(locked_backup_file(metadata, pruning_cutoff is not None))}
            handle, before = files[metadata]
            if before.st_size > 1024 * 1024:
                raise OperationError("backup manifest exceeds its size limit")
            manifest = _backup_manifest(runtime, path, json.loads(handle.read(1024 * 1024 + 1)))
            expired = pruning_cutoff is not None and manifest["created_at"] <= pruning_cutoff
            recovery = path.parent / ('recovery-' + path.name.replace('.sql.gz.enc', '.tar.gz.enc'))
            if (pruning_cutoff is not None and not manifest.get('recovery_filename')
                    and (recovery.exists() or recovery.is_symlink())):
                raise OperationError("unregistered recovery package; inventory retained for review")
            payloads = [(path, manifest["sha256"])]
            if manifest.get("recovery_filename"):
                payloads.append((path.parent / manifest["recovery_filename"], manifest["recovery_sha256"]))
            for member, expected in payloads:
                try:
                    files[member] = leases.enter_context(locked_backup_file(member, pruning_cutoff is not None))
                except FileNotFoundError:
                    if expired:
                        continue
                    raise
                digest = hashlib.sha256()
                for block in iter(lambda: files[member][0].read(1024 * 1024), b""):
                    digest.update(block)
                if not hmac.compare_digest(digest.hexdigest(), expected):
                    raise OperationError("backup content verification failed" if member == path
                                         else "configuration recovery content verification failed")
            if not all(_same_backup_file(p, h, info) for p, (h, info) in files.items()):
                raise OperationError("backup changed during verification; preserved for review")
            yield manifest, files
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise OperationError("backup manifest or payload is missing, active or invalid") from error


def verify_backup(runtime, path):
    with _verified_backup(runtime, path) as (manifest, _):
        return manifest


def restore_drill(runtime, filename=None, *, source_directory=None):
    if source_directory is None:
        prune_restore_workspaces(runtime)
    if source_directory is None and ops_offsite.enabled(runtime):
        filename = filename or json.loads((runtime.state / "backup.json").read_text())["filename"]
        with restore_workspace(runtime, 'download', filename) as directory:
            ops_offsite.download(runtime, filename, directory)
            result = restore_drill(runtime, filename, source_directory=Path(directory))
        return runtime.record("restore", **{k: v for k, v in result.items() if k not in ("operation", "completed_at")}, source="cos")
    if filename:
        if Path(filename).name != filename:
            raise OperationError("restore drill accepts only a backup filename")
        source = (source_directory or runtime.backups) / filename
    else:
        try:
            source = runtime.backups / json.loads((runtime.state / "backup.json").read_text())["filename"]
        except (OSError, KeyError, ValueError) as error:
            raise OperationError("a successful backup is required before a restore drill") from error
    manifest = verify_backup(runtime, source)
    if manifest.get("recovery_filename"):
        ops_offsite.verify_recovery_bundle(runtime, source, manifest)
    if manifest["created_at"] <= time.time() - BACKUP_RETENTION_SECONDS:
        raise OperationError("backup is outside the retention period")
    container = "tellyouwhat-restore-" + secrets.token_hex(8)
    password = secrets.token_urlsafe(32)
    database = "tellyouwhat_restore_test"
    with restore_workspace(runtime, 'sql', container) as directory:
        compressed = Path(directory) / "snapshot.sql.gz"
        compressed.touch(mode=0o600)
        crypt(runtime, source, compressed, decrypt=True)
        runtime.execute("restore-start", ["docker", "run", "--detach", "--rm", "--name", container,
                        "--label", "cn.tellyouwhat.operation=restore-drill",
                        "--label", "cn.tellyouwhat.restore-workspace=" + directory.name, "--network", "none",
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
            ready = subprocess.run(sql + ["--execute=SELECT 1"], capture_output=True, env=child_env, timeout=10,
                                   pass_fds=runtime._child_pass_fds)
            if ready.returncode == 0:
                break
            time.sleep(2)
        else:
            raise OperationError("isolated restore database did not become ready")
        diagnostic_path = Path(directory) / "restore.stderr"
        diagnostic_path.touch(mode=0o600)
        with diagnostic_path.open("wb") as diagnostic:
            process = subprocess.Popen(sql, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                                       stderr=diagnostic, env=child_env, pass_fds=runtime._child_pass_fds)
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
    return runtime.record("restore", filename=source.name, sha256=manifest["sha256"], tables=len(manifest["table_rows"]))
