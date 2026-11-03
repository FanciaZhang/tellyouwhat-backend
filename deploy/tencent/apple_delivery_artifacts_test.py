import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from apple_delivery_artifacts import cleanup_packages
from ops_common import OperationError
from ops_artifacts import cleanup


class AppleDeliveryCleanupTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.state = self.root / ".operations"
        (self.state / "artifact-owners").mkdir(parents=True)
        self.data = self.root / ".apple-delivery/data"
        self.data.mkdir(parents=True, mode=0o700)
        self.image = "tellyouwhat-apple-development:" + "a" * 40
        self.runtime = SimpleNamespace(root=self.root, state=self.state,
                                       execute=Mock(return_value=b'{"grants_removed":1,"packages_removed":2}'))

    def configure(self):
        (self.state / "apple-delivery.json").write_text(json.dumps({"image": self.image}))
        (self.state / "artifact-owners/apple-development.json").write_text(json.dumps({"current": {"images": [self.image]}}))

    def test_unconfigured_host_does_not_run_a_container(self):
        self.assertEqual(cleanup_packages(self.runtime), {"configured": False})
        self.runtime.execute.assert_not_called()

    def test_cleanup_is_scoped_to_registered_image_and_private_store(self):
        self.configure()
        self.assertEqual(cleanup_packages(self.runtime), {"configured": True, "grants_removed": 1, "packages_removed": 2})
        command = self.runtime.execute.call_args.args[1]
        self.assertIn(self.image, command)
        self.assertIn(f"type=bind,source={self.data},target=/data", command)
        self.assertEqual(command[command.index("--network") + 1], "none")
        self.assertEqual(command[command.index("--user") + 1], f"{os.geteuid()}:{os.getegid()}")
        self.assertIn("--read-only", command)

    def test_missing_state_or_wrong_registration_fails_before_execution(self):
        self.configure()
        (self.state / "apple-delivery.json").unlink()
        with self.assertRaises(OperationError): cleanup_packages(self.runtime)
        self.configure()
        (self.state / "artifact-owners/apple-development.json").write_text('{"current":{"images":["production:keep"]}}')
        with self.assertRaises(OperationError): cleanup_packages(self.runtime)
        self.runtime.execute.assert_not_called()

    def test_invalid_image_permissions_symlink_and_output_fail_closed(self):
        self.configure()
        (self.state / "apple-delivery.json").write_text('{"image":"production:keep"}')
        with self.assertRaises(OperationError): cleanup_packages(self.runtime)
        self.configure()
        self.data.chmod(0o755)
        with self.assertRaises(OperationError): cleanup_packages(self.runtime)
        self.data.chmod(0o700)
        self.runtime.execute.assert_not_called()
        with tempfile.TemporaryDirectory() as outside:
            self.data.rmdir()
            self.data.symlink_to(outside, target_is_directory=True)
            with self.assertRaises(OperationError): cleanup_packages(self.runtime)
            self.data.unlink()
            self.data.mkdir(mode=0o700)
        self.runtime.execute.assert_not_called()
        self.runtime.execute.return_value = b'{"grants_removed":true,"packages_removed":2}'
        with self.assertRaises(OperationError): cleanup_packages(self.runtime)
        self.runtime.execute.return_value = b'[]'
        with self.assertRaises(OperationError): cleanup_packages(self.runtime)

    def test_scheduled_cleanup_reports_delivery_failures_and_respects_dry_run_and_owner(self):
        self.runtime.record = Mock(side_effect=lambda name, **result: result)
        with patch('ops_artifacts.inspect_host', return_value=([], [])), \
                patch('ops_artifacts.read_state', return_value={}), \
                patch('ops_artifacts.plan_cleanup', return_value={'files': [], 'images': []}), \
                patch('apple_delivery_artifacts.cleanup_packages', side_effect=OperationError('fixture failure')) as collect:
            self.assertTrue(cleanup(self.runtime, apply=False)['dry_run'])
            self.assertTrue(cleanup(self.runtime, 'production')['passed'])
            collect.assert_not_called()
            for owner in (None, 'apple-development'):
                with self.assertRaisesRegex(OperationError, 'Apple delivery cleanup failed'):
                    cleanup(self.runtime, owner)
                self.assertFalse(self.runtime.record.call_args.kwargs['passed'])
            self.assertEqual(collect.call_count, 2)


if __name__ == "__main__": unittest.main()
