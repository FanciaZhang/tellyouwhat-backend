"""Concurrent checkpoint checks inside the existing disposable backup fixture."""
from concurrent.futures import ThreadPoolExecutor
from copy import copy
import datetime
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import time
from unittest.mock import patch
import uuid

import backup
import deletion_ledger
from ops_common import OperationError


def check_snapshot(test, source, binaries, sql):
    # Reuse the backup test's socket-only server and creator-owned directory.
    test.assertTrue(source.root.name.startswith("album-snapshot-"))
    test.assertEqual(Path(source.config["MYSQL_SOCKET"]).parent.resolve(), source.root.resolve())
    root_sql = [*sql, "--batch", "--skip-column-names"]
    environment = {k: v for k, v in os.environ.items() if k != "MYSQL_PWD"}

    def execute(statement):
        return subprocess.run(root_sql, input=statement.encode(), env=environment,
                              capture_output=True, check=True, timeout=30).stdout

    folder = source.root / "checkpoint-view"
    folder.mkdir(mode=0o700)
    user, password = "cp_" + uuid.uuid4().hex[:16], os.urandom(32).hex()
    execute("CREATE USER '" + user + "'@'localhost' IDENTIFIED BY '" + password + "'; GRANT SELECT ON albums_test.* TO '" + user + "'@'localhost';")
    settings = folder / "backup.env"
    settings.write_text("MYSQL_DATABASE=albums_test\nMYSQL_USER=" + user + "\nMYSQL_PASSWORD=" + password +
                       "\nMYSQL_SOCKET=" + source.config["MYSQL_SOCKET"] + "\nALBUM_BACKUP_ENCRYPTION_KEY=" + backup.backup_secret(source) + "\n")
    settings.chmod(0o600)
    runtime = backup.Runtime(folder, folder / "vault", settings)
    backup.configure(runtime)
    reader_sql = [str(binaries / "mysql"), "--no-defaults", *backup.connection(runtime),
                  "--database=albums_test", "--batch", "--skip-column-names"]
    credentials = {"MYSQL_PWD": password}

    def cli_check(checkpoint):
        result = subprocess.run([sys.executable, str(Path(backup.__file__)), "check-ledger", "--backend-dir", str(folder),
            "--environment-file", str(settings), "--backup-dir", str(runtime.backups),
            "--mysql-bin-dir", str(binaries), "--deletion-ledger", str(runtime.backups / checkpoint["filename"]),
            "--deletion-ledger-sha256", checkpoint["sha256"]], env=environment, capture_output=True, timeout=30)
        return result.returncode, json.loads(result.stdout)

    owner, previous_owner, request, collection, version = [str(uuid.uuid4()) for _ in range(5)]
    try:
        initial = deletion_ledger.current(runtime, reader_sql, credentials)
        stamp = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(seconds=1)).strftime("%Y-%m-%d %H:%M:%S.%f")
        execute("""
INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES ('%s',SHA2('%s',256));
INSERT INTO album_deletion_ledger(account_id,request_id,authorizing_session_sha256,requested_at)
 VALUES ('%s','%s',UNHEX(REPEAT('b',64)),'%s');
INSERT INTO album_asset_deletion_ledger(owner_id,asset_id,final_revision,reason,purged_at,version_ids)
 VALUES ('%s','older',2,'trash','%s',JSON_ARRAY('%s'));
INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at)
 VALUES ('%s','%s',2,'%s','',TRUE,'%s');
""" % (owner, owner, previous_owner, request, stamp, previous_owner, stamp, version, owner, collection, request, stamp))
        baseline = deletion_ledger.publish(runtime, reader_sql, credentials)
        before = json.loads((runtime.backups / baseline["filename"]).read_text())["payload"]
        test.assertEqual([len(before[k]) for k in ("entries", "assets", "collections")], [len(initial[k]) + 1 for k in ("entries", "assets", "collections")])
        test.assertEqual(cli_check(baseline)[0], 0)

        # A restore can retain exactly these deletion rows. Its recovery marker
        # must still prevent it from acting as the source of a freshness check.
        for mark in (
            "UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=UTC_TIMESTAMP(6) WHERE singleton_id=1;",
            "DELETE FROM album_recovery_state WHERE singleton_id=1;",
        ):
            execute(mark)
            try:
                code, rejected = cli_check(baseline)
                test.assertNotEqual(code, 0, rejected)
                test.assertFalse(rejected["passed"])
                test.assertIn("active source", rejected["error"])
                for vault, operation in (
                    (runtime.backups, lambda: deletion_ledger.publish(runtime, reader_sql, credentials)),
                    (source.backups, lambda: backup.create(source, binaries)),
                ):
                    with test.subTest(mark=mark, vault=vault.name):
                        files = set(vault.iterdir())
                        with test.assertRaisesRegex(OperationError, "active source"):
                            operation()
                        test.assertEqual(set(vault.iterdir()), files)
            finally:
                execute("INSERT INTO album_recovery_state(singleton_id) VALUES(1) ON DUPLICATE KEY UPDATE quarantined=FALSE,quarantined_at=NULL;")
        test.assertEqual(cli_check(baseline)[0], 0)

        wrong_source = copy(runtime)
        wrong_source.config = {**runtime.config, "MYSQL_DATABASE": "albums_production"}
        with test.subTest(case="wrong selected database"):
            files = set(runtime.backups.iterdir())
            with test.assertRaisesRegex(OperationError, "active source"):
                deletion_ledger.publish(wrong_source, reader_sql, credentials)
            test.assertEqual(set(runtime.backups.iterdir()), files)

        original_run = subprocess.run

        def quarantine_before_dump(command, **kwargs):
            if Path(command[0]).name == "mysqldump":
                execute("UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=UTC_TIMESTAMP(6) WHERE singleton_id=1;")
            return original_run(command, **kwargs)

        with test.subTest(case="quarantined after preflight"):
            files = set(source.backups.iterdir())
            try:
                with patch.object(subprocess, "run", side_effect=quarantine_before_dump):
                    with test.assertRaisesRegex(OperationError, "active source"):
                        backup.create(source, binaries)
                test.assertEqual(set(source.backups.iterdir()), files)
            finally:
                execute("UPDATE album_recovery_state SET quarantined=FALSE,quarantined_at=NULL WHERE singleton_id=1;")

        name = "albums_checkpoint_" + uuid.uuid4().hex
        gate = subprocess.Popen(root_sql + ["--unbuffered"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, text=True, env=environment)

        def gate_query(query):
            gate.stdin.write(query + ";\n")
            gate.stdin.flush()
            test.assertTrue(select.select([gate.stdout], [], [], 5)[0], "owned gate did not respond")
            test.assertEqual(gate.stdout.readline().strip(), "1")

        original_execute = runtime.execute

        def pause_after_account_read(label, command, **kwargs):
            if label == "albums-deletion-ledger-export":
                test.assertIn("SELECT '__albums_assets__';", command[-1])
                command = [*command[:-1], command[-1].replace("SELECT '__albums_assets__';",
                    "DO GET_LOCK('" + name + "',20);DO RELEASE_LOCK('" + name + "');SELECT '__albums_assets__';", 1)]
            return original_execute(label, command, **kwargs)

        try:
            gate_query("SELECT GET_LOCK('" + name + "',0)")
            with ThreadPoolExecutor(max_workers=1) as pool, patch.object(runtime, "execute", side_effect=pause_after_account_read):
                pending = pool.submit(deletion_ledger.publish, runtime, reader_sql, credentials)
                try:
                    for _ in range(100):
                        blocked = execute("SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE STATE='User lock' AND INFO LIKE '%" + name + "%';").strip()
                        if blocked == b"1":
                            break
                        test.assertFalse(pending.done(), "snapshot finished before the concurrent commit")
                        time.sleep(.05)
                    else:
                        test.fail("snapshot did not reach the owned read gate")
                    execute("""
START TRANSACTION;
INSERT INTO album_deletion_ledger(account_id,request_id,authorizing_session_sha256,requested_at)
 VALUES ('%s','%s',UNHEX(REPEAT('c',64)),'%s');
INSERT INTO album_asset_deletion_ledger(owner_id,asset_id,final_revision,reason,purged_at,version_ids)
 VALUES ('%s','newer',2,'trash','%s',JSON_ARRAY('%s'));
DELETE FROM album_collections WHERE owner_id='%s';
COMMIT;
""" % (owner, str(uuid.uuid4()), stamp, owner, stamp, str(uuid.uuid4()), owner))
                finally:
                    gate_query("SELECT RELEASE_LOCK('" + name + "')")
                snapshot = pending.result(timeout=30)
        finally:
            gate.stdin.close()
            try:
                gate.wait(timeout=5)
            except subprocess.TimeoutExpired:
                gate.kill()
                gate.wait(timeout=5)
            gate.stdout.close()

        captured = json.loads((runtime.backups / snapshot["filename"]).read_text())["payload"]
        for field in ("entries", "assets", "collections"):
            test.assertEqual(captured[field], before[field], "checkpoint mixed pre/post-commit deletion fences")
        with test.assertRaisesRegex(OperationError, "differs from the current database"):
            deletion_ledger.verify_current(runtime, reader_sql, credentials,
                                          runtime.backups / snapshot["filename"], snapshot["sha256"])
        code, stale = cli_check(snapshot)
        test.assertNotEqual(code, 0)
        test.assertFalse(stale["passed"])
        latest = deletion_ledger.publish(runtime, reader_sql, credentials)
        code, verified = cli_check(latest)
        test.assertEqual(code, 0)
        test.assertTrue(verified["point_in_time_only"])
        test.assertFalse(verified["authorizes_erasure_or_recovery"])
        after = json.loads((runtime.backups / latest["filename"]).read_text())["payload"]
        test.assertEqual([len(after[k]) for k in ("entries", "assets", "collections")], [len(before["entries"]) + 1, len(before["assets"]) + 1, len(before["collections"]) - 1])
        test.assertEqual(int(execute("SELECT COUNT(*) FROM album_deletion_ledger;").strip()), len(after["entries"]))
        test.assertEqual(int(execute("SELECT COUNT(*) FROM album_asset_deletion_ledger;").strip()), len(after["assets"]))
    finally:
        # Only rows and the SELECT identity created by this check; source data stays intact.
        execute("DELETE FROM album_asset_deletion_ledger WHERE owner_id IN ('" + owner + "','" + previous_owner + "');"
                "DELETE FROM album_deletion_ledger WHERE account_id IN ('" + owner + "','" + previous_owner + "');"
                "DELETE FROM album_collections WHERE owner_id='" + owner + "' AND collection_id='" + collection + "';"
                "DELETE FROM album_accounts WHERE account_id='" + owner + "';"
                "DROP USER '" + user + "'@'localhost';")
