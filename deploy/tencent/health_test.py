import json
from pathlib import Path
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from ops_common import Runtime, OperationError
from ops_health import health, report_health, recent_consent_failures
from ops_backup import file_digest, manifest_signature


class OperationalHealthTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / ".env.production").write_text(
            "HEALTH_API_DOMAIN=health.example\nJOURNAL_API_DOMAIN=journal.example\nADMIN_DOMAIN=admin.example\n"
            "MYSQL_DATABASE=fixture\nBACKUP_ENCRYPTION_KEY=" + "x" * 43 + "\n"
        )
        self.runtime = Runtime(self.root, self.root)
        (self.root / "backup.enc").touch()
        digest = file_digest(self.root / "backup.enc")
        manifest = {"filename": "backup.enc", "sha256": digest, "created_at": int(time.time()),
                    "database": "fixture", "table_rows": {"apps": 0}}
        (self.root / "backup.enc.json").write_text(json.dumps({"manifest": manifest,
                    "hmac_sha256": manifest_signature("x" * 43, manifest)}))
        self.runtime.record("backup", filename="backup.enc", sha256=digest)
        self.runtime.record("restore")
        self.runtime.record("maintenance")
        disk = patch("ops_health.shutil.disk_usage", return_value=SimpleNamespace(total=100 * 1024**3, free=50 * 1024**3))
        disk.start()
        self.addCleanup(disk.stop)

    def test_birthday_alias_failure_produces_one_failed_service_check(self):
        self.runtime.config.update(PLATFORM_APPS="health,journal,birthday",
                                   BIRTHDAY_API_HOSTS="birthday.example,birthday-alias.example")
        with patch("ops_health.probe", side_effect=lambda url, host, status: host != "birthday.example"):
            result = health(self.runtime)
        self.assertFalse(result["passed"])
        birthday = [check for check in result["checks"] if check["name"] == "birthday_gateway"]
        self.assertEqual(birthday, [{"name": "birthday_gateway", "passed": False}])
        with patch("ops_health.probe", return_value=True):
            self.assertTrue(health(self.runtime)["passed"])

    def test_albums_failure_is_reported_without_changing_other_service_checks(self):
        self.runtime.config.update(PLATFORM_APPS="health,journal,birthday,albums",
                                   ALBUM_API_HOST="api.albums.tellyouwhat.cn", ALBUM_DEPLOYMENT_ENVIRONMENT="production")
        with patch("ops_health.probe", side_effect=lambda url, host, status, field="status": field != "code"):
            result = health(self.runtime)
        self.assertFalse(result["passed"])
        services = {check["name"]: check["passed"] for check in result["checks"]}
        self.assertFalse(services["albums_gateway"])
        self.assertTrue(all(services[name] for name in ("health_gateway", "journal_gateway", "birthday_gateway")))

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

    def test_failed_latest_attempt_does_not_hide_behind_fresh_prior_success(self):
        self.runtime.record("backup-failure", passed=False)
        with patch("ops_health.probe", return_value=True):
            result = health(self.runtime)
        self.assertFalse(next(c for c in result["checks"] if c["name"] == "backup_freshness")["passed"])

    def test_reports_only_sanitized_health_evidence(self):
        with patch("ops_health.probe", return_value=True):
            result = health(self.runtime)
        with patch.object(self.runtime, "execute") as execute:
            report_health(self.runtime, result)
        payload = json.loads(execute.call_args.kwargs["input"])
        self.assertEqual(set(payload), {"checkedAt", "checks"})
        self.assertNotIn(".env", json.dumps(payload))
        self.assertEqual(execute.call_args.args[1][-2:], ["adminctl", "operations-health"])

    def test_recent_consent_failures_are_scoped_to_the_active_gateway(self):
        events = b'non-json startup line\n' + b'\n'.join(json.dumps(value).encode() for value in [
            {"path": "/v1/privacy/consents", "app_id": "health", "status": 503},
            {"path": "/v1/privacy/consents", "app_id": "journal", "status": 200},
            {"path": "/readyz", "app_id": "journal", "status": 503},
        ])
        with patch('blue_green.compose', return_value=['selected-slot']), patch.object(
                self.runtime, 'execute', side_effect=[b'a' * 64, events]) as execute:
            self.assertEqual(recent_consent_failures(self.runtime, {}), {'health'})
        self.assertEqual(execute.call_args.args[1], ['docker', 'logs', '--since', '5m', '--tail', '5000', 'a' * 64])

    def test_unavailable_or_truncated_request_logs_do_not_pass(self):
        for outputs in ([b''], [b'a' * 64, b'\n' * 5000]):
            with self.subTest(outputs=bool(outputs[0])), patch('blue_green.compose', return_value=[]), patch.object(
                    self.runtime, 'execute', side_effect=outputs), self.assertRaises(OperationError):
                recent_consent_failures(self.runtime, {})

    def test_consent_outage_fails_the_existing_service_alert(self):
        self.runtime.record('deployment_health', passed=True)
        with patch('blue_green.read_state', return_value={'phase': 'STABLE', 'current': {}}), patch(
                'ops_health.probe', return_value=True), patch('ops_health.recent_consent_failures', return_value={'health'}):
            result = health(self.runtime)
        self.assertFalse(result['passed'])
        checks = {c['name']: c['passed'] for c in result['checks']}
        self.assertFalse(checks['health_gateway'])
        self.assertTrue(checks['journal_gateway'])


if __name__ == "__main__":
    unittest.main()
