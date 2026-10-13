import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import time
import unittest
from unittest.mock import patch

from backup import Runtime, TABLES, SCOPE, RETENTION, configure, prune, OperationError
from deletion_ledger import SCOPE as LEDGER_SCOPE
from offsite import publish, fetch, read_receipt, publish_receipt
from ops_backup import file_digest, manifest_signature, backup_secret
from ops_common import atomic_json


class OffsiteTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="albums-offsite-test-")
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        env = root / "env"
        env.write_text("MYSQL_DATABASE=albums_test\nALBUM_BACKUP_ENCRYPTION_KEY=" + "a" * 64 + "\n")
        env.chmod(0o600)
        self.runtime = Runtime(root, root / "vault", env)
        configure(self.runtime)
        self.snapshot = self.runtime.backups / ("albums-" + "1" * 32 + ".sql.gz.enc")
        self.snapshot.write_bytes(b"synthetic opaque snapshot")
        self.snapshot.chmod(0o600)
        self.manifest = dict(scope=SCOPE, database="albums_test", filename=self.snapshot.name, created_at=int(time.time()),
                             sha256=file_digest(self.snapshot), table_rows={t: 0 for t in TABLES},
                             table_sha256={t: hashlib.sha256(b"").hexdigest() for t in TABLES})
        atomic_json(str(self.snapshot) + ".json", dict(manifest=self.manifest, hmac_sha256=manifest_signature(backup_secret(self.runtime), self.manifest)))
        self.ledger = self.runtime.backups / ("albums-deletions-" + "2" * 32 + ".json")
        payload = dict(scope=LEDGER_SCOPE, database="albums_test", exported_at=int(time.time()), entries=[])
        atomic_json(self.ledger, dict(payload=payload, hmac_sha256=manifest_signature(backup_secret(self.runtime), payload)))
        self.remote = root / "controlled-transport.enc"
        self.calls = []

    def move(self, runtime, executable, credentials, operation, path, key, digest, size, **pinned):
        self.calls.append(operation)
        result = dict(Key=key, SHA256=digest, Bytes=size, Version="test-version", Bucket="test-1250000000", Region="ap-guangzhou")
        if operation == "put":
            destination = self.remote if key.endswith(".enc") else self.remote.with_suffix(".receipt.json")
            shutil.copyfile(path, destination)
            self.assertNotIn(b"albums-deletion-ledger-v1", destination.read_bytes())
        else:
            self.assertEqual(pinned, {k: result[k] for k in ("Version", "Bucket", "Region")})
            shutil.copyfile(self.remote, path)
        return result

    def publish(self):
        return publish(self.runtime, "/controlled/bin", "/controlled/config", self.snapshot.name, self.ledger, file_digest(self.ledger))

    def test_real_encryption_pinned_roundtrip_and_isolated_outputs(self):
        with patch("offsite.move", side_effect=self.move):
            receipt = self.publish()
            result = fetch(self.runtime, "/controlled/bin", "/controlled/config", receipt["receipt"], receipt["receipt_sha256"])
        self.assertEqual(self.calls, ["put", "put", "get"])
        self.assertEqual(file_digest(self.remote.with_suffix(".receipt.json")), receipt["receipt_sha256"])
        self.assertTrue(receipt["remotely_read_verified"])
        self.assertFalse(receipt["latest_checkpoint_proven"])
        self.assertFalse(result["recovery_ready"])
        folder = Path(result["directory"])
        self.assertEqual((folder / self.snapshot.name).read_bytes(), self.snapshot.read_bytes())
        self.assertEqual((folder / self.ledger.name).read_bytes(), self.ledger.read_bytes())
        self.assertEqual(len(list(folder.iterdir())), 3)
        self.assertEqual(folder.stat().st_mode & 0o077, 0)
        self.assertTrue(all(p.stat().st_mode & 0o077 == 0 for p in folder.iterdir()))

    def test_upload_failure_never_issues_receipt(self):
        with patch("offsite.move", side_effect=OperationError("uncertain upload")):
            with self.assertRaises(OperationError): self.publish()
        self.assertEqual(list(self.runtime.backups.glob("albums-offsite-*.json")), [])
        self.assertEqual(list(self.runtime.backups.glob(".albums-offsite-*")), [])

    def test_receipt_tampering_and_wrong_scope_rejected_before_transfer(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        path = Path(receipt["receipt"])
        envelope = json.loads(path.read_text())
        envelope["receipt"]["database"] = "other_app"
        atomic_json(path, envelope)
        with patch("offsite.move") as transfer:
            with self.assertRaises(OperationError): fetch(self.runtime, "bin", "config", path, receipt["receipt_sha256"])
            with self.assertRaises(OperationError): fetch(self.runtime, "bin", "config", path, file_digest(path))
            envelope["hmac_sha256"] = manifest_signature(backup_secret(self.runtime), envelope["receipt"])
            atomic_json(path, envelope)
            with self.assertRaises(OperationError): fetch(self.runtime, "bin", "config", path, file_digest(path))
            transfer.assert_not_called()

    def test_corrupted_download_removed_before_decryption(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        self.remote.write_bytes(b"truncated")
        with patch("offsite.move", side_effect=self.move), patch("offsite.crypt") as decrypt:
            with self.assertRaises(OperationError): fetch(self.runtime, "bin", "config", receipt["receipt"], receipt["receipt_sha256"])
            decrypt.assert_not_called()
        self.assertEqual(list(self.runtime.backups.glob("albums-recovered-*")), [])

    def test_invalid_ledger_and_expired_snapshot_never_uploaded(self):
        with patch("offsite.move") as transfer:
            with self.assertRaises(OperationError): publish(self.runtime, "bin", "config", self.snapshot.name, self.ledger, "0" * 64)
            self.manifest["created_at"] -= 15 * 86400
            atomic_json(str(self.snapshot) + ".json", dict(manifest=self.manifest, hmac_sha256=manifest_signature(backup_secret(self.runtime), self.manifest)))
            with self.assertRaises(OperationError): self.publish()
            transfer.assert_not_called()

    def test_receipt_upload_failure_can_resume_without_bundle_reupload(self):
        def interrupted(*args, **kwargs):
            if args[5].endswith(".receipt.json"):
                raise OperationError("receipt response lost")
            return self.move(*args, **kwargs)
        with patch("offsite.move", side_effect=interrupted):
            with self.assertRaises(OperationError): self.publish()
        receipts = list(self.runtime.backups.glob("albums-offsite-*.json"))
        self.assertEqual(len(receipts), 1)
        self.assertEqual(self.calls, ["put"])
        bundle_hash = file_digest(self.remote)
        with patch("offsite.move", side_effect=self.move):
            result = publish_receipt(self.runtime, "bin", "config", receipts[0], file_digest(receipts[0]))
        self.assertEqual(self.calls, ["put", "put"])
        self.assertEqual(file_digest(self.remote), bundle_hash)
        self.assertEqual(result["receipt_remote"]["SHA256"], file_digest(receipts[0]))

    def test_private_receipt_is_required(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        path = Path(receipt["receipt"])
        path.chmod(0o644)
        with self.assertRaises(OperationError): read_receipt(self.runtime, path, receipt["receipt_sha256"])

    def recovered(self):
        with patch("offsite.move", side_effect=self.move):
            receipt = self.publish()
            result = fetch(self.runtime, "bin", "config", receipt["receipt"], receipt["receipt_sha256"])
        return Path(result["directory"])

    def test_recovered_expiry_uses_original_age_and_preserves_independent_ledger(self):
        folder = self.recovered()
        deadline = self.manifest["created_at"] + RETENTION
        with patch("backup.time.time", return_value=deadline - 1):
            self.assertEqual(prune(self.runtime), 0)
        self.assertTrue(folder.exists())
        with patch("backup.time.time", return_value=deadline):
            self.assertEqual(prune(self.runtime), 2)
            self.assertEqual(prune(self.runtime), 0)
        self.assertFalse(folder.exists())
        self.assertTrue(self.ledger.exists())
        self.assertTrue(self.remote.exists())

    def test_recovered_cleanup_resumes_after_partial_unlink(self):
        folder = self.recovered()
        original = Path.unlink
        deleted = []
        def interrupted(path, *args, **kwargs):
            if path.parent == folder:
                if deleted:
                    raise OSError("controlled interrupted cleanup")
                deleted.append(path.name)
            return original(path, *args, **kwargs)
        with patch("backup.time.time", return_value=self.manifest["created_at"] + RETENTION):
            with patch.object(Path, "unlink", interrupted):
                with self.assertRaises(OSError): prune(self.runtime)
            self.assertEqual(len(deleted), 1)
            self.assertTrue((folder / ".expiry.json").exists())
            self.assertEqual(prune(self.runtime), 2)
        self.assertFalse(folder.exists())

    def test_recovered_unknown_files_and_corruption_are_preserved(self):
        folder = self.recovered()
        unknown = folder / "keep.txt"
        unknown.write_text("user deliverable")
        unknown.chmod(0o600)
        with patch("backup.time.time", return_value=self.manifest["created_at"] + RETENTION):
            with self.assertRaises(OperationError): prune(self.runtime)
            self.assertTrue(unknown.exists())
            unknown.unlink()
            (folder / self.ledger.name).write_text("corrupted ledger")
            with self.assertRaises(OperationError): prune(self.runtime)
        self.assertTrue(self.snapshot.exists())
        self.assertTrue((folder / self.snapshot.name).exists())

    def test_recovered_symlink_cannot_remove_external_files(self):
        folder = self.recovered()
        member = folder / self.ledger.name
        member.unlink()
        member.symlink_to(self.ledger)
        with patch("backup.time.time", return_value=self.manifest["created_at"] + RETENTION):
            with self.assertRaises(OperationError): prune(self.runtime)
        self.assertTrue(self.ledger.exists())
        self.assertTrue(self.snapshot.exists())

    def test_recovered_cleanup_receipt_tampering_rejected_on_retry(self):
        folder = self.recovered()
        original = Path.unlink
        def interrupted(path, *args, **kwargs):
            if path.parent == folder:
                raise OSError("controlled interruption")
            return original(path, *args, **kwargs)
        with patch("backup.time.time", return_value=self.manifest["created_at"] + RETENTION):
            with patch.object(Path, "unlink", interrupted):
                with self.assertRaises(OSError): prune(self.runtime)
            receipt = folder / ".expiry.json"
            envelope = json.loads(receipt.read_text())
            envelope["plan"]["database"] = "another_app"
            atomic_json(receipt, envelope)
            with self.assertRaises(OperationError): prune(self.runtime)
        self.assertEqual(len(list(folder.iterdir())), 4)


if __name__ == "__main__": unittest.main()
