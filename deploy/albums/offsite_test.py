import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
import offsite

from backup import Runtime, TABLES, SCOPE, RETENTION, configure, prune, OperationError
from deletion_ledger import SCOPE as LEDGER_SCOPE
from offsite import publish, fetch, read_receipt, publish_receipt, expire, audit, initiate, MULTIPART_PROTOCOL
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
        payload = dict(scope=LEDGER_SCOPE, database="albums_test", exported_at=self.manifest["created_at"], entries=[], assets=[], collections=[])
        atomic_json(self.ledger, dict(payload=payload, hmac_sha256=manifest_signature(backup_secret(self.runtime), payload)))
        self.remote = root / "controlled-transport.enc"
        self.credentials = root / "controlled-cos.json"
        atomic_json(self.credentials, dict(Bucket="test-1250000000", Region="ap-guangzhou",
                                          SecretID="private-fixture-id", SecretKey="private-fixture-secret"))
        self.calls = []
        self.initiations = []
        self.initiate_patch = patch("offsite.initiate", side_effect=self.initiate)
        self.initiate_patch.start()
        self.addCleanup(self.initiate_patch.stop)

    def initiate(self, runtime, executable, credentials, key, target):
        self.initiations.append(key)
        plans = [json.loads(p.read_text())["receipt"] for p in self.runtime.backups.glob("albums-offsite-*.json")]
        bundle = key.removesuffix(".receipt.json") + ".enc" if key.endswith(".receipt.json") else key
        plan = next(r for r in plans if r["remote"]["Key"] == bundle)
        self.assertEqual(plan["upload_protocol"], MULTIPART_PROTOCOL)
        self.assertEqual({k: plan["remote"][k] for k in target}, target)
        self.assertFalse(plan["receipt_upload_id"] if key.endswith(".receipt.json") else plan["remote"]["UploadID"])
        return "receipt-session" if key.endswith(".receipt.json") else "bundle-session"

    def move(self, runtime, executable, credentials, operation, path, key, digest, size, **pinned):
        self.calls.append(operation)
        result = dict(Key=key, SHA256=digest, Bytes=size, Version="test-version", Bucket="test-1250000000", Region="ap-guangzhou")
        if operation == "put":
            result["UploadID"] = pinned["UploadID"]
            plans = [json.loads(p.read_text())["receipt"] for p in self.runtime.backups.glob("albums-offsite-*.json")]
            bundle = key.removesuffix(".receipt.json") + ".enc" if key.endswith(".receipt.json") else key
            plan = next(r for r in plans if r["remote"]["Key"] == bundle)
            self.assertEqual(pinned["UploadID"], plan["receipt_upload_id"] if key.endswith(".receipt.json") else plan["remote"]["UploadID"])
            destination = self.remote if key.endswith(".enc") else self.remote.with_suffix(".receipt.json")
            shutil.copyfile(path, destination)
            self.assertNotIn(b"albums-deletion-ledger-v1", destination.read_bytes())
        else:
            self.assertEqual(pinned, {k: result[k] for k in ("Version", "Bucket", "Region")})
            shutil.copyfile(self.remote, path)
            path.chmod(0o600)  # The real download adapter creates its destination privately with O_EXCL.
        return result

    def publish(self):
        return publish(self.runtime, "/controlled/bin", self.credentials, self.snapshot.name, self.ledger, file_digest(self.ledger))

    def audit_inventory(self, versions=(), uploads=()):
        return dict(Bucket="test-1250000000", Region="ap-guangzhou", Prefix="albums/recovery/",
                    Versions=list(versions), Uploads=list(uploads))

    def run_audit(self, inventory):
        def execute(name, command, *, input, timeout):
            self.assertEqual(name, "albums-backup-inventory")
            self.assertEqual(command, ["/controlled/bin", str(self.credentials)])
            self.assertEqual(json.loads(input), dict(Operation="inventory", Key="albums/recovery/",
                                                   Bucket="test-1250000000", Region="ap-guangzhou"))
            self.assertEqual(timeout, 960)
            return json.dumps(inventory).encode()
        with patch.object(self.runtime, "execute", side_effect=execute):
            return audit(self.runtime, "/controlled/bin", self.credentials)

    def expired_for_audit(self):
        with patch("offsite.move", side_effect=self.move): published = self.publish()
        path = Path(published["receipt"])
        receipt = json.loads(path.read_text())["receipt"]
        receipt["snapshot_created_at"] -= RETENTION + 1
        self.save_prune_intent(path, receipt)
        published["receipt_sha256"] = file_digest(path)
        remote = published["remote"]
        result = dict(Empty=True, UploadsEmpty=True, VersionsRemoved=2, SessionsClosed=2,
                      **{k: remote[k] for k in ("Key", "Bucket", "Region")})
        at = int(time.time())
        with patch("offsite.time.time", return_value=at), patch.object(self.runtime, "execute", return_value=json.dumps(result).encode()):
            expire(self.runtime, "/controlled/bin", self.credentials, published["receipt"], published["receipt_sha256"])
        return published, at

    def test_audit_recognizes_authenticated_expiry_but_never_claims_account_erasure(self):
        published, at = self.expired_for_audit()
        with patch("offsite.time.time", return_value=at):
            result = self.run_audit(self.audit_inventory())
        self.assertEqual(result["missing_confirmed_bundle_versions"], [])
        self.assertEqual(result["missing_remote_receipts"], [])
        self.assertEqual(result["confirmed_expired_keys"], [published["remote"]["Key"]])
        self.assertFalse(result["has_unresolved_items"])
        self.assertFalse(result["writers_quiescent"])
        self.assertFalse(result["account_erasure_complete"])

    def test_expiry_confirmation_is_private_authenticated_and_does_not_replace_publication(self):
        published, _ = self.expired_for_audit()
        path = next(self.runtime.backups.glob("albums-expired-*.json"))
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        envelope = json.loads(path.read_text())
        self.assertEqual(envelope["hmac_sha256"], manifest_signature(backup_secret(self.runtime), envelope["expiry"]))
        self.assertEqual(envelope["expiry"]["receipt_sha256"], file_digest(published["receipt"]))
        self.assertTrue(self.snapshot.exists() and self.ledger.exists())
        self.assertNotIn("private-fixture", path.read_text())

    def test_audit_flags_objects_markers_or_uploads_reappearing_after_confirmed_expiry(self):
        published, at = self.expired_for_audit(); remote = published["remote"]; key = remote["Key"]
        for mode in ("bundle", "receipt", "marker", "bundle-upload", "receipt-upload"):
            with self.subTest(mode=mode), patch("offsite.time.time", return_value=at):
                receipt_key = key.removesuffix(".enc") + ".receipt.json"
                inventory = self.audit_inventory()
                if "upload" in mode:
                    inventory["Uploads"] = [dict(Key=receipt_key if mode == "receipt-upload" else key, UploadID="late-session")]
                else:
                    inventory["Versions"] = [dict(Key=receipt_key if mode == "receipt" else key,
                        Version="late-version", Bytes=0 if mode == "marker" else remote["Bytes"], DeleteMarker=mode == "marker")]
                result = self.run_audit(inventory)
                self.assertEqual(result["confirmed_expired_keys"], [])
                self.assertEqual(result["reappeared_expired_keys"], [key])
                self.assertTrue(result["has_unresolved_items"])
                self.assertFalse(result["writers_quiescent"])

    def test_expiry_confirmation_scope_pin_age_and_completion_are_required_before_transport(self):
        published, at = self.expired_for_audit()
        path = next(self.runtime.backups.glob("albums-expired-*.json")); original = json.loads(path.read_text())
        minimum = json.loads(Path(published["receipt"]).read_text())["receipt"]["snapshot_created_at"] + RETENTION
        cases = [("scope", "other"), ("database", "other"), ("receipt_sha256", "0" * 64),
                 ("confirmed_at", True), ("confirmed_at", minimum - 1), ("confirmed_at", at + 301),
                 ("Key", "albums/recovery/" + "f" * 32 + ".enc"), ("Bucket", "other-1250000000"),
                 ("Region", "ap-shanghai"), ("Empty", False), ("UploadsEmpty", False),
                 ("VersionsRemoved", True), ("VersionsRemoved", -1), ("SessionsClosed", 10003),
                 ("extra", "field"), ("tampered", "signature")]
        for field, value in cases:
            with self.subTest(field=field, value=value), patch("offsite.time.time", return_value=at):
                envelope = json.loads(json.dumps(original)); proof = envelope["expiry"]
                if field in original["expiry"]["remote"]: proof["remote"][field] = value
                else: proof[field] = value
                if field != "tampered": envelope["hmac_sha256"] = manifest_signature(backup_secret(self.runtime), proof)
                atomic_json(path, envelope)
                with patch.object(self.runtime, "execute") as execute:
                    with self.assertRaises(OperationError): audit(self.runtime, "bin", self.credentials)
                    with self.assertRaises(OperationError): offsite.prune_expired(self.runtime, "bin", self.credentials)
                    with self.assertRaises(OperationError): expire(self.runtime, "bin", self.credentials, published["receipt"], published["receipt_sha256"])
                    execute.assert_not_called()
                self.assertEqual(json.loads(path.read_text()), envelope)

    def test_expiry_confirmation_unsafe_unknown_and_orphan_files_stop_before_listing(self):
        published, at = self.expired_for_audit()
        path = next(self.runtime.backups.glob("albums-expired-*.json")); original = path.read_bytes()
        for mode in ("public", "symlink", "hardlink", "fifo", "oversize", "name", "orphan"):
            with self.subTest(mode=mode), patch("offsite.time.time", return_value=at):
                path.write_bytes(original); path.chmod(0o600); current = path
                held = path.with_name("held-proof")
                if mode == "public": path.chmod(0o644)
                elif mode == "oversize": path.write_bytes(b"x" * 65537)
                elif mode == "name":
                    current = path.with_name("albums-expired-review.json"); path.rename(current)
                elif mode == "orphan":
                    current = path.with_name("albums-expired-" + "f" * 32 + ".json"); path.rename(current)
                else:
                    path.rename(held)
                    if mode == "symlink": path.symlink_to(held)
                    elif mode == "hardlink": os.link(held, path)
                    else: os.mkfifo(path, 0o600)
                with patch.object(self.runtime, "execute") as execute:
                    with self.assertRaises(OperationError): audit(self.runtime, "bin", self.credentials)
                    with self.assertRaises(OperationError): offsite.prune_expired(self.runtime, "bin", self.credentials)
                    execute.assert_not_called()
                self.assertTrue(current.exists() or current.is_symlink()); current.unlink()
                if held.exists(): held.unlink()

    def test_expiry_confirmation_enumeration_limit_is_bounded_before_listing(self):
        import itertools
        self.expired_for_audit(); path = next(self.runtime.backups.glob("albums-expired-*.json"))
        glob = Path.glob
        def enumerate_paths(folder, pattern):
            return itertools.repeat(path) if pattern == "albums-expired-*" else glob(folder, pattern)
        with patch.object(Path, "glob", enumerate_paths), patch.object(self.runtime, "execute") as execute:
            with self.assertRaisesRegex(OperationError, "limit"): audit(self.runtime, "bin", self.credentials)
            execute.assert_not_called()

    def test_confirmation_write_failure_or_cancellation_keeps_intent_and_retry_rechecks_remote(self):
        for failure in (OSError("controlled disk full"), KeyboardInterrupt()):
            with self.subTest(failure=type(failure).__name__):
                path, receipt = self.prune_intent("1"); digest = file_digest(path)
                proof = offsite._expiry_path(self.runtime, receipt); calls = []
                def cleanup(*args, **kwargs):
                    calls.append(1)
                    return json.dumps(dict(Empty=True, UploadsEmpty=True, VersionsRemoved=2 if len(calls) == 1 else 0,
                        SessionsClosed=0, **{k: receipt["remote"][k] for k in ("Key", "Bucket", "Region")})).encode()
                with patch.object(self.runtime, "execute", side_effect=cleanup):
                    with patch("offsite.atomic_json", side_effect=failure):
                        with self.assertRaises(type(failure)): expire(self.runtime, "bin", self.credentials, path, digest)
                    self.assertFalse(proof.exists()); self.assertEqual(file_digest(path), digest)
                    completed = expire(self.runtime, "bin", self.credentials, path, digest)
                self.assertEqual(completed["remote"]["VersionsRemoved"], 0)
                self.assertEqual(len(calls), 2); self.assertTrue(proof.exists())
                self.assertFalse(self.run_audit(self.audit_inventory())["has_unresolved_items"])
                proof.unlink(); path.unlink()

    def test_expiry_confirmation_cannot_authorize_restore_or_receipt_republication_after_clock_rollback(self):
        published, at = self.expired_for_audit()
        before_expiry = json.loads(Path(published["receipt"]).read_text())["receipt"]["snapshot_created_at"] + RETENTION - 1
        with patch("offsite.time.time", return_value=before_expiry), patch("offsite.move") as move, patch("offsite.initiate") as start:
            with self.assertRaisesRegex(OperationError, "cannot be restored or resumed"):
                fetch(self.runtime, "bin", self.credentials, published["receipt"], published["receipt_sha256"])
            with self.assertRaisesRegex(OperationError, "cannot be restored or resumed"):
                publish_receipt(self.runtime, "bin", self.credentials, published["receipt"], published["receipt_sha256"])
            move.assert_not_called(); start.assert_not_called()

    def test_failed_expiry_never_creates_an_authenticated_confirmation(self):
        path, receipt = self.prune_intent("1"); digest = file_digest(path)
        valid = dict(Empty=True, UploadsEmpty=True, VersionsRemoved=2, SessionsClosed=0,
                     **{k: receipt["remote"][k] for k in ("Key", "Bucket", "Region")})
        for field, value in (("Empty", False), ("UploadsEmpty", False), ("VersionsRemoved", True),
                             ("SessionsClosed", -1), ("Key", "foreign")):
            result = dict(valid, **{field: value})
            with self.subTest(field=field), patch.object(self.runtime, "execute", return_value=json.dumps(result).encode()):
                with self.assertRaises(OperationError): expire(self.runtime, "bin", self.credentials, path, digest)
                self.assertFalse(offsite._expiry_path(self.runtime, receipt).exists())
                self.assertEqual(file_digest(path), digest)

    def test_expiry_preserves_a_confirmation_replaced_while_transport_is_running(self):
        published, _ = self.expired_for_audit(); path = next(self.runtime.backups.glob("albums-expired-*.json"))
        remote = published["remote"]
        def cleanup(*args, **kwargs):
            path.write_text("unknown replacement preserved")
            return json.dumps(dict(Empty=True, UploadsEmpty=True, VersionsRemoved=0, SessionsClosed=0,
                                   **{k: remote[k] for k in ("Key", "Bucket", "Region")})).encode()
        with patch.object(self.runtime, "execute", side_effect=cleanup):
            with self.assertRaises(OperationError):
                expire(self.runtime, "bin", self.credentials, published["receipt"], published["receipt_sha256"])
        self.assertEqual(path.read_text(), "unknown replacement preserved")

    def test_legacy_expiry_does_not_mint_a_multipart_completion_confirmation(self):
        path, receipt = self.prune_intent("1")
        del receipt["upload_protocol"]; del receipt["receipt_upload_id"]; del receipt["remote"]["UploadID"]
        self.save_prune_intent(path, receipt)
        response = dict(Empty=True, VersionsRemoved=0, **{k: receipt["remote"][k] for k in ("Key", "Bucket", "Region")})
        with patch.object(self.runtime, "execute", return_value=json.dumps(response).encode()):
            result = expire(self.runtime, "bin", self.credentials, path, file_digest(path))
        self.assertIsNone(result["expiry_confirmation"])
        self.assertEqual(list(self.runtime.backups.glob("albums-expired-*")), [])
        self.assertTrue(self.run_audit(self.audit_inventory())["has_unresolved_items"])

    def test_expiry_confirmation_survives_process_exit_before_success_report_and_next_audit_reads_it(self):
        path, receipt = self.prune_intent("1"); digest = file_digest(path)
        root = self.runtime.root; executable = root / "controlled-expiry-transport"
        executable.write_text("#!" + sys.executable + "\n" + '''
import json,sys
r=json.load(sys.stdin)
if r['Operation']=='inventory':
    print(json.dumps(dict(Bucket=r['Bucket'],Region=r['Region'],Prefix=r['Key'],Versions=[],Uploads=[])))
else:
    assert r['Operation']=='expire'
    print(json.dumps(dict(Empty=True,UploadsEmpty=True,VersionsRemoved=0,SessionsClosed=0,
        **{k:r[k] for k in ('Key','Bucket','Region')})))
''')
        executable.chmod(0o700)
        env = dict(os.environ, PYTHONPATH=str(Path(__file__).parent.resolve()) + os.pathsep + str(Path(__file__).parents[1] / "tencent"))
        common = ["--backend-dir", str(root), "--environment-file", str(self.runtime.environment_file),
                  "--backup-dir", str(self.runtime.backups), "--transfer-bin", str(executable), "--cos-config", str(self.credentials)]
        script = '''
import backup,offsite,os,sys
save=offsite.atomic_json
def interrupted(path,envelope):
    save(path,envelope)
    if 'expiry' in envelope: os._exit(73)
offsite.atomic_json=interrupted
sys.exit(backup.main())
'''
        stopped = subprocess.run([sys.executable, "-c", script, "expire-offsite", *common,
            "--receipt", str(path), "--receipt-sha256", digest], env=env, capture_output=True, text=True, timeout=10)
        self.assertEqual(stopped.returncode, 73, stopped.stdout + stopped.stderr)
        self.assertNotIn('"passed": true', stopped.stdout)
        proof = offsite._expiry_path(self.runtime, receipt); before = file_digest(proof)
        checked = subprocess.run([sys.executable, str(Path(__file__).with_name("backup.py")), "audit-offsite", *common],
            env=env, capture_output=True, text=True, timeout=10)
        self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)
        result = json.loads(checked.stdout)
        self.assertEqual(result["confirmed_expired_keys"], [receipt["remote"]["Key"]])
        self.assertFalse(result["has_unresolved_items"])
        self.assertFalse(result["account_erasure_complete"])
        self.assertEqual(file_digest(path), digest); self.assertEqual(file_digest(proof), before)

    def test_prune_offsite_processes_only_expired_receipts_and_preserves_local_inputs(self):
        with patch("offsite.move", side_effect=self.move):
            first = self.publish()
            self.publish()
        path = Path(first["receipt"])
        receipt = json.loads(path.read_text())["receipt"]
        receipt["snapshot_created_at"] -= RETENTION
        atomic_json(path, dict(receipt=receipt, hmac_sha256=manifest_signature(backup_secret(self.runtime), receipt)))
        before = {p: file_digest(p) for p in self.runtime.backups.iterdir()}
        calls = []
        def cleanup(name, command, *, input, timeout):
            request = json.loads(input); calls.append(request)
            self.assertEqual(name, "albums-backup-expire")
            return json.dumps(dict(Empty=True, UploadsEmpty=True, VersionsRemoved=2, SessionsClosed=2,
                                   **{k: request[k] for k in ("Key", "Bucket", "Region")})).encode()
        with self.runtime.lock(), patch.object(self.runtime, "execute", side_effect=cleanup):
            result = offsite.prune_expired(self.runtime, "/controlled/bin", self.credentials)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0]["Key"], receipt["remote"]["Key"])
        self.assertEqual(result["inspected"], 2)
        self.assertEqual(result["expired"], 1)
        self.assertFalse(result["account_erasure_complete"])
        self.assertFalse(result["writers_quiescent"])
        after = {p: file_digest(p) for p in self.runtime.backups.iterdir()}
        self.assertEqual(before, {p: after[p] for p in before})
        proof = offsite._expiry_path(self.runtime, receipt)
        self.assertEqual(set(after) - set(before), {proof})
        self.assertEqual(offsite._read_expiry_confirmation(self.runtime, proof, receipt, file_digest(path))["remote"]["Key"], receipt["remote"]["Key"])

    def prune_intent(self, nonce, age=RETENTION + 1):
        receipt = dict(scope=offsite.SCOPE, database="albums_test", published_at=int(time.time()),
                       snapshot=self.snapshot.name, snapshot_created_at=int(time.time()) - age,
                       ledger=self.ledger.name, ledger_sha256=file_digest(self.ledger), publication_confirmed=False,
                       upload_protocol=MULTIPART_PROTOCOL, receipt_upload_id="",
                       remote=dict(Key="albums/recovery/" + nonce * 32 + ".enc", Version="", UploadID="",
                                   SHA256="a" * 64, Bytes=10, Bucket="test-1250000000", Region="ap-guangzhou"))
        path = self.runtime.backups / ("albums-offsite-" + nonce * 32 + ".json")
        self.save_prune_intent(path, receipt)
        return path, receipt

    def save_prune_intent(self, path, receipt):
        atomic_json(path, dict(receipt=receipt, hmac_sha256=manifest_signature(backup_secret(self.runtime), receipt)))

    def test_prune_offsite_preflights_all_receipts_before_deleting_anything(self):
        self.prune_intent("1")
        path, _ = self.prune_intent("2")
        path.write_text('{"invalid":"signed plan missing"}')
        with patch.object(self.runtime, "execute") as execute:
            with self.assertRaises(OperationError): offsite.prune_expired(self.runtime, "bin", self.credentials)
            execute.assert_not_called()

    def test_prune_offsite_rejects_foreign_scope_destination_and_filename_before_transport(self):
        for field, value in [("database", "other"), ("Bucket", "other-1250000000"), ("Region", "ap-shanghai"),
                             ("Key", "albums/recovery/" + "f" * 32 + ".enc"), ("snapshot_created_at", True)]:
            with self.subTest(field=field):
                self.prune_intent("1")
                path, receipt = self.prune_intent("2", age=0)
                (receipt["remote"] if field in ("Bucket", "Region", "Key") else receipt)[field] = value
                self.save_prune_intent(path, receipt)
                with patch.object(self.runtime, "execute") as execute:
                    with self.assertRaises(OperationError): offsite.prune_expired(self.runtime, "bin", self.credentials)
                    execute.assert_not_called()
                path.unlink()

    def test_prune_offsite_unknown_and_unsafe_receipt_files_are_preserved(self):
        for kind in ("name", "permissions", "symlink", "fifo", "oversize"):
            with self.subTest(kind=kind):
                path, _ = self.prune_intent("1")
                if kind == "name":
                    new = path.with_name("albums-offsite-review.json"); path.rename(new); path = new
                elif kind == "permissions": path.chmod(0o644)
                elif kind == "symlink":
                    path.unlink(); path.symlink_to(self.ledger)
                elif kind == "fifo":
                    path.unlink(); os.mkfifo(path, 0o600)
                else:
                    with path.open("wb") as output: output.truncate(65537)
                with patch.object(self.runtime, "execute") as execute:
                    with self.assertRaises(OperationError): offsite.prune_expired(self.runtime, "bin", self.credentials)
                    execute.assert_not_called()
                self.assertTrue(path.exists() or path.is_symlink()); path.unlink()

    def test_prune_offsite_legacy_expired_plan_stops_entire_batch(self):
        self.prune_intent("1")
        path, receipt = self.prune_intent("2")
        del receipt["upload_protocol"]; del receipt["receipt_upload_id"]; del receipt["remote"]["UploadID"]
        self.save_prune_intent(path, receipt)
        with patch.object(self.runtime, "execute") as execute:
            with self.assertRaisesRegex(OperationError, "legacy"): offsite.prune_expired(self.runtime, "bin", self.credentials)
            execute.assert_not_called()

    def test_prune_offsite_pins_authenticated_bytes_before_a_receipt_can_change(self):
        path, receipt = self.prune_intent("1")
        read = offsite._read_receipt
        def change(runtime, source, digest, **kwargs):
            result = read(runtime, source, digest, **kwargs)
            if digest is None:
                receipt["published_at"] += 1; self.save_prune_intent(path, receipt)
            return result
        with patch("offsite._read_receipt", side_effect=change), patch.object(self.runtime, "execute") as execute:
            with self.assertRaisesRegex(OperationError, "digest mismatch"):
                offsite.prune_expired(self.runtime, "bin", self.credentials)
            execute.assert_not_called()
        self.assertTrue(path.exists())

    def test_prune_offsite_freezes_retention_cutoff_for_the_whole_batch(self):
        now = int(time.time()); self.prune_intent("1", age=RETENTION - 1)
        read = offsite._read_receipt
        with patch("offsite.time.time", return_value=now) as clock:
            def advance(*args, **kwargs):
                result = read(*args, **kwargs); clock.return_value = now + 30; return result
            with patch("offsite._read_receipt", side_effect=advance), patch.object(self.runtime, "execute") as execute:
                self.assertEqual(offsite.prune_expired(self.runtime, "bin", self.credentials)["expired"], 0)
                execute.assert_not_called()

    def test_prune_offsite_limit_is_bounded_before_any_transport(self):
        import itertools
        path, _ = self.prune_intent("1")
        with patch.object(Path, "glob", return_value=itertools.repeat(path)), patch.object(self.runtime, "execute") as execute:
            with self.assertRaisesRegex(OperationError, "limit"): offsite.prune_expired(self.runtime, "bin", self.credentials)
            execute.assert_not_called()

    def test_prune_offsite_partial_failure_stops_and_retry_rechecks_already_empty_prefixes(self):
        first, _ = self.prune_intent("1"); second, _ = self.prune_intent("2"); self.prune_intent("3")
        calls, removed = [], set()
        failure = [True]
        def cleanup(name, command, *, input, timeout):
            request = json.loads(input); key = request["Key"]; calls.append(key)
            self.assertEqual(request["Protocol"], MULTIPART_PROTOCOL)
            self.assertEqual(request["UploadID"], "")
            if key.endswith("2" * 32 + ".enc") and failure[0]: raise OperationError("controlled provider failure")
            count = 0 if key in removed else 1; removed.add(key)
            return json.dumps(dict(Empty=True, UploadsEmpty=True, VersionsRemoved=count, SessionsClosed=0,
                                   **{k: request[k] for k in ("Key", "Bucket", "Region")})).encode()
        with self.runtime.lock(), patch.object(self.runtime, "execute", side_effect=cleanup):
            with self.assertRaises(OperationError): offsite.prune_expired(self.runtime, "bin", self.credentials)
            self.assertEqual(len(calls), 2)
            self.assertEqual(len(removed), 1)
            self.assertTrue(first.exists() and second.exists())
            failure[0] = False
            result = offsite.prune_expired(self.runtime, "bin", self.credentials)
        self.assertEqual(result["expired"], 3)
        self.assertEqual([r["remote"]["VersionsRemoved"] for r in result["results"]], [0, 1, 1])
        self.assertEqual(len(removed), 3)

    def prune_cli(self, *extra):
        env = dict(os.environ, PYTHONPATH=str(Path(__file__).parent.resolve()) + os.pathsep + str(Path(__file__).parents[1] / "tencent"))
        return subprocess.run([sys.executable, str(Path(__file__).with_name("backup.py")), "prune-offsite",
            "--backend-dir", str(self.runtime.root), "--environment-file", str(self.runtime.environment_file),
            "--backup-dir", str(self.runtime.backups), "--transfer-bin", "/not-called", "--cos-config", str(self.credentials), *extra],
            env=env, capture_output=True, text=True, timeout=10)

    def test_prune_offsite_cli_obeys_operations_lock_and_refuses_single_plan_selectors(self):
        self.prune_intent("1", age=0)
        with self.runtime.lock():
            result = self.prune_cli()
            self.assertEqual(result.returncode, 1); self.assertIn("another operations operation", result.stdout)
        result = self.prune_cli()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(json.loads(result.stdout)["expired"], 0)
        for flag in ("--receipt", "--receipt-sha256", "--backup", "--deletion-ledger", "--deletion-ledger-sha256"):
            for value in ("explicit-single-item", ""):
                result = self.prune_cli(flag, value)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn("refuses single-backup selectors", result.stdout)

    def test_prune_offsite_cli_process_death_after_delete_releases_lock_and_can_resume(self):
        for nonce in ("1", "2", "3"): self.prune_intent(nonce)
        root = self.runtime.root
        executable = root / "controlled-cleanup"
        executable.write_text("#!" + sys.executable + "\n" + '''
import json,os,signal,sys,time
from pathlib import Path
root=Path(sys.argv[1]).parent
request=json.load(sys.stdin)
state=root/'controlled-removed.json'
removed=json.loads(state.read_text()) if state.exists() else []
count=0 if request['Key'] in removed else 1
if count: removed.append(request['Key'])
state.write_text(json.dumps(removed))
once=root/'controlled-interrupted'
if not once.exists():
    deadline=time.monotonic()+5
    guard=root/'owned-parent-pid'
    while not guard.exists() and time.monotonic()<deadline: time.sleep(.01)
    assert os.getppid()==int(guard.read_text())
    once.touch()
    os.kill(os.getppid(),signal.SIGKILL)
    (root/'controlled-child-finished').touch()
    os._exit(0)
print(json.dumps(dict(Empty=True,UploadsEmpty=True,VersionsRemoved=count,SessionsClosed=0,
    **{k:request[k] for k in ('Key','Bucket','Region')})))
''')
        executable.chmod(0o700)
        env = dict(os.environ, PYTHONPATH=str(Path(__file__).parent.resolve()) + os.pathsep + str(Path(__file__).parents[1] / "tencent"))
        args = [sys.executable, str(Path(__file__).with_name("backup.py")), "prune-offsite",
                "--backend-dir", str(root), "--environment-file", str(self.runtime.environment_file),
                "--backup-dir", str(self.runtime.backups), "--transfer-bin", str(executable), "--cos-config", str(self.credentials)]
        with subprocess.Popen(args, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE) as process:
            (root / "owned-parent-pid").write_text(str(process.pid))
            process.communicate(timeout=10)
            self.assertEqual(process.returncode, -9)
        for _ in range(500):
            if (root / "controlled-child-finished").exists(): break
            time.sleep(.01)
        self.assertTrue((root / "controlled-child-finished").exists())
        self.assertEqual(len(json.loads((root / "controlled-removed.json").read_text())), 1)
        self.assertEqual(len(list(self.runtime.backups.glob("albums-offsite-*.json"))), 3)
        completed = subprocess.run(args, env=env, capture_output=True, text=True, timeout=10)
        self.assertEqual(completed.returncode, 0, completed.stdout)
        result = json.loads(completed.stdout)
        self.assertEqual([r["remote"]["VersionsRemoved"] for r in result["results"]], [0, 1, 1])
        self.assertFalse(result["account_erasure_complete"])

    def test_audit_includes_expired_confirmed_and_active_unconfirmed_intents_without_mutation(self):
        with patch("offsite.move", side_effect=self.move):
            published = self.publish()
        confirmed = json.loads(Path(published["receipt"]).read_text())["receipt"]["remote"]
        # An expired confirmed publication and an active uncertain upload must both be audited.
        path = Path(published["receipt"])
        payload = json.loads(path.read_text())["receipt"]
        payload["snapshot_created_at"] -= RETENTION
        atomic_json(path, dict(receipt=payload, hmac_sha256=manifest_signature(backup_secret(self.runtime), payload)))
        with patch("offsite.move", side_effect=OperationError("uncertain")):
            with self.assertRaises(OperationError): self.publish()
        pending = next(json.loads(p.read_text())["receipt"]["remote"] for p in self.runtime.backups.glob("albums-offsite-*.json") if p != path)
        unknown = "albums/recovery/" + "f" * 32 + ".enc"
        inventory = self.audit_inventory([
            dict(Key=confirmed["Key"], Version=confirmed["Version"], Bytes=confirmed["Bytes"], DeleteMarker=False),
            dict(Key=unknown, Version="deleted", Bytes=0, DeleteMarker=True)],
            [dict(Key=pending["Key"], UploadID="in-progress")])
        before = {p: file_digest(p) for p in self.runtime.backups.iterdir()}
        result = self.run_audit(inventory)
        self.assertEqual(result["receipts"], 2)
        self.assertEqual(result["unregistered_keys"], [unknown])
        self.assertEqual(result["unconfirmed_publications"], [pending["Key"]])
        self.assertEqual(result["missing_confirmed_bundle_versions"], [])
        self.assertEqual(result["missing_remote_receipts"], [confirmed["Key"]])
        self.assertTrue(result["has_unresolved_items"])
        self.assertFalse(result["writers_quiescent"])
        self.assertFalse(result["account_erasure_complete"])
        self.assertEqual(before, {p: file_digest(p) for p in self.runtime.backups.iterdir()})

    def test_audit_empty_listing_never_proves_erasure(self):
        result = self.run_audit(self.audit_inventory())
        self.assertFalse(result["has_unresolved_items"])
        self.assertFalse(result["writers_quiescent"])
        self.assertFalse(result["account_erasure_complete"])

    def test_failed_audit_preserves_intents_and_previous_report(self):
        self.run_audit(self.audit_inventory())
        report = self.runtime.state / "albums-offsite-audit.json"
        previous = report.read_bytes()
        with patch("offsite.move", side_effect=OperationError("uncertain")):
            with self.assertRaises(OperationError): self.publish()
        path = next(self.runtime.backups.glob("albums-offsite-*.json"))
        intent = path.read_bytes()
        with patch.object(self.runtime, "execute", side_effect=OperationError("interrupted inventory")):
            with self.assertRaises(OperationError): audit(self.runtime, "/controlled/bin", self.credentials)
        self.assertEqual(path.read_bytes(), intent)
        self.assertEqual(report.read_bytes(), previous)

    def test_audit_cli_uses_operations_lock_and_private_record(self):
        executable = self.runtime.root / "controlled-inventory"
        executable.write_text("#!/usr/bin/env python3\nimport json,sys\nr=json.load(sys.stdin)\n"
                              "assert r['Operation']=='inventory' and r['Key']=='albums/recovery/'\n"
                              "print(" + repr(json.dumps(self.audit_inventory())) + ")\n")
        executable.chmod(0o700)
        command = [sys.executable, str(Path(__file__).with_name("backup.py")), "audit-offsite",
                   "--backend-dir", str(self.runtime.root), "--environment-file", str(self.runtime.environment_file),
                   "--backup-dir", str(self.runtime.backups), "--transfer-bin", str(executable),
                   "--cos-config", str(self.credentials)]
        with self.runtime.lock():
            blocked = subprocess.run(command, capture_output=True, timeout=10)
        self.assertEqual(blocked.returncode, 1, blocked.stderr)
        self.assertFalse((self.runtime.state / "albums-offsite-audit.json").exists())
        completed = subprocess.run(command, capture_output=True, timeout=10)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        result = json.loads(completed.stdout)
        self.assertTrue(result["passed"])
        self.assertFalse(result["account_erasure_complete"])
        record = self.runtime.state / "albums-offsite-audit.json"
        self.assertEqual(record.stat().st_mode & 0o077, 0)
        self.assertNotIn("private-fixture", record.read_text())

    def test_audit_reports_missing_and_unexpected_bundle_versions(self):
        with patch("offsite.move", side_effect=self.move): published = self.publish()
        remote = json.loads(Path(published["receipt"]).read_text())["receipt"]["remote"]
        result = self.run_audit(self.audit_inventory([
            dict(Key=remote["Key"], Version="unexpected", Bytes=remote["Bytes"] + 1, DeleteMarker=False),
            dict(Key=remote["Key"].removesuffix(".enc") + ".receipt.json", Version="receipt-version", Bytes=800, DeleteMarker=False)]))
        self.assertEqual(result["missing_confirmed_bundle_versions"], [remote["Key"]])
        self.assertEqual(result["missing_remote_receipts"], [])
        self.assertEqual(len(result["unexpected_bundle_versions"]), 1)
        self.assertEqual(len(result["bundle_size_mismatches"]), 1)

    def test_audit_rejects_unauthenticated_private_or_wrong_destination_intents_before_listing(self):
        with patch("offsite.move", side_effect=self.move): published = self.publish()
        path = Path(published["receipt"])
        original = path.read_bytes()
        for mode in ("tamper", "foreign-database", "foreign-bucket", "wrong-key", "public", "symlink", "fifo", "directory", "oversized", "bad-filename"):
            with self.subTest(mode=mode), patch.object(self.runtime, "execute") as execute:
                path.write_bytes(original)
                path.chmod(0o600)
                if mode in ("tamper", "foreign-database", "foreign-bucket", "wrong-key"):
                    envelope = json.loads(original)
                    r = envelope["receipt"]
                    if mode == "tamper": r["remote"]["Bytes"] += 1
                    elif mode == "foreign-database": r["database"] = "other_test"
                    elif mode == "foreign-bucket": r["remote"]["Bucket"] = "other-1250000000"
                    else: r["remote"]["Key"] = "albums/recovery/" + "e" * 32 + ".enc"
                    if mode != "tamper": envelope["hmac_sha256"] = manifest_signature(backup_secret(self.runtime), r)
                    atomic_json(path, envelope)
                elif mode == "public": path.chmod(0o644)
                elif mode == "oversized": path.write_bytes(b"x" * 65537)
                elif mode == "bad-filename": path.rename(path.with_name("albums-offsite-unknown.json"))
                else:
                    path.unlink()
                    if mode == "symlink": path.symlink_to(self.snapshot)
                    elif mode == "fifo": os.mkfifo(path)
                    else: path.mkdir()
                with self.assertRaises(OperationError): audit(self.runtime, "/controlled/bin", self.credentials)
                execute.assert_not_called()
                if path.is_dir(): path.rmdir()
                elif path.exists() or path.is_symlink(): path.unlink()
                unknown = path.with_name("albums-offsite-unknown.json")
                if unknown.exists(): unknown.unlink()
        path.write_bytes(original)
        path.chmod(0o600)

    def test_audit_transport_contract_rejects_partial_foreign_or_invalid_results(self):
        key = "albums/recovery/" + "a" * 32 + ".enc"
        for mode in ("missing-uploads", "foreign-bucket", "foreign-prefix", "null-versions", "extra-field", "bad-key", "bad-version", "bool-bytes", "marker-size", "duplicate-version", "bad-upload", "duplicate-upload", "too-many-versions"):
            with self.subTest(mode=mode):
                inventory = self.audit_inventory()
                v = dict(Key=key, Version="v1", Bytes=1, DeleteMarker=False)
                if mode == "missing-uploads": del inventory["Uploads"]
                elif mode == "foreign-bucket": inventory["Bucket"] = "other-1250000000"
                elif mode == "foreign-prefix": inventory["Prefix"] = "albums/"
                elif mode == "null-versions": inventory["Versions"] = None
                elif mode == "extra-field": inventory["Complete"] = True
                elif mode == "too-many-versions": inventory["Versions"] = [v] * 10001
                elif mode in ("bad-upload", "duplicate-upload"):
                    inventory["Uploads"] = [dict(Key=key, UploadID="null" if mode == "bad-upload" else "id")] * (2 if mode == "duplicate-upload" else 1)
                else:
                    if mode == "bad-key": v["Key"] = "health/private"
                    elif mode == "bad-version": v["Version"] = "null"
                    elif mode == "bool-bytes": v["Bytes"] = True
                    elif mode == "marker-size": v["DeleteMarker"] = True
                    inventory["Versions"] = [v] * (2 if mode == "duplicate-version" else 1)
                with self.assertRaises(OperationError): self.run_audit(inventory)

    def test_restore_receipt_still_requires_pinned_digest_and_age(self):
        with patch("offsite.move", side_effect=self.move): published = self.publish()
        for kwargs in ({"digest": None}, {"digest": published["receipt_sha256"], "expired": None}):
            with self.assertRaises(OperationError): read_receipt(self.runtime, published["receipt"], **kwargs)

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

    def test_session_write_failure_never_sends_payload(self):
        writes=[]
        def persist(path,envelope):
            if "receipt" not in envelope: return atomic_json(path,envelope)
            writes.append(envelope["receipt"]["remote"]["UploadID"])
            if envelope["receipt"]["remote"]["UploadID"]: raise OSError("session checkpoint write failed")
            atomic_json(path,envelope)
        with patch("offsite.atomic_json",side_effect=persist),patch("offsite.move") as transfer:
            with self.assertRaises(OSError): self.publish()
            transfer.assert_not_called()
        path=next(self.runtime.backups.glob("albums-offsite-*.json"))
        self.assertEqual(json.loads(path.read_text())["receipt"]["remote"]["UploadID"],"")
        self.assertEqual(writes,["","bundle-session"])
        self.assertEqual(list(self.runtime.backups.glob(".albums-offsite-*")),[])

    def test_initiation_response_loss_retains_preinitiation_intent(self):
        with patch("offsite.initiate",side_effect=OperationError("unknown session")),patch("offsite.move") as transfer:
            with self.assertRaises(OperationError): self.publish()
            transfer.assert_not_called()
        path=next(self.runtime.backups.glob("albums-offsite-*.json"))
        r=json.loads(path.read_text())["receipt"]
        self.assertEqual(r["upload_protocol"],MULTIPART_PROTOCOL)
        self.assertEqual(r["remote"]["UploadID"],"")
        self.assertFalse(r["publication_confirmed"])

    def test_initiation_transport_requires_exact_binding(self):
        target=dict(Bucket="test-1250000000",Region="ap-guangzhou")
        key="albums/recovery/"+"a"*32+".enc"
        for mode in ("ok","foreign-key","foreign-bucket","null-id","missing-id","extra"):
            with self.subTest(mode=mode):
                result=dict(Key=key,UploadID="bound-session",**target)
                if mode=="foreign-key": result["Key"]+="other"
                elif mode=="foreign-bucket": result["Bucket"]="foreign-1250000000"
                elif mode=="null-id": result["UploadID"]="null"
                elif mode=="missing-id": del result["UploadID"]
                elif mode=="extra": result["Version"]="v1"
                def execute(label,args,*,input,timeout):
                    self.assertEqual(label,"albums-backup-initiate")
                    self.assertEqual(json.loads(input),dict(Operation="initiate",Key=key,**target))
                    return json.dumps(result).encode()
                with patch.object(self.runtime,"execute",side_effect=execute):
                    if mode=="ok": self.assertEqual(initiate(self.runtime,"bin",self.credentials,key,target),"bound-session")
                    else:
                        with self.assertRaises(OperationError): initiate(self.runtime,"bin",self.credentials,key,target)

    def test_legacy_receipt_can_restore_but_cannot_be_republished(self):
        with patch("offsite.move",side_effect=self.move): result=self.publish()
        path=Path(result["receipt"])
        r=json.loads(path.read_text())["receipt"]
        r.pop("upload_protocol");r.pop("receipt_upload_id");r["remote"].pop("UploadID")
        atomic_json(path,dict(receipt=r,hmac_sha256=manifest_signature(backup_secret(self.runtime),r)))
        digest=file_digest(path)
        self.assertEqual(read_receipt(self.runtime,path,digest),r)
        with patch("offsite.move") as move,patch("offsite.initiate") as start:
            with self.assertRaises(OperationError):publish_receipt(self.runtime,"bin",self.credentials,path,digest)
            move.assert_not_called();start.assert_not_called()
        with patch("offsite.move",side_effect=self.move):
            restored=fetch(self.runtime,"bin",self.credentials,path,digest)
        self.assertFalse(restored["recovery_ready"])

    def test_confirmation_write_failure_preserves_preupload_intent(self):
        writes = []
        def persist(path, data):
            if "receipt" not in data: return atomic_json(path,data)
            writes.append(data["receipt"]["publication_confirmed"])
            if data["receipt"]["publication_confirmed"]:
                raise OSError("controlled confirmation disk failure")
            atomic_json(path, data)
        with patch("offsite.atomic_json", side_effect=persist), patch("offsite.move", side_effect=self.move):
            with self.assertRaises(OSError): self.publish()
        self.assertEqual(writes, [False, False, True])
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
        receipt.pop("upload_protocol")
        receipt.pop("receipt_upload_id")
        receipt["remote"].pop("UploadID")
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
            self.assertEqual(kwargs, {"Bucket": "test-1250000000", "Region": "ap-guangzhou", "UploadID": "bundle-session"})
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
            return json.dumps(dict(Empty=True, VersionsRemoved=1, UploadsEmpty=True, SessionsClosed=1, **{k: request[k] for k in ("Key", "Bucket", "Region")})).encode()
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

    def test_fetch_failure_and_cancellation_reclaim_only_owned_staging(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        originals = {p: p.read_bytes() for p in (self.snapshot, self.ledger, self.remote)}
        for error in (RuntimeError, KeyboardInterrupt):
            def interrupted(runtime, source, destination, **kwargs):
                destination.write_bytes(b"partial synthetic plaintext")
                self.assertEqual(destination.stat().st_mode & 0o777, 0o600)
                raise error("controlled decoding interruption")
            with self.subTest(error=error), patch("offsite.move", side_effect=self.move), patch("offsite.crypt", side_effect=interrupted):
                with self.assertRaises(error):
                    fetch(self.runtime, "bin", "config", receipt["receipt"], receipt["receipt_sha256"])
            self.assertEqual(list(self.runtime.backups.glob(".albums-offsite-*")), [])
            self.assertEqual(list(self.runtime.backups.glob("albums-recovered-*")), [])
            for path, contents in originals.items(): self.assertEqual(path.read_bytes(), contents)

    def test_fetch_disk_budget_rejects_before_download(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        with patch("offsite.shutil.disk_usage", return_value=shutil._ntuple_diskusage(1, 1, 0)), patch("offsite.move") as transfer:
            with self.assertRaisesRegex(OperationError, "insufficient space"):
                fetch(self.runtime, "bin", "config", receipt["receipt"], receipt["receipt_sha256"])
            transfer.assert_not_called()
        self.assertEqual(list(self.runtime.backups.glob(".albums-offsite-*")), [])

    def test_fetch_sigkill_recovers_staging_and_preserves_completed_handoff(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        originals = {p: p.read_bytes() for p in (self.snapshot, self.ledger, self.remote)}
        code = r"""
import os,shutil,signal,sys
from pathlib import Path
from unittest.mock import patch
import offsite
from backup import Runtime,configure
mode,root,vault,env,remote,receipt,digest=sys.argv[1:]
r=Runtime(root,vault,env);configure(r)
def stop():os.kill(os.getpid(),signal.SIGKILL)
def move(runtime,executable,credentials,operation,path,key,sha,size,**pinned):
    assert operation=='get'
    shutil.copyfile(remote,path);path.chmod(0o600)
    if mode=='download':stop()
    return dict(Key=key,SHA256=sha,Bytes=size,**pinned)
crypt,verify=offsite.crypt,offsite.verify
def decode(*args,**kwargs):
    crypt(*args,**kwargs)
    if mode=='decode':stop()
def checked(*args,**kwargs):
    result=verify(*args,**kwargs)
    if mode=='extracted':stop()
    return result
with r.lock(),patch('offsite.move',side_effect=move),patch('offsite.crypt',side_effect=decode),patch('offsite.verify',side_effect=checked),patch('offsite._remove_recovered_owner',side_effect=lambda *a:stop()):
    offsite.fetch(r,'controlled','controlled',receipt,digest)
"""
        for mode in ("download", "decode", "extracted", "handoff"):
            with self.subTest(mode=mode):
                exited = subprocess.run([sys.executable, "-c", code, mode, str(self.runtime.root), str(self.runtime.backups),
                                         str(self.runtime.environment_file), str(self.remote), receipt["receipt"], receipt["receipt_sha256"]],
                                        capture_output=True, timeout=20)
                self.assertEqual(exited.returncode, -signal.SIGKILL, exited.stderr)
                stages = list(self.runtime.backups.glob(".albums-offsite-*"))
                recovered = list(self.runtime.backups.glob("albums-recovered-*"))
                self.assertEqual(len(stages), 0 if mode == "handoff" else 1)
                self.assertEqual(len(recovered), 1 if mode == "handoff" else 0)
                with self.runtime.lock(): self.assertEqual(prune(self.runtime), 0)
                self.assertEqual(list(self.runtime.backups.glob(".albums-offsite-*")), [])
                if mode == "handoff":
                    self.assertEqual({p.name for p in recovered[0].iterdir()}, {self.snapshot.name, self.snapshot.name + ".json", self.ledger.name})
                for path, contents in originals.items(): self.assertEqual(path.read_bytes(), contents)

    def test_handoff_owner_authentication_and_active_lease_preserve_recovery_files(self):
        with patch("offsite.move", side_effect=self.move): receipt = self.publish()
        with patch("offsite.move", side_effect=self.move), patch("offsite._remove_recovered_owner", side_effect=OperationError("controlled handoff acknowledgement loss")):
            with self.assertRaises(OperationError):
                fetch(self.runtime, "bin", "config", receipt["receipt"], receipt["receipt_sha256"])
        folder, = self.runtime.backups.glob("albums-recovered-*")
        owner = folder / "owner.json"; original = owner.read_bytes()
        contents = {p.name: p.read_bytes() for p in folder.iterdir() if p != owner}
        for mode in ("signature", "destination", "scope", "files"):
            envelope = json.loads(original)
            if mode in ("signature", "destination"): envelope["owner"]["destination"] = "albums-recovered-00000000"
            elif mode == "scope": envelope["owner"]["database"] = "other_app"
            else: envelope["owner"]["files"][-1] = "unexpected-file"
            if mode != "signature": envelope["hmac_sha256"] = manifest_signature(backup_secret(self.runtime), envelope["owner"])
            atomic_json(owner, envelope)
            with self.subTest(mode=mode), self.assertRaises(OperationError): prune(self.runtime)
        owner.write_bytes(original)
        with owner.open("rb") as active:
            fcntl.flock(active, fcntl.LOCK_SH | fcntl.LOCK_NB)
            with self.assertRaises(OperationError): prune(self.runtime)
        self.assertEqual({p.name: p.read_bytes() for p in folder.iterdir() if p != owner}, contents)
        self.assertEqual(prune(self.runtime), 0)
        self.assertFalse(owner.exists())
        self.assertEqual({p.name: p.read_bytes() for p in folder.iterdir()}, contents)

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
        self.assertEqual(len(self.initiations),2)

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
        result.update(Empty=True, VersionsRemoved=4, UploadsEmpty=True, SessionsClosed=2)
        def execute(name, command, *, input, timeout):
            request = json.loads(input)
            self.assertEqual(name, "albums-backup-expire")
            self.assertEqual(command, ["bin", "cleanup-config"])
            self.assertEqual(request, dict(Operation="expire", ReceiptSHA256=receipt["receipt_sha256"],
                                          ReceiptBytes=path.stat().st_size,
                                          Protocol=MULTIPART_PROTOCOL, UploadID=remote["UploadID"], ReceiptUploadID="receipt-session",
                                          **{k: remote[k] for k in ("Key", "Bucket", "Region", "Bytes", "SHA256")}))
            return json.dumps(result).encode()
        with patch("offsite.time.time", return_value=self.manifest["created_at"] + RETENTION), patch.object(self.runtime, "execute", side_effect=execute):
            completed = expire(self.runtime, "bin", "cleanup-config", path, receipt["receipt_sha256"])
            self.assertFalse(completed["account_erasure_complete"])
            for key, value in (("Empty", False), ("VersionsRemoved", True), ("VersionsRemoved", 10001), ("Bucket", "other"), ("UploadsEmpty",False), ("SessionsClosed",True)):
                previous = result[key]
                result[key] = value
                with self.assertRaises(OperationError):
                    expire(self.runtime, "bin", "cleanup-config", path, receipt["receipt_sha256"])
                result[key] = previous
        self.assertTrue(path.exists())
        self.assertTrue(self.snapshot.exists())
        self.assertTrue(self.ledger.exists())

    def test_recovered_expiry_uses_original_age_and_preserves_newer_independent_ledger(self):
        folder = self.recovered()
        independent = self.runtime.backups / ("albums-deletions-" + "3" * 32 + ".json")
        payload = dict(scope=LEDGER_SCOPE, database="albums_test", exported_at=self.manifest["created_at"] + 60, entries=[], assets=[], collections=[])
        atomic_json(independent, dict(payload=payload, hmac_sha256=manifest_signature(backup_secret(self.runtime), payload)))
        deadline = self.manifest["created_at"] + RETENTION
        with patch("backup.time.time", return_value=deadline - 1):
            self.assertEqual(prune(self.runtime), 0)
        self.assertTrue(folder.exists())
        with patch("backup.time.time", return_value=deadline):
            self.assertEqual(prune(self.runtime), 3)
            self.assertEqual(prune(self.runtime), 0)
        self.assertFalse(folder.exists())
        self.assertFalse(self.ledger.exists())
        self.assertTrue(independent.exists())
        self.assertTrue(self.remote.exists())

    def test_prior_v2_recovered_copies_expire_without_becoming_restore_authority(self):
        from deletion_ledger import verify as verify_ledger
        folder = self.recovered(); path = folder / self.ledger.name
        envelope = json.loads(path.read_text()); payload = envelope['payload']
        payload['scope'] = 'albums-deletion-ledger-v2'; payload.pop('collections')
        envelope['hmac_sha256'] = manifest_signature(backup_secret(self.runtime), payload)
        atomic_json(path, envelope)
        with self.assertRaises(OperationError):
            verify_ledger(self.runtime, path, file_digest(path), 0)
        with patch('backup.time.time', return_value=self.manifest['created_at'] + RETENTION):
            prune(self.runtime)
        self.assertFalse(folder.exists())

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
            self.assertEqual(prune(self.runtime), 3)
        self.assertFalse(folder.exists())
        self.assertFalse(self.ledger.exists())

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
