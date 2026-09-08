import json
from pathlib import Path
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from ops_common import Runtime
from ops_health import health, report_health


class OperationalHealthTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / ".env.production").write_text(
            "HEALTH_API_DOMAIN=health.example\nJOURNAL_API_DOMAIN=journal.example\nADMIN_DOMAIN=admin.example\n"
        )
        self.runtime = Runtime(self.root, self.root)
        (self.root / "backup.enc").touch()
        self.runtime.record("backup", filename="backup.enc")
        self.runtime.record("restore")
        self.runtime.record("maintenance")
        disk = patch("ops_health.shutil.disk_usage", return_value=SimpleNamespace(total=100 * 1024**3, free=50 * 1024**3))
        disk.start()
        self.addCleanup(disk.stop)

    def test_disk_pressure_is_unhealthy(self):
        with patch("ops_health.probe", return_value=True), patch(
            "ops_health.shutil.disk_usage", return_value=SimpleNamespace(total=100 * 1024**3, free=1024**3)
        ):
            result = health(self.runtime)
        checks = {value["name"]: value["passed"] for value in result["checks"]}
        self.assertFalse(result["passed"])
        self.assertFalse(checks["disk_space"])

    def test_dependency_failure_is_not_reported_healthy(self):
        with patch("ops_health.probe", side_effect=[True, False, True, True]):
            result = health(self.runtime)
        self.assertFalse(result["passed"])
        self.assertFalse(result["checks"][1]["passed"])

    def test_stale_success_and_missing_backup_are_failures(self):
        self.runtime.record("maintenance", completed_at=int(time.time()) - 31 * 3600)
        (self.root / "backup.enc").unlink()
        with patch("ops_health.probe", return_value=True):
            result = health(self.runtime)
        checks = {value["name"]: value["passed"] for value in result["checks"]}
        self.assertFalse(checks["backup_freshness"])
        self.assertFalse(checks["maintenance_freshness"])
        self.assertTrue(checks["restore_freshness"])

    def test_healthy_services_and_fresh_operations_pass(self):
        with patch("ops_health.probe", return_value=True):
            result = health(self.runtime)
        self.assertTrue(result["passed"])
        self.assertTrue(json.loads((self.runtime.state / "health.json").read_text())["passed"])

    def test_fresh_failure_is_not_success(self):
        self.runtime.record("restore", passed=False)
        with patch("ops_health.probe", return_value=True):
            result = health(self.runtime)
        self.assertFalse(next(c for c in result["checks"] if c["name"] == "restore_freshness")["passed"])

    def test_reports_only_sanitized_health_evidence(self):
        with patch("ops_health.probe", return_value=True):
            result = health(self.runtime)
        with patch.object(self.runtime, "execute") as execute:
            report_health(self.runtime, result)
        payload = json.loads(execute.call_args.kwargs["input"])
        self.assertEqual(set(payload), {"checkedAt", "checks"})
        self.assertNotIn(".env", json.dumps(payload))
        self.assertEqual(execute.call_args.args[1][-2:], ["adminctl", "operations-health"])


if __name__ == "__main__":
    unittest.main()
