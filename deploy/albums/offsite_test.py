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
from offsite import publish, fetch, read_receipt, publish_receipt, expire
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
        self.credentials = root / "controlled-cos.json"
        atomic_json(self.credentials, dict(Bucket="test-1250000000", Region="ap-guangzhou",
                                          SecretID="private-fixture-id", SecretKey="private-fixture-secret"))
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
        return publish(self.runtime, "/controlled/bin", self.credentials, self.snapshot.name, self.ledger, file_digest(self.ledger))

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

    def test_upload_failure_retains_only_unconfirmed_intent(self):
        with patch("offsite.move", side_effect=OperationError("uncertain upload")):
            with self.assertRaises(OperationError): self.publish()
        intents = list(self.runtime.backups.glob("albums-offsite-*.json"))
        self.assertEqual(len(intents), 1)
        intent = json.loads(intents[0].read_text())["receipt"]
        self.assertFalse(intent["publication_confirmed"])
        self.assertNotIn("private-fixture", intents[0].read_text())
        self.assertEqual(intent["remote"]["Version"], "")
        self.assertEqual(intents[0].stat().st_mode & 0o077, 0)
        with patch("offsite.move") as transfer:
            with self.assertRaises(OperationError): fetch(self.runtime, "bin", "config", intents[0], file_digest(intents[0]))
            with self.assertRaises(OperationError): publish_receipt(self.runtime, "bin", "config", intents[0], file_digest(intents[0]))
            transfer.assert_not_called()
        self.assertEqual(list(self.runtime.backups.glob(".albums-offsite-*")), [])

    def test_intent_must_be_durable_before_upload(self):
        with patch("offsite.atomic_json", side_effect=OSError("controlled disk full")), patch("offsite.move") as transfer:
            with self.assertRaises(OSError): self.publish()
            transfer.assert_not_called()

    def test_confirmation_write_failure_preserves_preupload_intent(self):
        writes = []
        def persist(path, data):
            writes.append(data["receipt"]["publication_confirmed"])
            if len(writes) == 2:
                raise OSError("controlled confirmation disk failure")
            atomic_json(path, data)
        with patch("offsite.atomic_json", side_effect=persist), patch("offsite.move", side_effect=self.move):
            with self.assertRaises(OSError): self.publish()
        self.assertEqual(writes, [False, True])
        self.assertEqual(self.calls, ["put"])
        path = next(self.runtime.backups.glob("albums-offsite-*.json"))
        with patch("offsite.time.time", return_value=self.manifest["created_at"] + RETENTION):
            pending = read_receipt(self.runtime, path, file_digest(path), expired=True)
        self.assertFalse(pending["publication_confirmed"])
        self.assertEqual(pending["remote"]["SHA256"], file_digest(self.remote))

    def test_legacy_confirmed_receipt_remains_readable(self):
        with patch("offsite.move", side_effect=self.move): result = self.publish()
        path = Path(result["receipt"])
        receipt = json.loads(path.read_text())["receipt"]
        receipt.pop("publication_confirmed")
        atomic_json(path, dict(receipt=receipt, hmac_sha256=manifest_signature(backup_secret(self.runtime), receipt)))
        self.assertEqual(read_receipt(self.runtime, path, file_digest(path)), receipt)

    def test_lost_upload_response_can_expire_using_exact_intent(self):
        def uncertain(*args, **kwargs):
            intents = list(self.runtime.backups.glob("albums-offsite-*.json"))
            self.assertEqual(len(intents), 1)
            plan = json.loads(intents[0].read_text())["receipt"]
            self.assertFalse(plan["publication_confirmed"])
            self.assertEqual(plan["remote"]["Key"], args[5])
            self.assertEqual(plan["remote"]["SHA256"], file_digest(args[4]))
            self.assertEqual(kwargs, {"Bucket": "test-1250000000", "Region": "ap-guangzhou"})
            self.move(*args, **kwargs)
            raise OperationError("controlled response lost after storage")
        with patch("offsite.move", side_effect=uncertain):
            with self.assertRaises(OperationError): self.publish()
        path = next(self.runtime.backups.glob("albums-offsite-*.json"))
        self.assertTrue(self.remote.exists())
        self.assertFalse(self.remote.with_suffix(".receipt.json").exists())
        def cleanup(name, command, *, input, timeout):
            request = json.loads(input)
            self.assertEqual(request["SHA256"], file_digest(self.remote))
            self.assertEqual(request["Bytes"], self.remote.stat().st_size)
            return json.dumps(dict(Empty=True, VersionsRemoved=1, **{k: request[k] for k in ("Key", "Bucket", "Region")})).encode()
        with patch("offsite.time.time", return_value=self.manifest["created_at"] + RETENTION), patch.object(self.runtime, "execute", side_effect=cleanup):
            self.assertFalse(expire(self.runtime, "bin", "cleanup-config", path, file_digest(path))["account_erasure_complete"])

    def test_destination_must_be_private_and_regular(self):
        original = self.credentials.read_bytes()
        self.credentials.chmod(0o644)
        with patch("offsite.move") as transfer:
            with self.assertRaises(OperationError): self.publish()
            self.credentials.chmod(0o600)
            self.credentials.write_text('{"Bucket":"other/invalid","Region":"ap-guangzhou"}')
            with self.assertRaises(OperationError): self.publish()
            self.credentials.write_bytes(original)
            linked = self.credentials.with_name("linked.json")
            linked.symlink_to(self.credentials)
            self.credentials = linked
            with self.assertRaises(OperationError): self.publish()
            transfer.assert_not_called()
        self.assertEqual(list(self.runtime.backups.glob("albums-offsite-*.json")), [])

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

    def test_offsite_expiry_requires_authenticated_expired_receipt(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        path = Path(receipt["receipt"])
        with patch.object(self.runtime, "execute") as execute:
            with self.assertRaises(OperationError):
                expire(self.runtime, "bin", "cleanup-config", path, receipt["receipt_sha256"])
            with patch("offsite.time.time", return_value=self.manifest["created_at"] + RETENTION):
                with self.assertRaises(OperationError):
                    expire(self.runtime, "bin", "cleanup-config", path, "0" * 64)
                with self.assertRaises(OperationError):
                    fetch(self.runtime, "bin", "config", path, receipt["receipt_sha256"])
            execute.assert_not_called()

    def test_offsite_expiry_checks_transport_completion_and_retains_receipt(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        path = Path(receipt["receipt"])
        remote = receipt["remote"]
        result = {k: remote[k] for k in ("Key", "Bucket", "Region")}
        result.update(Empty=True, VersionsRemoved=4)
        def execute(name, command, *, input, timeout):
            request = json.loads(input)
            self.assertEqual(name, "albums-backup-expire")
            self.assertEqual(command, ["bin", "cleanup-config"])
            self.assertEqual(request, dict(Operation="expire", ReceiptSHA256=receipt["receipt_sha256"],
                                          ReceiptBytes=path.stat().st_size,
                                          **{k: remote[k] for k in ("Key", "Bucket", "Region", "Bytes", "SHA256")}))
            return json.dumps(result).encode()
        with patch("offsite.time.time", return_value=self.manifest["created_at"] + RETENTION), patch.object(self.runtime, "execute", side_effect=execute):
            completed = expire(self.runtime, "bin", "cleanup-config", path, receipt["receipt_sha256"])
            self.assertFalse(completed["account_erasure_complete"])
            for key, value in (("Empty", False), ("VersionsRemoved", True), ("VersionsRemoved", 10001), ("Bucket", "other")):
                previous = result[key]
                result[key] = value
                with self.assertRaises(OperationError):
                    expire(self.runtime, "bin", "cleanup-config", path, receipt["receipt_sha256"])
                result[key] = previous
        self.assertTrue(path.exists())
        self.assertTrue(self.snapshot.exists())
        self.assertTrue(self.ledger.exists())

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
