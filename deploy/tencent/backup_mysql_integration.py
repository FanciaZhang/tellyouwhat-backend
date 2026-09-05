"""Verify transient-data exclusion and restoration against an isolated CI database."""

import gzip
import os
from pathlib import Path
import secrets
import tempfile
import unittest

from ops_backup import create_backup, crypt, restore_drill, verify_backup
from ops_common import Runtime


class BackupMySQLIntegrationTests(unittest.TestCase):
    def test_backup_restores_schema_and_durable_rows_without_temporary_ai_content(self):
        host = os.environ["BACKUP_TEST_MYSQL_HOST"]
        self.assertIn(host, ("127.0.0.1", "localhost"))
        database = "tellyouwhat_backup_test_" + secrets.token_hex(6)
        with tempfile.TemporaryDirectory(prefix="tellyouwhat-backup-integration-") as directory:
            root = Path(directory)
            backend = root / "backend"
            backend.mkdir(mode=0o700)
            config = {
                "MYSQL_HOST": host,
                "MYSQL_PORT": os.environ["BACKUP_TEST_MYSQL_PORT"],
                "MYSQL_USER": os.environ["BACKUP_TEST_MYSQL_USER"],
                "MYSQL_PASSWORD": os.environ["BACKUP_TEST_MYSQL_PASSWORD"],
                "MYSQL_DATABASE": database,
                "BACKUP_ENCRYPTION_KEY": secrets.token_urlsafe(48),
            }
            environment = backend / ".env.production"
            environment.write_text("".join(key + "=" + value + "\n" for key, value in config.items()))
            environment.chmod(0o600)
            runtime = Runtime(backend, root / "backups")
            sql = ["docker", "run", "--rm", "--interactive", "--network", "host", "--env", "MYSQL_PWD",
                   "mysql:8.4", "mysql", "--host=" + host, "--port=" + config["MYSQL_PORT"],
                   "--user=" + config["MYSQL_USER"], "--batch", "--skip-column-names"]
            credentials = {"MYSQL_PWD": config["MYSQL_PASSWORD"]}
            runtime.execute("fixture-create", sql, env=credentials,
                            input=("CREATE DATABASE `" + database + "`;").encode())
            try:
                fixture = b"""
                    CREATE TABLE schema_migrations (version INT PRIMARY KEY);
                    INSERT INTO schema_migrations VALUES (1);
                    CREATE TABLE private_records (id INT PRIMARY KEY, payload TEXT);
                    INSERT INTO private_records VALUES (7, 'durable fixture record');
                    CREATE TABLE ai_jobs (id INT PRIMARY KEY, owner_id INT,
                        request_ciphertext LONGBLOB, result_ciphertext LONGBLOB,
                        FOREIGN KEY (owner_id) REFERENCES private_records(id));
                    INSERT INTO ai_jobs VALUES (11, 7, 'temporary AI request fixture', 'temporary AI result fixture');
                    CREATE TABLE job_dispatch_outbox (job_id INT PRIMARY KEY,
                        FOREIGN KEY (job_id) REFERENCES ai_jobs(id));
                    INSERT INTO job_dispatch_outbox VALUES (11);
                """
                runtime.execute("fixture-seed", sql + ["--database=" + database], env=credentials, input=fixture)
                backup = create_backup(runtime)
                path = runtime.backups / backup["filename"]
                manifest = verify_backup(runtime, path)
                self.assertEqual(manifest["table_rows"], {
                    "schema_migrations": 1, "private_records": 1, "ai_jobs": 0, "job_dispatch_outbox": 0,
                })
                self.assertEqual(manifest["excluded_data_tables"], ["ai_jobs", "job_dispatch_outbox"])
                decrypted = root / "decrypted.gz"
                crypt(runtime, path, decrypted, decrypt=True)
                dump = gzip.decompress(decrypted.read_bytes())
                for payload in (b"temporary AI request fixture", b"temporary AI result fixture"):
                    self.assertNotIn(payload, dump)
                    self.assertNotIn(payload.hex().encode().upper(), dump.upper())
                self.assertIn(b"durable fixture record", dump)
                restore = restore_drill(runtime)
                self.assertEqual(restore["sha256"], backup["sha256"])
                self.assertEqual(restore["tables"], 4)
                remaining = runtime.execute("fixture-source-count", sql + ["--database=" + database],
                                            env=credentials, input=b"SELECT COUNT(*) FROM ai_jobs;")
                self.assertEqual(remaining.strip(), b"1")
            finally:
                runtime.execute("fixture-cleanup", sql, env=credentials,
                                input=("DROP DATABASE IF EXISTS `" + database + "`;").encode())


if __name__ == "__main__":
    unittest.main(verbosity=2)
