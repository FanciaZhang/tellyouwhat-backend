import gzip
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from ops_backup import create_backup, crypt, restore_drill, verify_backup
from ops_common import OperationError, Runtime, read_environment


class BackupSafetyTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.backend = self.root / "backend"
        self.backend.mkdir()
        (self.backend / ".env.production").write_text(
            "MYSQL_HOST=database.internal\nMYSQL_USER=application\nMYSQL_PASSWORD=secret\n"
            "MYSQL_DATABASE=tellyouwhat_test\nBACKUP_ENCRYPTION_KEY=" + "x" * 64 + "\n"
        )
        self.bin = self.root / "bin"
        self.bin.mkdir()
        docker = self.bin / "docker"
        docker.write_text(
            "#!/bin/sh\n"
            "printf '%s\\n' 'CREATE TABLE `schema_migrations` (' '  `version` int' ');' "
            "\"INSERT INTO \\`schema_migrations\\` VALUES (1);\" "
            "'CREATE TABLE `private_records` (' '  `payload` text' ');' "
            "\"INSERT INTO \\`private_records\\` VALUES ('synthetic private payload');\"\n"
            "exit \"${DUMP_EXIT_CODE:-0}\"\n"
        )
        docker.chmod(0o755)
        environment = patch.dict(os.environ, {"PATH": str(self.bin) + os.pathsep + os.environ["PATH"]})
        environment.start()
        self.addCleanup(environment.stop)
        self.runtime = Runtime(self.backend, self.root / "backups")

    def test_failed_export_never_publishes_a_backup(self):
        with patch.dict(os.environ, {"DUMP_EXIT_CODE": "7"}):
            with self.assertRaisesRegex(OperationError, "export failed"):
                create_backup(self.runtime)
        self.assertEqual(list(self.runtime.backups.iterdir()), [])
        self.assertFalse((self.runtime.state / "backup.json").exists())

    def test_encrypted_backup_round_trip_and_table_counts(self):
        result = create_backup(self.runtime)
        path = self.runtime.backups / result["filename"]
        self.assertNotIn(b"synthetic private payload", path.read_bytes())
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        manifest = verify_backup(self.runtime, path)
        self.assertEqual(manifest["table_rows"], {"schema_migrations": 1, "private_records": 1})
        decrypted = self.root / "decrypted.gz"
        crypt(self.runtime, path, decrypted, decrypt=True)
        self.assertIn(b"synthetic private payload", gzip.decompress(decrypted.read_bytes()))

    def test_corrupt_archive_is_rejected_before_restore(self):
        result = create_backup(self.runtime)
        path = self.runtime.backups / result["filename"]
        path.write_bytes(path.read_bytes() + b"changed")
        with self.assertRaisesRegex(OperationError, "content verification"):
            restore_drill(self.runtime)
        self.assertFalse((self.runtime.state / "restore.json").exists())

    def test_modified_metadata_cannot_authorize_another_database(self):
        result = create_backup(self.runtime)
        path = self.runtime.backups / result["filename"]
        metadata = Path(str(path) + ".json")
        envelope = json.loads(metadata.read_text())
        envelope["manifest"]["database"] = "health_ai"
        metadata.write_text(json.dumps(envelope))
        with self.assertRaisesRegex(OperationError, "authentication failed"):
            verify_backup(self.runtime, path)

    def test_environment_values_are_data_not_shell_commands(self):
        target = self.root / "must-not-exist"
        source = self.root / "literal.env"
        value = "$(touch " + str(target) + ")`id`$HOME"
        source.write_text("SECRET=" + value + "\n")
        self.assertEqual(read_environment(source)["SECRET"], value)
        self.assertFalse(target.exists())

    def test_restore_rejects_paths_outside_backup_directory(self):
        with self.assertRaisesRegex(OperationError, "only a backup filename"):
            restore_drill(self.runtime, "../unrelated.sql.gz.enc")


if __name__ == "__main__":
    unittest.main()
