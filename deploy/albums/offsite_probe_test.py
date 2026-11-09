"""Probe trust-boundary tests. These do not prove any real COS capability."""
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

import backup
import offsite_probe as probe
from ops_common import atomic_json, OperationError


class OffsiteProbeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="albums-probe-boundary-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.writer = self.root / "writer.json"
        self.cleanup = self.root / "cleanup.json"
        self.binary = self.root / "albumbackup"
        self.binary.write_text("#!/bin/sh\nexit 1\n")
        self.binary.chmod(0o700)
        for p in (self.writer, self.cleanup):
            atomic_json(p, dict(Bucket="tellyouwhat-albums-backups-test-1250000000", Region="ap-guangzhou",
                                SecretID="fixture-id", SecretKey="fixture-secret"))
        atomic_json(self.root / "probe.json", dict(scope=probe.SCOPE, writer=str(self.writer),
                                                  cleanup=str(self.cleanup), transfer=str(self.binary)))

    def test_configuration_binds_separate_credentials_to_one_test_bucket(self):
        c, target = probe.configuration(self.root)
        self.assertEqual(c["writer"], str(self.writer))
        self.assertEqual(target["Bucket"], "tellyouwhat-albums-backups-test-1250000000")
        original = json.loads(self.cleanup.read_text())
        original["Bucket"] = "tellyouwhat-albums-backups-production-1250000000"
        atomic_json(self.cleanup, original)
        with self.assertRaises(OperationError): probe.configuration(self.root)

    def test_ordinary_bucket_and_unsafe_executable_are_rejected(self):
        for p in (self.writer, self.cleanup):
            data = json.loads(p.read_text())
            data["Bucket"] = "shared-backup-1250000000"
            atomic_json(p, data)
        with self.assertRaises(OperationError): probe.configuration(self.root)
        for p in (self.writer, self.cleanup):
            data["Bucket"] = "tellyouwhat-albums-backups-test-1250000000"
            atomic_json(p, data)
        self.binary.chmod(0o722)
        with self.assertRaises(OperationError): probe.configuration(self.root)

    def test_non_disposable_source_cannot_start_cloud_work(self):
        env = self.root / "env"
        env.write_text("MYSQL_DATABASE=albums_test\n")
        env.chmod(0o600)
        source = backup.Runtime(self.root, self.root / "unrelated", env)
        source.config = dict(MYSQL_DATABASE="albums_test", MYSQL_SOCKET="/tmp/production/mysql.sock")
        with patch("offsite_probe.configuration") as config, patch("offsite_probe.offsite.publish") as publish:
            with self.assertRaises(OperationError): probe.run(source, Path("/usr/bin"), "snapshot", [], self.root)
            config.assert_not_called()
            publish.assert_not_called()

    def test_existing_probe_is_not_republished_after_an_unknown_result(self):
        # Match the disposable source boundary without connecting to any database.
        from types import SimpleNamespace
        source = SimpleNamespace(root=Path("/tmp/album-snapshot-synthetic").resolve(),
                                 config=dict(MYSQL_DATABASE="albums_test", MYSQL_SOCKET="/tmp/album-snapshot-synthetic/mysql.sock"))
        (self.root / "intent.json").write_text("unknown result")
        with patch("offsite_probe.offsite.publish") as publish, patch("offsite_probe.backup.verify") as verify:
            with self.assertRaisesRegex(OperationError, "already started"):
                sql = ["/usr/bin/mysql", "--no-defaults", "--protocol=socket", "--socket=" + source.config["MYSQL_SOCKET"],
                       "--user=root", "--database=albums_test"]
                probe.run(source, Path("/usr/bin"), "snapshot", sql, self.root)
            publish.assert_not_called()
            verify.assert_not_called()

    def test_tcp_database_command_is_rejected_before_credentials_or_cloud(self):
        from types import SimpleNamespace
        source = SimpleNamespace(root=Path("/tmp/album-snapshot-synthetic").resolve(),
                                 config=dict(MYSQL_DATABASE="albums_test", MYSQL_SOCKET="/tmp/album-snapshot-synthetic/mysql.sock"))
        with patch("offsite_probe.configuration") as config:
            with self.assertRaisesRegex(OperationError, "socket-only"):
                probe.run(source, Path("/usr/bin"), "snapshot", ["mysql", "--host=production"], self.root)
            config.assert_not_called()

    def disposable_source(self):
        source_temp = tempfile.TemporaryDirectory(prefix="album-snapshot-", dir="/tmp")
        self.addCleanup(source_temp.cleanup)
        folder = Path(source_temp.name)
        env = folder / "source.env"
        env.write_text("MYSQL_DATABASE=albums_test\nMYSQL_SOCKET=" + str(folder / "mysql.sock") +
                       "\nALBUM_BACKUP_ENCRYPTION_KEY=" + "a" * 64 + "\n")
        env.chmod(0o600)
        source = backup.Runtime(folder, folder / "vault", env)
        backup.configure(source)
        filename = "albums-" + "a" * 32 + ".sql.gz.enc"
        ledger = "albums-deletions-" + "b" * 32 + ".json"
        for name in (filename, filename + ".json", ledger):
            (source.backups / name).write_bytes(b"guard-only fixture, never uploaded")
        sql = ["/usr/bin/mysql", "--no-defaults", "--protocol=socket", "--socket=" + source.config["MYSQL_SOCKET"],
               "--user=root", "--database=albums_test"]
        return source, filename, ledger, sql

    def test_restore_scratch_uses_owned_short_fixture_root_and_downloaded_inputs(self):
        source, filename, ledger, sql = self.disposable_source()
        fetched = self.root / "vault" / "albums-recovered-fixture"
        manifest = dict(created_at=int(time.time()), sha256="c" * 64, table_rows={})
        with patch("offsite_probe.backup.verify", return_value=manifest), \
                patch("offsite_probe.deletion_ledger.publish", return_value=dict(filename=ledger, sha256="d" * 64)), \
                patch("offsite_probe.offsite.publish", return_value=dict(remote={"Version": "fixture"},
                      receipt_remote={"Version": "fixture"}, receipt="fixture", receipt_sha256="e" * 64)), \
                patch("offsite_probe.offsite.check_ledger", return_value=dict(bundle_version="fixture",
                      consistent_database_snapshot=True, authorizes_erasure_or_recovery=False)), \
                patch("offsite_probe.offsite.fetch", return_value=dict(directory=str(fetched),
                      deletion_ledger=str(fetched / ledger), deletion_ledger_sha256="d" * 64)), \
                patch("offsite_probe.backup.drill", return_value=dict(quarantined=True, deletion_ledger_applied=True)) as drill, \
                patch("offsite_probe.recover", return_value=[]):
            report = probe.run(source, Path("/usr/bin"), filename, sql, self.root)
        recovered = drill.call_args.args[0]
        self.assertEqual(recovered.root, source.root)
        self.assertEqual(recovered.state, source.state)
        self.assertEqual(recovered.backups, fetched)
        self.assertEqual(recovered.environment_file, (self.root / "probe.env").resolve())
        self.assertEqual(drill.call_args.args[3:], (fetched / ledger, "d" * 64))
        self.assertTrue(report["restored"])
        self.assertTrue(report["checkpoint_matched_source"])
        self.assertTrue((self.root / "intent.json").exists())
        self.assertTrue((self.root / "vault" / filename).exists())

    def test_failure_preserves_recovery_inputs_and_does_not_claim_remote_success(self):
        source, filename, ledger, sql = self.disposable_source()
        manifest = dict(created_at=int(time.time()), sha256="c" * 64, table_rows={})
        with patch("offsite_probe.backup.verify", return_value=manifest), \
                patch("offsite_probe.deletion_ledger.publish", return_value=dict(filename=ledger, sha256="d" * 64)), \
                patch("offsite_probe.offsite.publish", side_effect=OperationError("controlled failure")), \
                patch("offsite_probe.recover", return_value=[]) as recover:
            with self.assertRaises(OperationError): probe.run(source, Path("/usr/bin"), filename, sql, self.root)
            recover.assert_called_once_with(self.root)
        report = json.loads((self.root / "report.json").read_text())
        self.assertFalse(report["published"])
        self.assertFalse(report["restored"])
        self.assertFalse(report["cleanup_confirmed"])
        self.assertTrue((self.root / "probe.env").exists())
        marker = json.loads((self.root / "intent.json").read_text())
        marker["intent"]["snapshot"] = "unrelated"
        atomic_json(self.root / "intent.json", marker)
        with patch("offsite_probe.offsite.expire") as expire:
            with self.assertRaises(OperationError): probe.recover(self.root)
            expire.assert_not_called()
