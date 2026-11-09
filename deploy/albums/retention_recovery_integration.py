"""Test-only bridge from the Go expiry worker to real, quarantined MySQL recovery."""
import hashlib
from contextlib import contextmanager
import json
import os
from pathlib import Path
import re
import secrets
import sys

import backup
import deletion_ledger
from ops_common import OperationError, atomic_json
from ops_backup import file_digest


def exercise(phase, folder, binaries):
    root = Path(os.environ["ALBUM_RETENTION_RECOVERY_ROOT"])
    folder, binaries = Path(folder), Path(binaries)
    if (root.is_symlink() or folder.is_symlink() or folder.parent != root
            or not root.name.startswith("albums-retention-")
            or (root / "fixture-owner.json").read_text() != "albums-retention-recovery-v1\n"
            or folder.stat().st_mode & 0o077):
        raise OperationError("private, explicitly owned fixture required")
    request = json.loads((folder / "request.json").read_text())
    for name in ["owner", "control_owner", "control_collection", "version_id"]:
        if not re.fullmatch(r"[0-9a-f-]{36}", request[name]):
            raise OperationError("invalid synthetic identity")
    if (request["scenario"] not in ("expired", "renewed", "provider-unavailable")
            or len(request["collections"]) != 2 or any(not re.fullmatch(r"[0-9a-f-]{36}", v) for v in request["collections"])
            or not re.fullmatch(r"[0-9a-f]{64}", request["manifest_digest"]) or type(request["expired"]) is not bool):
        raise OperationError("invalid synthetic recovery expectation")
    runtime = backup.Runtime(folder, folder / "vault", folder / "backup.env")
    database = backup.configure(runtime)
    if not database.startswith("albums_gateway") or not database.endswith("_test") or not runtime.config["MYSQL_SOCKET"].startswith(str(root) + os.sep):
        raise OperationError("refusing a non-fixture source")
    sql = [str(binaries / "mysql"), "--no-defaults", *backup.connection(runtime), "--database=" + database,
           "--batch", "--skip-column-names"]
    credentials = {"MYSQL_PWD": runtime.config["MYSQL_PASSWORD"]}

    def rows(command, label):
        owner, control = request["owner"], request["control_owner"]
        query = ("SELECT owner_id,collection_id,name,deleted,revision,request_id,DATE_FORMAT(updated_at,'%Y-%m-%dT%H:%i:%s.%fZ') "
                 f"FROM album_collections WHERE owner_id IN ('{owner}','{control}') ORDER BY owner_id,collection_id;"
                 + "SELECT 'members',COUNT(*) FROM album_collection_members WHERE owner_id='%s';" % owner
                 + "SELECT 'version',COUNT(*) FROM album_asset_versions WHERE owner_id='%s' AND version_id='%s' AND manifest_digest='%s';" % (owner, request["version_id"], request["manifest_digest"])
                 + "SELECT 'quota',used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id='%s';" % owner)
        return runtime.execute(label, command + ["--execute=" + query], env=credentials).decode().splitlines()

    if phase == "prepare":
        before = rows(sql, "retention-source-before")
        snapshot = backup.create(runtime, binaries)
        ledger = deletion_ledger.publish(runtime, sql, credentials)
        atomic_json(folder / "prepared.json", {"snapshot": snapshot, "ledger": ledger, "before": before})
        return {"phase": phase, "old_snapshot_created": True, "collection_count": 3}
    if phase != "recover":
        raise OperationError("unknown integration phase")
    prepared = json.loads((folder / "prepared.json").read_text())
    old = prepared["ledger"]
    try:
        deletion_ledger.verify_current(runtime, sql, credentials, runtime.backups / old["filename"], old["sha256"])
    except OperationError as error:
        if not request["expired"] or "differs from the current database" not in str(error):
            raise
    else:
        if request["expired"]:
            raise AssertionError("pre-expiry checkpoint was mistaken for current deletions")
    ledger = deletion_ledger.publish(runtime, sql, credentials)
    deletion_ledger.verify_current(runtime, sql, credentials, runtime.backups / ledger["filename"], ledger["sha256"])
    expected = 2 if request["expired"] else 0
    if (ledger["records"], ledger["asset_records"], ledger["collection_records"]) != (0, 0, expected):
        raise AssertionError("worker deletion checkpoints differ from actual expiry")
    current = rows(sql, "retention-source-after")
    if len(current) != 6 or "version\t1" not in current or "quota\t%d\t%d" % (request["used_bytes"], request["reserved_bytes"]) not in current:
        raise AssertionError("source originals or quota changed")
    owner_rows = [line.split("\t") for line in current if line.startswith(request["owner"] + "\t")]
    if len(owner_rows) != 2 or any((row[2], row[3], row[4]) != ("", "1", "2") if request["expired"] else (not row[2] or row[3:5] != ["0", "1"]) for row in owner_rows):
        raise AssertionError("expired groups resurrected or protected groups removed")
    if "members\t" + str(0 if request["expired"] else 2) not in current:
        raise AssertionError("incorrect retained membership")
    control = [line for line in current if line.startswith(request["control_owner"] + "\t")]
    if control != [line for line in prepared["before"] if line.startswith(request["control_owner"] + "\t")]:
        raise AssertionError("other account collection changed")
    inputs = {p.name: file_digest(p) for p in runtime.backups.iterdir() if p.is_file()}
    original = backup.restore_database
    original_temporary = backup.tempfile.TemporaryDirectory
    observations = []

    @contextmanager
    def keep_failure_diagnostic(*args, **kwargs):
        with original_temporary(*args, **kwargs) as name:
            try:
                yield name
            except BaseException:
                work = Path(name)
                diagnostic = work / "mysql.stderr"
                if work.parent == runtime.state and work.name.startswith("albums-restore-") and diagnostic.is_file():
                    saved = folder / ("restore-failure-" + secrets.token_hex(4) + ".log")
                    with saved.open("xb") as target:
                        saved.chmod(0o600)
                        target.write(diagnostic.read_bytes()[-65536:])
                raise

    def observe(rt, target, dump, manifest, raw, work, diagnostic, checkpoint):
        original(rt, target, dump, manifest, raw, work, diagnostic, checkpoint)
        # Observe the actual new MySQL server before the drill shuts it down.
        source_credentials = credentials.copy()
        credentials["MYSQL_PWD"] = ""
        try:
            actual = rows(target + ["--database=albums_restore_test"], "retention-restored-rows")
        finally:
            credentials.update(source_credentials)
        if actual != current:
            raise AssertionError("old backup restored retired grouping or changed originals")
        observations.append(hashlib.sha256("\n".join(actual).encode()).hexdigest())

    backup.restore_database = observe
    backup.tempfile.TemporaryDirectory = keep_failure_diagnostic
    try:
        for _ in range(2):
            result = backup.drill(runtime, binaries, prepared["snapshot"]["filename"], runtime.backups / ledger["filename"], ledger["sha256"])
            if not result["quarantined"] or result["collection_deletion_intents"] != expected or not result["disposable_database_removed"]:
                raise AssertionError("unquarantined or incomplete recovery")
        tampered = folder / "tampered-checkpoint.json"
        value = json.loads((runtime.backups / ledger["filename"]).read_text())
        value["payload"]["exported_at"] -= 1
        atomic_json(tampered, value)
        try:
            backup.drill(runtime, binaries, prepared["snapshot"]["filename"], tampered, file_digest(tampered))
        except OperationError as error:
            if "authentication failed" not in str(error):
                raise
        else:
            raise AssertionError("tampered checkpoint authorized recovery")
    finally:
        backup.restore_database = original
        backup.tempfile.TemporaryDirectory = original_temporary
    if len(observations) != 2 or observations[0] != observations[1]:
        raise AssertionError("repeat recovery changed rows")
    if current != rows(sql, "retention-source-preserved") or inputs != {p.name: file_digest(p) for p in runtime.backups.iterdir() if p.is_file()}:
        raise AssertionError("drill changed live fixture or recovery inputs")
    if any(p.is_dir() or p.is_symlink() for p in runtime.state.glob("albums-restore-*")):
        raise AssertionError("disposable restore resources remain")
    report = {"phase": phase, "scenario": request["scenario"], "expired": request["expired"], "real_mysql_restores": 2, "collection_deletion_intents": expected,
              "restored_rows_sha256": observations[0], "source_and_inputs_preserved": True, "tampered_checkpoint_rejected": True,
              "stale_checkpoint_rejected": request["expired"], "quarantined": True, "latest_checkpoint_proven": False,
              "recovery_ready": False, "cloud_requests": 0}
    atomic_json(folder / "result.json", report)
    return report


if __name__ == "__main__":
    print(json.dumps(exercise(*sys.argv[1:]), sort_keys=True))
