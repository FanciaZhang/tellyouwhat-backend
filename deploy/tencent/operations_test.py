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
            "case \" $* \" in *' --no-data '*)\n"
            "printf '%s\\n' 'CREATE TABLE `ai_jobs` (`id` int);' 'CREATE TABLE `job_dispatch_outbox` (`id` int);'\n"
            "exit \"${DUMP_SCHEMA_EXIT_CODE:-0}\" ;; esac\n"
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

    def test_failed_transient_schema_export_never_publishes_a_partial_backup(self):
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
        self.assertEqual(manifest["table_rows"], {"schema_migrations": 1, "private_records": 1, "ai_jobs": 0, "job_dispatch_outbox": 0})
        decrypted = self.root / "decrypted.gz"
        crypt(self.runtime, path, decrypted, decrypt=True)
        self.assertIn(b"synthetic private payload", gzip.decompress(decrypted.read_bytes()))

    def test_unexpected_transient_data_aborts_without_publishing(self):
        (self.bin / "docker").write_text(
            "#!/bin/sh\n"
            "printf '%s\\n' 'CREATE TABLE `schema_migrations` (`version` int);' "
            "'INSERT INTO `ai_jobs` VALUES (0x73656e736974697665);'\n"
        )
        with self.assertRaisesRegex(OperationError, "excluded transient data"):
            create_backup(self.runtime)
        self.assertEqual(list(self.runtime.backups.iterdir()), [])
        self.assertFalse((self.runtime.state / "backup.json").exists())

    def test_transient_ai_payloads_are_not_in_encrypted_backup(self):
        docker = self.bin / "docker"
        docker.write_text(
            "#!/usr/bin/env python3\n"
            "import sys\n"
            "tables = ['schema_migrations', 'private_records', 'ai_jobs', 'job_dispatch_outbox']\n"
            "if '--no-data' in sys.argv: tables = ['ai_jobs', 'job_dispatch_outbox']\n"
            "for table in tables:\n"
            "    if '--ignore-table=tellyouwhat_test.' + table in sys.argv: continue\n"
            "    print('CREATE TABLE `' + table + '` (\\n  `payload` blob\\n);')\n"
            "    if '--no-data' not in sys.argv:\n"
            "        payload = 'temporary AI private payload' if table == 'ai_jobs' else 'durable metadata'\n"
            "        print(\"INSERT INTO `\" + table + \"` VALUES ('\" + payload + \"');\")\n"
        )
        result = create_backup(self.runtime)
        path = self.runtime.backups / result["filename"]
        decrypted = self.root / "decrypted.gz"
        crypt(self.runtime, path, decrypted, decrypt=True)
        sql = gzip.decompress(decrypted.read_bytes())
        self.assertNotIn(b"temporary AI private payload", sql)
        self.assertNotIn(b"INSERT INTO `ai_jobs`", sql)
        self.assertNotIn(b"INSERT INTO `job_dispatch_outbox`", sql)
        self.assertIn(b"CREATE TABLE `ai_jobs`", sql)
        self.assertIn(b"CREATE TABLE `job_dispatch_outbox`", sql)
        self.assertIn(b"durable metadata", sql)
        manifest = verify_backup(self.runtime, path)
        self.assertEqual(manifest["table_rows"]["ai_jobs"], 0)
        self.assertEqual(manifest["table_rows"]["job_dispatch_outbox"], 0)
        self.assertEqual(manifest["excluded_data_tables"], ["ai_jobs", "job_dispatch_outbox"])

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
