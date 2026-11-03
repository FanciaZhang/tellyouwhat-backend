import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from ops_common import OperationError
from ops_health import backup_valid


class BackupHealthTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.runtime = Mock(root=self.root / "backend", state=self.root / "state", backups=self.root / "backups")
        self.runtime.state.mkdir()
        self.record = {"filename": "mysql-20261013T035318Z-abcdef.sql.gz.enc", "sha256": "digest"}
        self.manifest = {**self.record, "recovery_filename": "recovery-pair.tar.gz.enc"}

    def test_tampered_backup_is_not_healthy_even_when_file_exists(self):
        with patch("ops_backup.verify_backup", side_effect=OperationError("invalid")):
            with self.assertRaises(OperationError):
                backup_valid(self.runtime, self.record)

    def test_required_offsite_backup_needs_matching_complete_readback_record(self):
        config = self.root / "backup/cos.env"
        config.parent.mkdir()
        config.touch()
        remote = {**self.record, "passed": True, "verified_objects": 3, "recovery_filename": self.manifest["recovery_filename"]}
        with patch("ops_backup.verify_backup", return_value=self.manifest):
            for changes, expected in [({}, True), ({"filename": "older"}, False), ({"verified_objects": 2}, False), ({"passed": False}, False)]:
                (self.runtime.state / "offsite.json").write_text(json.dumps({**remote, **changes}))
                self.assertEqual(backup_valid(self.runtime, self.record), expected)

    def test_required_offsite_config_cannot_disappear_silently(self):
        (self.runtime.state / "offsite-required").touch()
        with patch("ops_backup.verify_backup", return_value=self.manifest):
            self.assertFalse(backup_valid(self.runtime, self.record))


if __name__ == "__main__":
    unittest.main()
