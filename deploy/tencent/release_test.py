import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from ops_common import OperationError, Runtime, read_environment
from release import KEY_FILES, copy_runtime, deploy, rollback


class ReleaseRecoveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name) / "backend"
        (self.root / "secrets").mkdir(parents=True)
        (self.root / "deploy/tencent").mkdir(parents=True)
        (self.root / ".env.production").write_text(
            "IMAGE_TAG=old\nIMAGE_REGISTRY_PREFIX=registry/app\n"
            "HEALTH_API_DOMAIN=health.example\nJOURNAL_API_DOMAIN=journal.example\n"
            "ADMIN_DOMAIN=admin.example\nPUBLIC_PROXY_MODE=external\nRELEASE_VALUE=old\n"
        )
        (self.root / "compose.production.yaml").write_text("old compose")
        for name in KEY_FILES:
            (self.root / "secrets" / name).write_text("old key")
        self.runtime = Runtime(self.root)
        self.bundle = self.root / ".incoming-new"
        copy_runtime(self.root, self.bundle)
        (self.bundle / "deploy/tencent").mkdir(parents=True)
        environment = self.bundle / ".env.production"
        environment.write_text(environment.read_text().replace("RELEASE_VALUE=old", "RELEASE_VALUE=new"))
        (self.bundle / "compose.production.yaml").write_text("new compose")
        for name in KEY_FILES:
            (self.bundle / "secrets" / name).write_text("new key")

    def run_deploy(self):
        return deploy(self.runtime, "new", "registry/app", "internal", self.bundle, 1)

    def assert_old_runtime(self):
        self.assertEqual(read_environment(self.root / ".env.production")["RELEASE_VALUE"], "old")
        self.assertEqual(read_environment(self.root / ".env.production")["IMAGE_TAG"], "old")
        self.assertEqual((self.root / "compose.production.yaml").read_text(), "old compose")
        self.assertEqual((self.root / "secrets" / KEY_FILES[0]).read_text(), "old key")

    def test_migration_failure_does_not_replace_active_configuration(self):
        def execute(label, *_args, **_kwargs):
            if label == "release-migrate":
                raise OperationError("migration unavailable")
            return b""
        with patch.object(self.runtime, "execute", side_effect=execute), patch("release.internally_ready", return_value=True):
            with self.assertRaisesRegex(OperationError, "migration unavailable"):
                self.run_deploy()
        self.assert_old_runtime()

    def test_readiness_failure_restores_image_environment_and_keys(self):
        with patch.object(self.runtime, "execute", return_value=b"") as execute, \
                patch("release.internally_ready", side_effect=[True, False, True]):
            with self.assertRaisesRegex(OperationError, "previous healthy version restored"):
                self.run_deploy()
        self.assert_old_runtime()
        activations = [call.kwargs["env"]["IMAGE_TAG"] for call in execute.call_args_list if call.args[0] == "release-activate"]
        self.assertEqual(activations, ["new", "old"])
        self.assertTrue(json.loads((self.runtime.state / "release_failure.json").read_text())["previous_restored"])

    def test_successful_release_can_roll_back_and_return_forward(self):
        with patch.object(self.runtime, "execute", return_value=b""), patch("release.internally_ready", return_value=True):
            self.run_deploy()
            self.assertFalse(self.bundle.exists())
            result = rollback(self.runtime, 1)
            self.assertEqual(result["current"]["tag"], "old")
            self.assert_old_runtime()
            result = rollback(self.runtime, 1)
            self.assertEqual(result["current"]["tag"], "new")
            self.assertEqual(read_environment(self.root / ".env.production")["RELEASE_VALUE"], "new")

    def test_rollback_failure_restores_current_healthy_release(self):
        with patch.object(self.runtime, "execute", return_value=b""), patch("release.internally_ready", return_value=True):
            self.run_deploy()
        with patch.object(self.runtime, "execute", return_value=b""), patch("release.internally_ready", side_effect=[False, True]):
            with self.assertRaisesRegex(OperationError, "readiness verification"):
                rollback(self.runtime, 1)
        self.assertEqual(read_environment(self.root / ".env.production")["IMAGE_TAG"], "new")
        self.assertEqual(json.loads((self.runtime.state / "release.json").read_text())["current"]["tag"], "new")


if __name__ == "__main__":
    unittest.main()
