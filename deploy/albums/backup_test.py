import hashlib
import json
import os
from pathlib import Path
import re
import tempfile
import time
import unittest
from unittest.mock import patch

from backup import (TABLES, SCOPE, RETENTION, configure, drill, prune, verify,
                    Runtime, OperationError, manifest_signature, backup_secret)
from ops_backup import verify_backup as verify_control_backup


class AlbumsBackupSafetyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="albums-backup-unit-")
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        env = root / "snapshot.env"
        env.write_text("MYSQL_DATABASE=albums_test\nALBUM_BACKUP_ENCRYPTION_KEY=" + "a" * 64 + "\n")
        env.chmod(0o600)
        self.runtime = Runtime(root, root / "vault", env)
        configure(self.runtime)
        self.path = self.runtime.backups / ("albums-" + "1" * 32 + ".sql.gz.enc")
        self.path.write_bytes(b"authenticated opaque fixture")
        self.path.chmod(0o600)
        self.manifest = {"scope": SCOPE, "created_at": int(time.time()), "database": "albums_test", "filename": self.path.name,
                         "table_rows": {t: 0 for t in TABLES}, "table_sha256": {t: hashlib.sha256(b"").hexdigest() for t in TABLES},
                         "sha256": hashlib.sha256(self.path.read_bytes()).hexdigest()}
        self.sign()

    def sign(self):
        Path(str(self.path) + ".json").write_text(json.dumps({"manifest": self.manifest, "hmac_sha256": manifest_signature(backup_secret(self.runtime), self.manifest)}))

    def test_explicit_albums_environment_is_not_replaced_by_active_deployment(self):
        active = self.runtime.root / ".slots" / "health"
        active.mkdir(parents=True)
        (active / ".env.production").write_text("MYSQL_DATABASE=health_wrong_source\n")
        (self.runtime.state / "blue-green.json").write_text(json.dumps({"current": {"path": ".slots/health"}}))
        self.runtime.refresh()
        self.assertEqual(configure(self.runtime), "albums_test")

    def test_example_key_is_rejected(self):
        self.runtime.config["ALBUM_BACKUP_ENCRYPTION_KEY"] = "replace-with-at-least-256-bits-of-random-key-material"
        with self.assertRaisesRegex(OperationError, "placeholder"):
            configure(self.runtime)

    def test_schema_coverage_is_complete_and_explicit(self):
        migrations = Path(__file__).resolve().parents[2] / "migrations"
        names = set()
        for file in migrations.glob("*album*.sql"):
            names.update(re.findall(r"CREATE TABLE (?:IF NOT EXISTS )?(album_\w+)", file.read_text()))
        self.assertEqual(set(TABLES), names)
        self.assertEqual(len(TABLES), len(set(TABLES)))
        self.assertTrue(all(t.startswith("album_") for t in TABLES))

    def test_valid_scope_cannot_enter_control_plane_restore(self):
        self.assertEqual(verify(self.runtime, self.path), self.manifest)
        with self.assertRaisesRegex(OperationError, "scope"):
            verify_control_backup(self.runtime, self.path)

    def test_ciphertext_and_manifest_tampering_rejected(self):
        self.path.write_bytes(b"changed")
        with self.assertRaises(OperationError): verify(self.runtime, self.path)
        self.path.write_bytes(b"authenticated opaque fixture")
        self.manifest["created_at"] -= 86400
        envelope = json.loads(Path(str(self.path) + ".json").read_text())
        envelope["manifest"] = self.manifest
        Path(str(self.path) + ".json").write_text(json.dumps(envelope))
        with self.assertRaisesRegex(OperationError, "authentication"): verify(self.runtime, self.path)

    def test_wrong_key_database_scope_and_partial_tables_rejected(self):
        self.runtime.config["ALBUM_BACKUP_ENCRYPTION_KEY"] = "b" * 64
        with self.assertRaises(OperationError): verify(self.runtime, self.path)
        self.runtime.config["ALBUM_BACKUP_ENCRYPTION_KEY"] = "a" * 64
        configure(self.runtime)
        original = json.loads(json.dumps(self.manifest))
        for mutation in (lambda m: m.update(database="other"), lambda m: m.update(scope="control-plane"),
                         lambda m: m["table_rows"].pop("album_uploads"), lambda m: m.update(created_at=int(time.time()) + 3600)):
            self.manifest = json.loads(json.dumps(original))
            mutation(self.manifest)
            self.sign()
            with self.assertRaises(OperationError): verify(self.runtime, self.path)

    def test_expiry_checked_before_decrypt_or_starting_mysql(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        with patch("backup.crypt") as crypto, patch("backup.subprocess.Popen") as server:
            with self.assertRaisesRegex(OperationError, "retention"): drill(self.runtime, Path("/unused"), self.path.name)
        crypto.assert_not_called()
        server.assert_not_called()

    def test_pruning_uses_authenticated_age_and_keeps_unknown_files(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        os.utime(self.path, None)
        other = self.runtime.backups / "user-deliverable.enc"
        other.write_bytes(b"keep")
        self.assertEqual(prune(self.runtime), 1)
        self.assertTrue(other.exists())
        self.assertFalse(self.path.exists())

    def test_corruption_is_preserved_for_review_not_pruned(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        self.path.write_bytes(b"corrupt")
        with self.assertRaises(OperationError): prune(self.runtime)
        self.assertTrue(self.path.exists())

    def test_symlink_and_traversal_and_nonprivate_directory_rejected(self):
        original = self.path.with_name("original.enc")
        self.path.rename(original)
        self.path.symlink_to(original)
        with self.assertRaises(OperationError): verify(self.runtime, self.path)
        with self.assertRaises(OperationError): verify(self.runtime, self.runtime.backups / "../outside.enc")
        self.runtime.backups.chmod(0o755)
        with self.assertRaisesRegex(OperationError, "private"): configure(self.runtime)


if __name__ == "__main__":
    unittest.main()
