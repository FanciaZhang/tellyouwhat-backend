import datetime
import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

from backup import Runtime, configure, prune, RETENTION
from deletion_ledger import publish, verify, validate, SCOPE, MAX_RECORDS
from ops_backup import backup_secret, manifest_signature
from ops_common import OperationError, atomic_json


class DeletionLedgerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="album-ledger-test-")
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        env = root / "backup.env"
        env.write_text("MYSQL_DATABASE=albums_fixture\nALBUM_BACKUP_ENCRYPTION_KEY=" + "b" * 64 + "\n")
        env.chmod(0o600)
        self.runtime = Runtime(root, root / "vault", env)
        configure(self.runtime)
        now = datetime.datetime.now(datetime.timezone.utc)
        self.entries = [["10000000-0000-4000-8000-000000000001", "20000000-0000-4000-8000-000000000001", "aa" * 32, now.strftime("%Y-%m-%dT%H:%M:%S.%fZ")]]
        self.payload = {"scope": SCOPE, "database": "albums_fixture", "exported_at": int(now.timestamp()), "entries": self.entries, "assets": [], "collections": []}
        self.path = self.runtime.backups / "ledger.json"

    def write(self):
        atomic_json(self.path, {"payload": self.payload, "hmac_sha256": manifest_signature(backup_secret(self.runtime), self.payload)})
        return hashlib.sha256(self.path.read_bytes()).hexdigest()

    def test_export_is_private_authenticated_and_explicitly_local(self):
        with patch.object(self.runtime, "execute", side_effect=[b"1\t1\n", b"", ("\t".join(self.entries[0]) + "\n").encode(), b"1\t1\n", b""]):
            result = publish(self.runtime, ["mysql-fixture"], {})
        path = self.runtime.backups / result["filename"]
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assertFalse(result["offsite"])
        self.assertEqual(verify(self.runtime, path, result["sha256"], 0), self.entries)

    def test_checkpoint_includes_deleted_collection_fences(self):
        row = [self.entries[0][0], "30000000-0000-4000-8000-000000000001", 3,
               "40000000-0000-4000-8000-000000000001", self.entries[0][3]]
        responses = [b"1\t1\n", ("\t".join(map(str, row)) + "\n").encode(),
                     ("\t".join(self.entries[0]) + "\n").encode(), b"1\t1\n", b""]
        with patch.object(self.runtime, "execute", side_effect=responses):
            result = publish(self.runtime, ["mysql-fixture"], {})
        payload = json.loads((self.runtime.backups / result["filename"]).read_text())["payload"]
        self.assertEqual(payload["collections"], [row])

    def test_pin_and_authentication_are_both_required(self):
        digest = self.write()
        with self.assertRaises(OperationError): verify(self.runtime, self.path, "00" * 32, 0)
        with self.assertRaises(OperationError): verify(self.runtime, self.path, None, 0)
        envelope = json.loads(self.path.read_text())
        envelope["payload"]["entries"] = []
        self.path.write_text(json.dumps(envelope))
        changed = hashlib.sha256(self.path.read_bytes()).hexdigest()
        with self.assertRaises(OperationError): verify(self.runtime, self.path, digest, 0)
        with self.assertRaisesRegex(OperationError, "authentication"): verify(self.runtime, self.path, changed, 0)

    def test_scope_database_and_snapshot_order_reject_even_signed_input(self):
        original = json.loads(json.dumps(self.payload))
        for field, value in (("scope", "other"), ("database", "other"), ("exported_at", int(time.time()) + 3600)):
            self.payload = dict(original, **{field: value})
            with self.assertRaises(OperationError): verify(self.runtime, self.path, self.write(), 0)
        self.payload = original
        with self.assertRaises(OperationError): verify(self.runtime, self.path, self.write(), original["exported_at"] + 1)

    def test_ambiguous_rows_cannot_reach_sql(self):
        for position, value in ((0, "' OR 1=1 --"), (1, "00000000-0000-0000-0000-000000000000"), (2, "abc"), (3, "2026-02-30T00:00:00.000000Z")):
            rows = [self.entries[0].copy()]
            rows[0][position] = value
            with self.assertRaises(OperationError): validate(rows)
        with self.assertRaises(OperationError): validate(self.entries * 2)
        with self.assertRaises(OperationError): validate(self.entries * (MAX_RECORDS + 1))
        with self.assertRaises(OperationError): validate([self.entries[0] + ["extra"]])

    def expired_checkpoint(self, nonce="1"):
        self.path = self.runtime.backups / ("albums-deletions-" + nonce * 32 + ".json")
        old = int(time.time()) - RETENTION - 1
        self.payload["exported_at"] = old
        self.entries[0][3] = datetime.datetime.fromtimestamp(old, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
        self.payload["entries"] = self.entries
        self.write()
        return self.path

    def prune_cli(self, script=None):
        arguments = ["prune", "--backend-dir", str(self.runtime.root), "--environment-file", str(self.runtime.environment_file), "--backup-dir", str(self.runtime.backups)]
        entry = [sys.executable, "-c", script] if script else [sys.executable, str(Path(__file__).with_name("backup.py"))]
        env = dict(os.environ, PYTHONPATH=str(Path(__file__).parent.resolve()) + os.pathsep + str(Path(__file__).parents[1] / "tencent"))
        return subprocess.run(entry + arguments, env=env, capture_output=True, text=True, timeout=10)

    def test_prune_removes_only_authenticated_expired_loose_checkpoints(self):
        old = self.expired_checkpoint()
        unrelated = self.runtime.backups / "manual-review.json"
        unrelated.write_text("preserved deliverable")
        # Filesystem age is not the authenticated retention clock.
        os.utime(old, None)
        self.path = self.runtime.backups / ("albums-deletions-" + "2" * 32 + ".json")
        self.payload["exported_at"] = int(time.time()); digest = self.write()
        os.utime(self.path, (1, 1))
        with self.runtime.lock():
            self.assertEqual(prune(self.runtime), 1)
            self.assertEqual(prune(self.runtime), 0)
        self.assertFalse(old.exists())
        self.assertEqual(unrelated.read_text(), "preserved deliverable")
        self.assertEqual(verify(self.runtime, self.path, digest, 0), self.entries)

    def test_unrecognized_checkpoint_filename_is_preserved_for_review(self):
        old = self.expired_checkpoint()
        unknown = old.with_name("albums-deletions-manual-review.json")
        old.rename(unknown)
        with self.assertRaisesRegex(OperationError, "unrecognized"):
            prune(self.runtime)
        self.assertTrue(unknown.exists())

    def test_account_only_checkpoint_is_preserved_but_cannot_authorize_recovery(self):
        self.expired_checkpoint()
        self.payload["scope"] = "albums-deletion-ledger-v1"
        del self.payload["assets"]; del self.payload["collections"]
        digest = self.write()
        original = self.path.read_bytes()
        with self.runtime.lock():
            self.assertEqual(prune(self.runtime), 0)
            self.assertEqual(prune(self.runtime), 0)
        self.assertEqual(self.path.read_bytes(), original)
        with self.assertRaises(OperationError):
            verify(self.runtime, self.path, digest, 0)
        for change in ({"database": "other"}, {"entries": [["invalid"]]},
                       {"assets": []}, {"exported_at": True}):
            with self.subTest(change=change):
                original_payload = self.payload.copy()
                self.payload.update(change)
                self.write()
                with self.assertRaises(OperationError):
                    prune(self.runtime)
                self.assertTrue(self.path.exists())
                self.payload = original_payload
        self.write()
        envelope = json.loads(self.path.read_text())
        envelope["payload"]["entries"] = []
        self.path.write_text(json.dumps(envelope))
        with self.assertRaisesRegex(OperationError, "authentication"):
            prune(self.runtime)
        self.assertTrue(self.path.exists())

    def test_recent_request_inside_an_old_checkpoint_is_preserved(self):
        self.expired_checkpoint()
        self.entries[0][3] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
        self.write()
        self.assertEqual(prune(self.runtime), 0)
        self.assertTrue(self.path.exists())

    def test_prune_preserves_invalid_or_foreign_checkpoint_contents(self):
        for field, value in [("scope", "foreign"), ("database", "other"), ("exported_at", 0), ("exported_at", True), ("exported_at", int(time.time()) + 3600), ("entries", [["invalid"]]), ("unexpected", "field")]:
            self.expired_checkpoint(); original = json.loads(json.dumps(self.payload)); self.payload[field] = value
            self.write()
            with self.assertRaises(OperationError): prune(self.runtime)
            self.assertTrue(self.path.exists()); self.path.unlink(); self.payload = original
        self.expired_checkpoint()
        envelope = json.loads(self.path.read_text()); envelope["payload"]["entries"] = []
        self.path.write_text(json.dumps(envelope))
        with self.assertRaises(OperationError): prune(self.runtime)
        self.assertTrue(self.path.exists())

    def test_prune_preserves_unsafe_file_types_links_and_modes(self):
        for kind in ["symlink", "hardlink", "directory", "fifo", "permissions", "oversize"]:
            self.expired_checkpoint(); target = self.path.with_name("preserved-target")
            if kind == "permissions": self.path.chmod(0o644)
            elif kind == "oversize":
                with self.path.open("wb") as file: file.truncate(64 * 1024 * 1024 + 1)
            else:
                self.path.rename(target)
                if kind == "symlink": self.path.symlink_to(target)
                elif kind == "hardlink": os.link(target, self.path)
                elif kind == "directory": self.path.mkdir()
                else: os.mkfifo(self.path, 0o600)
            with self.assertRaises(OperationError): prune(self.runtime)
            self.assertTrue(self.path.exists() or self.path.is_symlink())
            if kind == "directory": self.path.rmdir()
            else: self.path.unlink()
            if target.exists(): target.unlink()

    def test_prune_rejects_replacement_after_authentication(self):
        import deletion_ledger
        self.expired_checkpoint(); read = deletion_ledger._read_checkpoint
        def replace(*args, **kwargs):
            result = read(*args, **kwargs)
            self.path.unlink(); self.payload["exported_at"] = int(time.time()); self.write()
            return result
        with patch.object(deletion_ledger, "_read_checkpoint", side_effect=replace):
            with self.assertRaises(OperationError): prune(self.runtime)
        self.assertTrue(self.path.exists())

    def test_prune_cli_respects_the_producer_operations_lock(self):
        self.expired_checkpoint()
        with self.runtime.lock():
            blocked = self.prune_cli()
            self.assertEqual(blocked.returncode, 1, blocked.stdout)
            self.assertIn("another operations operation", blocked.stdout)
            self.assertTrue(self.path.exists())
        completed = self.prune_cli()
        self.assertEqual(completed.returncode, 0, completed.stdout)
        self.assertEqual(json.loads(completed.stdout)["removed"], 1)
        self.assertFalse(self.path.exists())

    def test_process_exit_after_unlink_resumes_without_a_second_deletion_authority(self):
        first = self.expired_checkpoint("1"); second = self.expired_checkpoint("2")
        script = """
import os
from pathlib import Path
import backup
original=Path.unlink
def interrupted(path,*args,**kwargs):
    original(path,*args,**kwargs)
    if path.name=='albums-deletions-'+('1'*32)+'.json':os._exit(73)
Path.unlink=interrupted
raise SystemExit(backup.main())
"""
        interrupted = self.prune_cli(script)
        self.assertEqual(interrupted.returncode, 73, interrupted.stdout + interrupted.stderr)
        self.assertFalse(first.exists()); self.assertTrue(second.exists())
        completed = self.prune_cli()
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertEqual(json.loads(completed.stdout)["removed"], 1)
        self.assertFalse(second.exists())

    def test_private_file_and_no_symlinks(self):
        digest = self.write()
        self.path.chmod(0o644)
        with self.assertRaises(OperationError): verify(self.runtime, self.path, digest, 0)
        self.path.chmod(0o600)
        link = self.path.with_name("link.json")
        link.symlink_to(self.path)
        with self.assertRaises(OperationError): verify(self.runtime, link, digest, 0)


if __name__ == "__main__":
    unittest.main()
