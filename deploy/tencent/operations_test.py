import gzip
import json
import os
from pathlib import Path
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from ops_backup import create_backup, crypt, prune_backups, restore_drill, verify_backup
from ops_common import OperationError, Runtime, read_environment
from operations import operate


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
            "#!/usr/bin/env python3\n"
            "import os, sys\n"
            "tables = ['schema_migrations', 'apps', 'privacy_deletion_receipts', 'private_records', 'ai_jobs', 'job_dispatch_outbox']\n"
            "if 'mysql' in sys.argv and 'mysqldump' not in sys.argv:\n"
            "    print('\\n'.join(tables)); raise SystemExit(0)\n"
            "if '--no-data' in sys.argv:\n"
            "    for table in tables: print('CREATE TABLE `' + table + '` (`payload` text);')\n"
            "    raise SystemExit(int(os.getenv('DUMP_SCHEMA_EXIT_CODE', '0')))\n"
            "for table, payload in [('schema_migrations', '1'), ('apps', 'health'), "
            "('privacy_deletion_receipts', 'opaque deletion digest')]:\n"
            "    if table in sys.argv: print(\"INSERT INTO `\" + table + \"` VALUES ('\" + payload + \"');\")\n"
            "raise SystemExit(int(os.getenv('DUMP_EXIT_CODE', '0')))\n"
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

    def test_backup_age_uses_authenticated_creation_time_after_file_copy(self):
        now = int(time.time())
        with patch("ops_backup.time.time", return_value=now - 15 * 86400):
            result = create_backup(self.runtime)
        path = self.runtime.backups / result["filename"]
        os.utime(path, (now, now))
        create_backup(self.runtime)
        self.assertFalse(path.exists())
        self.assertFalse(Path(str(path) + ".json").exists())

    def test_expired_backup_is_pruned_even_when_new_database_export_fails(self):
        now = int(time.time())
        with patch("ops_backup.time.time", return_value=now - 15 * 86400):
            result = create_backup(self.runtime)
        path = self.runtime.backups / result["filename"]
        with patch.dict(os.environ, {"DUMP_EXIT_CODE": "7"}):
            with self.assertRaisesRegex(OperationError, "export failed"):
                create_backup(self.runtime)
        self.assertFalse(path.exists())

    def test_expired_snapshot_is_rejected_before_decryption_or_restore(self):
        now = int(time.time())
        with patch("ops_backup.time.time", return_value=now - 14 * 86400):
            result = create_backup(self.runtime)
        with patch("ops_backup.time.time", return_value=now), patch("ops_backup.crypt") as decrypt:
            with self.assertRaisesRegex(OperationError, "retention"):
                restore_drill(self.runtime, result["filename"])
        decrypt.assert_not_called()

    def test_maintenance_prunes_backups_without_successful_new_export(self):
        now = int(time.time())
        with patch("ops_backup.time.time", return_value=now - 15 * 86400):
            result = create_backup(self.runtime)
        with patch.object(self.runtime, "execute") as execute:
            operate(self.runtime, SimpleNamespace(operation="maintenance"))
        self.assertFalse((self.runtime.backups / result["filename"]).exists())
        execute.assert_called_once()
        record = json.loads((self.runtime.state / "maintenance.json").read_text())
        self.assertEqual(record["expired_backups_removed"], 1)

    def test_invalid_snapshot_does_not_skip_other_expired_snapshots_or_database_cleanup(self):
        corrupt = create_backup(self.runtime)
        corrupt_path = self.runtime.backups / corrupt["filename"]
        now = int(time.time())
        with patch("ops_backup.time.time", return_value=now - 15 * 86400):
            expired = create_backup(self.runtime)
        corrupt_path.write_bytes(corrupt_path.read_bytes() + b"invalid")
        with patch.object(self.runtime, "execute") as execute:
            with self.assertRaisesRegex(OperationError, "retention verification failed"):
                operate(self.runtime, SimpleNamespace(operation="maintenance"))
        execute.assert_called_once()
        self.assertTrue(corrupt_path.exists())
        self.assertFalse((self.runtime.backups / expired["filename"]).exists())
        self.assertFalse((self.runtime.state / "maintenance.json").exists())

    def test_recent_backup_is_preserved_at_retention_boundary(self):
        now = int(time.time())
        with patch("ops_backup.time.time", return_value=now - 14 * 86400 + 1):
            result = create_backup(self.runtime)
        with patch("ops_backup.time.time", return_value=now):
            self.assertEqual(prune_backups(self.runtime), 0)
        path = self.runtime.backups / result["filename"]
        self.assertTrue(path.exists())
        with patch("ops_backup.time.time", return_value=now + 1):
            self.assertEqual(prune_backups(self.runtime), 1)
        self.assertFalse(path.exists())

    def test_failed_schema_export_never_publishes_a_partial_backup(self):
        with patch.dict(os.environ, {"DUMP_SCHEMA_EXIT_CODE": "9"}):
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
        self.assertEqual(manifest["table_rows"], {
            "schema_migrations": 1, "apps": 1, "privacy_deletion_receipts": 1,
            "private_records": 0, "ai_jobs": 0, "job_dispatch_outbox": 0,
        })
        decrypted = self.root / "decrypted.gz"
        crypt(self.runtime, path, decrypted, decrypt=True)
        sql = gzip.decompress(decrypted.read_bytes())
        self.assertIn(b"opaque deletion digest", sql)
        self.assertNotIn(b"synthetic private payload", sql)

    def test_unexpected_application_data_aborts_without_publishing(self):
        (self.bin / "docker").write_text(
            "#!/usr/bin/env python3\n"
            "import sys\n"
            "if 'mysql' in sys.argv and 'mysqldump' not in sys.argv:\n"
            "    print('schema_migrations\\napps\\nai_jobs'); raise SystemExit(0)\n"
            "if '--no-data' in sys.argv:\n"
            "    print('CREATE TABLE `schema_migrations` (`version` int);')\n"
            "    print('CREATE TABLE `apps` (`id` int);')\n"
            "    print('CREATE TABLE `ai_jobs` (`payload` blob);'); raise SystemExit(0)\n"
            "print('INSERT INTO `schema_migrations` VALUES (1);')\n"
            "print('INSERT INTO `ai_jobs` VALUES (0x73656e736974697665);')\n"
        )
        with self.assertRaisesRegex(OperationError, "excluded application data"):
            create_backup(self.runtime)
        self.assertEqual(list(self.runtime.backups.iterdir()), [])
        self.assertFalse((self.runtime.state / "backup.json").exists())

    def test_current_and_future_application_rows_are_not_in_encrypted_backup(self):
        docker = self.bin / "docker"
        docker.write_text(
            "#!/usr/bin/env python3\n"
            "import sys\n"
            "tables = ['schema_migrations', 'apps', 'privacy_deletion_receipts', 'private_records', 'ai_jobs', 'job_dispatch_outbox', 'future_user_data']\n"
            "if 'mysql' in sys.argv and 'mysqldump' not in sys.argv:\n"
            "    print('\\n'.join(tables)); raise SystemExit(0)\n"
            "if '--no-data' in sys.argv:\n"
            "    for table in tables: print('CREATE TABLE `' + table + '` (`payload` blob);')\n"
            "    raise SystemExit(0)\n"
            "for table, payload in [('schema_migrations', 'migration'), ('apps', 'app registry'), "
            "('privacy_deletion_receipts', 'opaque deletion digest')]:\n"
            "    if table in sys.argv: print(\"INSERT INTO `\" + table + \"` VALUES ('\" + payload + \"');\")\n"
        )
        result = create_backup(self.runtime)
        path = self.runtime.backups / result["filename"]
        decrypted = self.root / "decrypted.gz"
        crypt(self.runtime, path, decrypted, decrypt=True)
        sql = gzip.decompress(decrypted.read_bytes())
        self.assertNotIn(b"temporary AI private payload", sql)
        self.assertNotIn(b"INSERT INTO `ai_jobs`", sql)
        self.assertNotIn(b"INSERT INTO `job_dispatch_outbox`", sql)
        self.assertNotIn(b"INSERT INTO `private_records`", sql)
        self.assertNotIn(b"INSERT INTO `future_user_data`", sql)
        self.assertIn(b"CREATE TABLE `ai_jobs`", sql)
        self.assertIn(b"CREATE TABLE `job_dispatch_outbox`", sql)
        self.assertIn(b"CREATE TABLE `future_user_data`", sql)
        self.assertIn(b"opaque deletion digest", sql)
        manifest = verify_backup(self.runtime, path)
        self.assertEqual(manifest["table_rows"]["ai_jobs"], 0)
        self.assertEqual(manifest["table_rows"]["job_dispatch_outbox"], 0)
        self.assertEqual(manifest["table_rows"]["future_user_data"], 0)
        self.assertEqual(manifest["included_data_tables"], ["schema_migrations", "apps", "privacy_deletion_receipts"])
        self.assertEqual(manifest["excluded_data_tables"], ["ai_jobs", "future_user_data", "job_dispatch_outbox", "private_records"])

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

    def test_explicit_environment_file_reaches_child_process_without_global_changes(self):
        credential = self.root / "production-credential"
        credential.write_text((self.backend / ".env.production").read_text())
        runtime = Runtime(self.backend, self.root / "backups", os.path.relpath(credential))
        probe = self.root / "probe.py"
        probe.write_text(
            "import os, pathlib, sys\n"
            "path = pathlib.Path(os.environ['TELLYOUWHAT_ENV_FILE'])\n"
            "assert path.is_absolute() and path.samefile(sys.argv[1])\n"
            "assert 'MYSQL_DATABASE=tellyouwhat_test' in path.read_text()\n"
            "print('configuration available')\n"
        )
        with patch.dict(os.environ, {"TELLYOUWHAT_ENV_FILE": "/unrelated/environment"}):
            output = runtime.execute("credential-probe", ["python3", str(probe), str(credential)])
            self.assertEqual(os.environ["TELLYOUWHAT_ENV_FILE"], "/unrelated/environment")
        self.assertEqual(output.strip(), b"configuration available")

    def test_restore_rejects_paths_outside_backup_directory(self):
        with self.assertRaisesRegex(OperationError, "only a backup filename"):
            restore_drill(self.runtime, "../unrelated.sql.gz.enc")


if __name__ == "__main__":
    unittest.main()
