"""Explicit real-COS extension of the disposable MySQL test, never a live backup source."""
from contextlib import contextmanager
import hmac
import json
import os
from pathlib import Path
import re
import shutil
import stat
from unittest.mock import patch

import backup
import deletion_ledger
import offsite
from ops_backup import backup_secret, file_digest, manifest_signature
from ops_common import OperationError, atomic_json

SCOPE = "albums-synthetic-offsite-probe-v1"


def private_json(path):
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > 65536:
            raise OperationError("COS probe configuration must be private")
        return json.loads(source.read(65537))


def configuration(root):
    root = Path(root)
    info = root.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o077:
        raise OperationError("COS probe directory must be private and must not be a link")
    c = private_json(root / "probe.json")
    if not isinstance(c, dict) or set(c) != {"scope", "writer", "cleanup", "transfer"} or c["scope"] != SCOPE:
        raise OperationError("explicit synthetic COS probe configuration required")
    if any(not isinstance(c[k], str) or not Path(c[k]).is_absolute() for k in ("writer", "cleanup", "transfer")):
        raise OperationError("COS probe requires explicit absolute credential and executable paths")
    target = offsite.destination(c["writer"])
    if (offsite.destination(c["cleanup"]) != target
            or not re.fullmatch(r"tellyouwhat-albums-backups(?:-test)?-[0-9]{5,20}", target["Bucket"])):
        raise OperationError("COS probe requires one dedicated Albums test backup bucket")
    binary = Path(c["transfer"]).lstat()
    if not stat.S_ISREG(binary.st_mode) or binary.st_mode & 0o022 or not binary.st_mode & 0o100:
        raise OperationError("COS probe transfer executable is unavailable or writable by others")
    return c, target


def recover(root):
    """Retry only this probe's authenticated intent; preserve local inputs on failure."""
    root = Path(root)
    c, target = configuration(root)
    runtime = backup.Runtime(root, root / "vault", root / "probe.env")
    backup.configure(runtime)
    if runtime.config["MYSQL_DATABASE"] != "albums_test":
        raise OperationError("COS probe recovery refuses another database scope")
    envelope = private_json(root / "intent.json")
    intent = envelope["intent"]
    if (not isinstance(intent, dict) or set(intent) != {"scope", "target", "snapshot", "created_at", "snapshot_sha256"}
            or intent["scope"] != SCOPE or intent["target"] != target
            or not hmac.compare_digest(envelope["hmac_sha256"], manifest_signature(backup_secret(runtime), intent))):
        raise OperationError("COS probe ownership authentication failed")
    with runtime.lock():
        backup.cleanup_staging(runtime)
        paths = list(runtime.backups.glob("albums-offsite-*.json"))
        if len(paths) > 1:
            raise OperationError("unexpected extra COS probe publication; preserved")
        results = []
        # Immediate cleanup is authorized only for this disposable test's synthetic pair.
        # A controlled clock exercises retention eligibility; it is not 14 days of live observation.
        with patch("offsite.time.time", return_value=intent["created_at"] + backup.RETENTION + 1):
            for path in paths:
                r, digest = offsite._read_receipt(runtime, path, None, expired=True)
                if r["snapshot"] != intent["snapshot"] or any(r["remote"][k] != v for k, v in target.items()):
                    raise OperationError("COS probe publication scope mismatch")
                results.append(offsite.expire(runtime, c["transfer"], c["cleanup"], path, digest))
        return results


