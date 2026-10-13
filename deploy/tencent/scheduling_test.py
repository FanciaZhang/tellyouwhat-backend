import contextlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from ops_common import Runtime
import operations
import release


class SchedulingTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        (self.root / ".env.production").write_text("")
        self.runtime = Runtime(self.root)

    def test_reconciliation_does_not_acquire_backup_lock(self):
        with patch("sys.argv", ["release.py", "maintain"]), patch("release.Runtime", return_value=self.runtime), \
                patch.object(self.runtime, "lock", return_value=contextlib.nullcontext()) as lock, \
                patch("blue_green.maintain", return_value={"passed": True}):
            self.assertEqual(release.main(), 0)
        lock.assert_called_once_with("deployment", wait_seconds=0)

    def test_backup_waits_for_transient_operations_contention(self):
        with patch("sys.argv", ["operations.py", "backup"]), patch("operations.Runtime", return_value=self.runtime), \
                patch.object(self.runtime, "lock", return_value=contextlib.nullcontext()) as lock, \
                patch("operations.create_backup", return_value={"passed": True}):
            self.assertEqual(operations.main(), 0)
        lock.assert_called_once_with(wait_seconds=600)

    def test_database_cleanup_failure_does_not_skip_host_cleanup(self):
        from types import SimpleNamespace
        from ops_common import OperationError
        with patch("operations.prune_backups", return_value=0), \
                patch.object(self.runtime, "execute", side_effect=OperationError("database cleanup failed")), \
                patch("operations.cleanup") as cleanup:
            with self.assertRaisesRegex(OperationError, "database cleanup failed"):
                operations.operate(self.runtime, SimpleNamespace(operation="maintenance"))
        cleanup.assert_called_once_with(self.runtime, cache=True)
        self.assertFalse((self.runtime.state / "maintenance.json").exists())

    def test_failed_backup_attempt_is_recorded_and_a_successful_retry_clears_it(self):
        from ops_common import OperationError
        failure = self.runtime.state / "backup-failure.json"
        with patch("sys.argv", ["operations.py", "backup"]), patch("operations.Runtime", return_value=self.runtime), \
                patch("operations.create_backup", side_effect=OperationError("unavailable")):
            self.assertEqual(operations.main(), 1)
        self.assertFalse(json.loads(failure.read_text())["passed"])
        with patch("sys.argv", ["operations.py", "backup"]), patch("operations.Runtime", return_value=self.runtime), \
                patch("operations.create_backup", return_value={"passed": True}):
            self.assertEqual(operations.main(), 0)
        self.assertFalse(failure.exists())
