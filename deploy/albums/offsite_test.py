import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import time
import unittest
from unittest.mock import patch

from backup import Runtime, TABLES, SCOPE, configure, OperationError
from deletion_ledger import SCOPE as LEDGER_SCOPE
from offsite import publish, fetch, read_receipt
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
            shutil.copyfile(path, self.remote)
            self.assertNotIn(b"albums-deletion-ledger-v1", self.remote.read_bytes())
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
        self.assertEqual(self.calls, ["put", "get"])
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

    def test_private_receipt_is_required(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        path = Path(receipt["receipt"])
        path.chmod(0o644)
        with self.assertRaises(OperationError): read_receipt(self.runtime, path, receipt["receipt_sha256"])


if __name__ == "__main__": unittest.main()