@contextmanager
def publication(source, binaries, filename, sql, root):
    root = Path(root)
    # The only caller supplies this socket-only source created by backup_integration_test.py.
    socket = Path(source.config.get("MYSQL_SOCKET", ""))
    if (source.config.get("MYSQL_DATABASE") != "albums_test" or socket.name != "mysql.sock"
            or socket.parent.resolve() != source.root or source.root.parent != Path("/tmp").resolve()
            or not source.root.name.startswith("album-snapshot-")):
        raise OperationError("COS probe refuses a non-disposable database source")
    if sql != [str(binaries / "mysql"), "--no-defaults", "--protocol=socket", "--socket=" + str(socket),
               "--user=root", "--database=albums_test"]:
        raise OperationError("COS probe requires the fixture's exact socket-only MySQL command")
    c, target = configuration(root)
    if (root / "intent.json").exists() or (root / "vault").exists() or (root / "probe.env").exists():
        raise OperationError("COS probe already started; use recover() instead of publishing again")
    m = backup.verify(source, source.backups / filename)
    ledger = deletion_ledger.publish(source, sql + ["--batch", "--skip-column-names"], {"MYSQL_PWD": ""})
    env = root / "probe.env"
    with os.fdopen(os.open(env, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as output:
        output.write("MYSQL_DATABASE=albums_test\nALBUM_BACKUP_ENCRYPTION_KEY=" + backup_secret(source) + "\n")
        output.flush()
        os.fsync(output.fileno())
    runtime = backup.Runtime(root, root / "vault", env)
    backup.configure(runtime)
    for name in (filename, filename + ".json", ledger["filename"]):
        path = runtime.backups / name
        path.touch(mode=0o600, exist_ok=False)
        shutil.copyfile(source.backups / name, path)
    backup.verify(runtime, runtime.backups / filename)
    intent = dict(scope=SCOPE, target=target, snapshot=filename, created_at=m["created_at"],
                  snapshot_sha256=m["sha256"])
    atomic_json(root / "intent.json", dict(intent=intent, hmac_sha256=manifest_signature(backup_secret(runtime), intent)))
    report = dict(scope=SCOPE, synthetic_source=True, mysql_socket_only=True, table_count=len(m["table_rows"]),
                  source_sha256=m["sha256"], published=False, downloaded=False, restored=False,
                  cleanup_confirmed=False, retention_clock_controlled=True, transfer_sha256=file_digest(Path(c["transfer"])))
    try:
        with runtime.lock():
            published = offsite.publish(runtime, c["transfer"], c["writer"], filename,
                                       runtime.backups / ledger["filename"], ledger["sha256"])
            report.update(published=True, bundle_version=published["remote"]["Version"],
                          receipt_version=published["receipt_remote"]["Version"])
            checked = offsite.check_ledger(runtime, c["transfer"], c["writer"], published["receipt"],
                                           published["receipt_sha256"], sql + ["--batch", "--skip-column-names"], {"MYSQL_PWD": ""})
            if (checked["bundle_version"] != report["bundle_version"] or not checked["consistent_database_snapshot"]
                    or checked["authorizes_erasure_or_recovery"]):
                raise OperationError("COS probe did not match the pinned checkpoint to its source")
            report["checkpoint_matched_source"] = True
            fetched = offsite.fetch(runtime, c["transfer"], c["writer"], published["receipt"], published["receipt_sha256"])
            # Keep the drill's Unix socket under the fixture's short, owned scratch root.
            recovered = backup.Runtime(source.root, fetched["directory"], env)
            copied = backup.verify(recovered, recovered.backups / filename)
            if copied["sha256"] != m["sha256"]:
                raise OperationError("COS probe recovered snapshot differs from its source")
            report["downloaded"] = True
            yield recovered, fetched, report
    finally:
        try:
            removed = recover(root)
            report.update(cleanup_confirmed=bool(removed), cleanup=[r["remote"] for r in removed])
        finally:
            atomic_json(root / "report.json", report)


def run(source, binaries, filename, sql, root):
    with publication(source, binaries, filename, sql, root) as (recovered, fetched, report):
        result = backup.drill(recovered, binaries, filename, Path(fetched["deletion_ledger"]), fetched["deletion_ledger_sha256"])
        if not result["quarantined"] or not result["deletion_ledger_applied"]:
            raise OperationError("COS probe restore was not quarantined with its deletion fence")
        report["restored"] = True
    return report
