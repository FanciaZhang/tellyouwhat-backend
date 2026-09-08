"""Verify privacy-safe disaster recovery against an isolated CI database."""

import gzip
import os
from pathlib import Path
import secrets
import tempfile
import unittest

from ops_backup import create_backup, crypt, restore_drill, verify_backup
from ops_common import Runtime


class BackupMySQLIntegrationTests(unittest.TestCase):
    def test_backup_restores_control_plane_without_application_user_rows(self):
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
                    CREATE TABLE apps (app_id VARCHAR(64) PRIMARY KEY, display_name VARCHAR(128));
                    INSERT INTO apps VALUES ('health', 'Health fixture');
                    CREATE TABLE privacy_deletion_receipts (
                        app_id VARCHAR(64) NOT NULL,
                        request_digest BINARY(32) NOT NULL,
                        PRIMARY KEY (app_id, request_digest),
                        FOREIGN KEY (app_id) REFERENCES apps(app_id));
                    INSERT INTO privacy_deletion_receipts VALUES ('health', UNHEX(REPEAT('ab', 32)));
                    CREATE TABLE ai_cost_control_state (singleton_id TINYINT PRIMARY KEY);
                    INSERT INTO ai_cost_control_state VALUES (1);
                    CREATE TABLE ai_cost_months (
                        month_start DATE PRIMARY KEY, budget_nanos BIGINT NOT NULL,
                        charged_nanos BIGINT NOT NULL, updated_at DATETIME(6) NOT NULL);
                    INSERT INTO ai_cost_months VALUES ('2026-09-01', 100000000000, 25000000, NOW(6));
                    CREATE TABLE ai_cost_attempts (
                        id CHAR(36) PRIMARY KEY, month_start DATE NOT NULL, app_id VARCHAR(64) NOT NULL,
                        operation VARCHAR(128) NOT NULL, meter VARCHAR(32) NOT NULL,
                        reserved_nanos BIGINT NOT NULL, actual_nanos BIGINT,
                        status VARCHAR(16) NOT NULL, created_at DATETIME(6) NOT NULL,
                        lease_expires_at DATETIME(6) NOT NULL, completed_at DATETIME(6),
                        FOREIGN KEY (month_start) REFERENCES ai_cost_months(month_start));
                    INSERT INTO ai_cost_attempts VALUES (
                        '20000000-0000-4000-8000-000000000001', '2026-09-01', 'health',
                        'meal_text_capture', 'ark', 30000000, 25000000, 'settled',
                        NOW(6), NOW(6), NOW(6));
                    CREATE TABLE app_attest_keys (
                        app_id VARCHAR(64) NOT NULL, key_id VARCHAR(64) NOT NULL,
                        receipt LONGBLOB NOT NULL, PRIMARY KEY (app_id, key_id),
                        FOREIGN KEY (app_id) REFERENCES apps(app_id));
                    INSERT INTO app_attest_keys VALUES ('health', 'private-key-id', 'private attestation receipt');
                    CREATE TABLE privacy_consents (
                        app_id VARCHAR(64) NOT NULL, key_id VARCHAR(64) NOT NULL,
                        granted BOOLEAN NOT NULL, PRIMARY KEY (app_id, key_id),
                        FOREIGN KEY (app_id, key_id) REFERENCES app_attest_keys(app_id, key_id));
                    INSERT INTO privacy_consents VALUES ('health', 'private-key-id', TRUE);
                    CREATE TABLE future_user_data (id INT PRIMARY KEY, payload TEXT);
                    INSERT INTO future_user_data VALUES (7, 'future private fixture record');
                    CREATE TABLE ai_jobs (id INT PRIMARY KEY, owner_id INT,
                        request_ciphertext LONGBLOB, result_ciphertext LONGBLOB,
                        FOREIGN KEY (owner_id) REFERENCES future_user_data(id));
                    INSERT INTO ai_jobs VALUES (11, 7, 'temporary AI request fixture', 'temporary AI result fixture');
                    CREATE TABLE job_dispatch_outbox (job_id INT PRIMARY KEY,
                        FOREIGN KEY (job_id) REFERENCES ai_jobs(id));
                    INSERT INTO job_dispatch_outbox VALUES (11);
                    CREATE TABLE operations_collection (singleton_id INT PRIMARY KEY, started_at DATETIME);
                    INSERT INTO operations_collection VALUES (1, '2026-09-08 00:00:00');
                    CREATE TABLE platform_ops_patrol (id INT PRIMARY KEY, document JSON);
                    INSERT INTO platform_ops_patrol VALUES (1, JSON_OBJECT("state", "durable-automation-patrol"));
                    CREATE TABLE platform_ops_circuits (id INT PRIMARY KEY, document JSON);
                    INSERT INTO platform_ops_circuits VALUES (1, JSON_OBJECT("state", "durable-automation-circuits"));
                    CREATE TABLE platform_ops_incidents (id INT PRIMARY KEY, document JSON);
                    INSERT INTO platform_ops_incidents VALUES (1, JSON_OBJECT("state", "durable-automation-incidents"));
                    CREATE TABLE platform_ops_events (id INT PRIMARY KEY, document JSON);
                    INSERT INTO platform_ops_events VALUES (1, JSON_OBJECT("state", "durable-automation-events"));
                    CREATE TABLE operations_free_cohorts (id INT PRIMARY KEY, key_id VARCHAR(64));
                    INSERT INTO operations_free_cohorts VALUES (1, 'private-cohort-key');
                    CREATE TABLE operations_purchase_observations (id INT PRIMARY KEY, evidence TEXT);
                    INSERT INTO operations_purchase_observations VALUES (1, 'private purchase metadata');
                """
                runtime.execute("fixture-seed", sql + ["--database=" + database], env=credentials, input=fixture)
                backup = create_backup(runtime)
                path = runtime.backups / backup["filename"]
                manifest = verify_backup(runtime, path)
                self.assertEqual(manifest["table_rows"], {
                    "schema_migrations": 1, "apps": 1, "privacy_deletion_receipts": 1,
                    "ai_cost_control_state": 1, "ai_cost_months": 1, "ai_cost_attempts": 1,
                    "app_attest_keys": 0, "privacy_consents": 0, "future_user_data": 0,
                    "ai_jobs": 0, "job_dispatch_outbox": 0,
                    "platform_ops_patrol": 1, "platform_ops_circuits": 1, "platform_ops_incidents": 1, "platform_ops_events": 1,
                    "operations_collection": 1, "operations_free_cohorts": 0, "operations_purchase_observations": 0,
                })
                self.assertEqual(manifest["included_data_tables"], [
                    "schema_migrations", "apps", "privacy_deletion_receipts",
                    "ai_cost_control_state", "ai_cost_months", "ai_cost_attempts", "operations_collection",
                    "platform_ops_patrol", "platform_ops_circuits", "platform_ops_incidents", "platform_ops_events",
                ])
                self.assertEqual(manifest["excluded_data_tables"], [
                    "ai_jobs", "app_attest_keys", "future_user_data", "job_dispatch_outbox", "operations_free_cohorts", "operations_purchase_observations", "privacy_consents",
                ])
                decrypted = root / "decrypted.gz"
                crypt(runtime, path, decrypted, decrypt=True)
                dump = gzip.decompress(decrypted.read_bytes())
                for name in ("patrol", "circuits", "incidents", "events"):
                    self.assertIn(("durable-automation-" + name).encode(), dump)
                for payload in (
                    b"temporary AI request fixture", b"temporary AI result fixture",
                    b"private attestation receipt", b"future private fixture record", b"private-cohort-key", b"private purchase metadata",
                ):
                    self.assertNotIn(payload, dump)
                    self.assertNotIn(payload.hex().encode().upper(), dump.upper())
                self.assertIn(b"Health fixture", dump)
                self.assertIn(b"ABABABAB", dump.upper())
                restore = restore_drill(runtime)
                self.assertEqual(restore["sha256"], backup["sha256"])
                self.assertEqual(restore["tables"], 18)
                remaining = runtime.execute("fixture-source-count", sql + ["--database=" + database],
                                            env=credentials, input=b"SELECT COUNT(*) FROM ai_jobs;")
                self.assertEqual(remaining.strip(), b"1")
            finally:
                runtime.execute("fixture-cleanup", sql, env=credentials,
                                input=("DROP DATABASE IF EXISTS `" + database + "`;").encode())


if __name__ == "__main__":
    unittest.main(verbosity=2)
