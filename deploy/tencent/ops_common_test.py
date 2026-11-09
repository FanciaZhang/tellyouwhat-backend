from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from ops_common import OperationError, Runtime


class RuntimeLockTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / ".env.production").write_text("")
        self.runtime = Runtime(self.root, self.root)

    def test_execute_only_inherits_explicit_creator_leases(self):
        result = SimpleNamespace(returncode=0, stdout=b"synthetic result")
        with patch("ops_common.subprocess.run", return_value=result) as run:
            self.assertEqual(self.runtime.execute("controlled", ["controlled"]), b"synthetic result")
            self.assertNotIn("pass_fds", run.call_args.kwargs)
            self.runtime._child_pass_fds = (101,)
            self.runtime.execute("controlled", ["controlled"])
            self.assertEqual(run.call_args.kwargs["pass_fds"], (101,))

    def test_lock_waits_for_temporary_contention(self):
        with patch("ops_common.fcntl.flock", side_effect=[BlockingIOError(), None]) as flock, \
                patch("ops_common.time.monotonic", side_effect=[10.0, 10.1]), \
                patch("ops_common.time.sleep") as sleep:
            with self.runtime.lock("deployment", wait_seconds=1):
                pass
        self.assertEqual(flock.call_count, 2)
        sleep.assert_called_once_with(0.25)

    def test_lock_still_fails_after_bounded_wait(self):
        with patch("ops_common.fcntl.flock", side_effect=BlockingIOError()), \
                patch("ops_common.time.monotonic", side_effect=[10.0, 11.0]), \
                patch("ops_common.time.sleep") as sleep:
            with self.assertRaisesRegex(OperationError, "another deployment operation is running"):
                with self.runtime.lock("deployment", wait_seconds=1):
                    pass
        sleep.assert_not_called()


if __name__ == "__main__":
    unittest.main()
