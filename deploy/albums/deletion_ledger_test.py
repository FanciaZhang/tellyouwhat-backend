import datetime
import hashlib
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

from backup import Runtime, configure
from deletion_ledger import publish, verify, validate, SCOPE, MAX_RECORDS
from ops_backup import backup_secret, manifest_signature
from ops_common import OperationError, atomic_json


class DeletionLedgerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="album-ledger-test-")
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        env = root / "backup.env"
        env.write_text("MYSQL_DATABASE=albums_fixture\nALBUM_BACKUP_ENCRYPTION_KEY=" + "b" * 64 + "\n")
        env.chmod(0o600)
        self.runtime = Runtime(root, root / "vault", env)
        configure(self.runtime)
        now = datetime.datetime.now(datetime.timezone.utc)
        self.entries = [["10000000-0000-4000-8000-000000000001", "20000000-0000-4000-8000-000000000001", "aa" * 32, now.strftime("%Y-%m-%dT%H:%M:%S.%fZ")]]
        self.payload = {"scope": SCOPE, "database": "albums_fixture", "exported_at": int(now.timestamp()), "entries": self.entries}
        self.path = self.runtime.backups / "ledger.json"

    def write(self):
        atomic_json(self.path, {"payload": self.payload, "hmac_sha256": manifest_signature(backup_secret(self.runtime), self.payload)})
        return hashlib.sha256(self.path.read_bytes()).hexdigest()

    def test_export_is_private_authenticated_and_explicitly_local(self):
        with patch.object(self.runtime, "execute", return_value=("\t".join(self.entries[0]) + "\n").encode()):
            result = publish(self.runtime, ["mysql-fixture"], {})
        path = self.runtime.backups / result["filename"]
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assertFalse(result["offsite"])
        self.assertEqual(verify(self.runtime, path, result["sha256"], 0), self.entries)

    def test_pin_and_authentication_are_both_required(self):
        digest = self.write()
        with self.assertRaises(OperationError): verify(self.runtime, self.path, "00" * 32, 0)
        with self.assertRaises(OperationError): verify(self.runtime, self.path, None, 0)
        envelope = json.loads(self.path.read_text())
        envelope["payload"]["entries"] = []
        self.path.write_text(json.dumps(envelope))
        changed = hashlib.sha256(self.path.read_bytes()).hexdigest()
        with self.assertRaises(OperationError): verify(self.runtime, self.path, digest, 0)
        with self.assertRaisesRegex(OperationError, "authentication"): verify(self.runtime, self.path, changed, 0)

    def test_scope_database_and_snapshot_order_reject_even_signed_input(self):
        original = json.loads(json.dumps(self.payload))
        for field, value in (("scope", "other"), ("database", "other"), ("exported_at", int(time.time()) + 3600)):
            self.payload = dict(original, **{field: value})
            with self.assertRaises(OperationError): verify(self.runtime, self.path, self.write(), 0)
        self.payload = original
        with self.assertRaises(OperationError): verify(self.runtime, self.path, self.write(), original["exported_at"] + 1)

    def test_ambiguous_rows_cannot_reach_sql(self):
        for position, value in ((0, "' OR 1=1 --"), (1, "00000000-0000-0000-0000-000000000000"), (2, "abc"), (3, "2026-02-30T00:00:00.000000Z")):
            rows = [self.entries[0].copy()]
            rows[0][position] = value
            with self.assertRaises(OperationError): validate(rows)
        with self.assertRaises(OperationError): validate(self.entries * 2)
        with self.assertRaises(OperationError): validate(self.entries * (MAX_RECORDS + 1))
        with self.assertRaises(OperationError): validate([self.entries[0] + ["extra"]])

    def test_private_file_and_no_symlinks(self):
        digest = self.write()
        self.path.chmod(0o644)
        with self.assertRaises(OperationError): verify(self.runtime, self.path, digest, 0)
        self.path.chmod(0o600)
        link = self.path.with_name("link.json")
        link.symlink_to(self.path)
        with self.assertRaises(OperationError): verify(self.runtime, link, digest, 0)


if __name__ == "__main__":
    unittest.main()
