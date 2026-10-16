import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from ops_common import OperationError, Runtime, read_environment
from release import KEY_FILES, copy_runtime, deploy, rollback, internally_ready, validate, probe


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
        for target, value in (("release.create_backup", {"filename": "fixture.enc", "sha256": "fixture"}),
                              ("release.verify_backup", {})):
            mock = patch(target, return_value=value)
            mock.start()
            self.addCleanup(mock.stop)
        self.bundle = self.root / ".incoming-new"
        copy_runtime(self.root, self.bundle)
        (self.bundle / "deploy/tencent").mkdir(parents=True)
        environment = self.bundle / ".env.production"
        environment.write_text(environment.read_text().replace("RELEASE_VALUE=old", "RELEASE_VALUE=new"))
        (self.bundle / "compose.production.yaml").write_text("new compose")
        for name in KEY_FILES:
            (self.bundle / "secrets" / name).write_text("new key")

    def test_birthday_readiness_uses_candidate_port_and_every_host(self):
        config = dict(self.runtime.config, PLATFORM_APPS="health, journal, birthday",
                      BIRTHDAY_API_HOSTS="birthday.example, birthday-alias.example")
        with patch("release.probe", side_effect=lambda url, host, status: host != "birthday-alias.example") as probe:
            self.assertFalse(internally_ready(config, 1, ports=(18180, 18181, 18182)))
        self.assertIn((("http://127.0.0.1:18180/readyz", "birthday.example", "ready"),), probe.call_args_list)
        self.assertIn((("http://127.0.0.1:18180/readyz", "birthday-alias.example", "ready"),), probe.call_args_list)
        with patch("release.probe", return_value=True):
            self.assertTrue(internally_ready(config, 1))

    def test_birthday_default_host_is_checked_only_when_enabled(self):
        for apps, count in (("health,journal", 4), ("health,journal,birthday", 5)):
            with self.subTest(apps=apps), patch("release.probe", return_value=True) as probe:
                self.assertTrue(internally_ready(dict(self.runtime.config, PLATFORM_APPS=apps), 1))
                self.assertEqual(probe.call_count, count)
                if count == 5:
                    probe.assert_called_with("http://127.0.0.1:18080/readyz", "api.birthday.tellyouwhat.cn", "ready")

    def test_invalid_birthday_host_blocks_release_before_activation(self):
        with (self.bundle / ".env.production").open("a") as output:
            output.write("PLATFORM_APPS=health,journal,birthday\nBIRTHDAY_API_HOSTS=https://birthday.example/path\n")
        with self.assertRaisesRegex(OperationError, "birthday production API host"):
            validate(self.bundle, "new", "registry/app", "internal")

    def test_albums_readiness_checks_same_gateway_with_its_own_response_contract(self):
        config = dict(self.runtime.config, PLATFORM_APPS="health,journal,birthday,albums",
                      ALBUM_API_HOST="api.albums.tellyouwhat.cn", ALBUM_DEPLOYMENT_ENVIRONMENT="production")
        with patch("release.probe", side_effect=lambda url, host, status, field="status": field != "code") as check:
            self.assertFalse(internally_ready(config, 1, ports=(18180, 18181, 18182)))
            check.assert_called_with("http://127.0.0.1:18180/readyz", "api.albums.tellyouwhat.cn", "ok", "code")
        for key, value in (("ALBUM_API_HOST", "api-test.albums.tellyouwhat.cn"),
                           ("ALBUM_DEPLOYMENT_ENVIRONMENT", "test")):
            with self.assertRaisesRegex(OperationError, "album production"):
                internally_ready(dict(config, **{key: value}), 1)

    def test_probe_does_not_confuse_album_and_ai_readiness(self):
        from types import SimpleNamespace
        for body, field, expected in (({'code': 'ok'}, 'code', True), ({'status': 'ok'}, 'code', False),
                                       ({'code': 'ok'}, 'status', False), ({'status': 'ok'}, 'status', True)):
            def curl(command, **kwargs):
                Path(command[command.index('--output') + 1]).write_text(json.dumps(body))
                return SimpleNamespace(returncode=0, stdout=b'200')
            with patch("release.subprocess.run", side_effect=curl):
                self.assertEqual(probe("http://localhost/readyz", "example.test", "ok", field), expected)

    def run_deploy(self):
        with patch("release.require_disk_space"):
            return deploy(self.runtime, "new", "registry/app", "internal", self.bundle, 1)

    def assert_old_runtime(self):
        self.assertEqual(read_environment(self.root / ".env.production")["RELEASE_VALUE"], "old")
        self.assertEqual(read_environment(self.root / ".env.production")["IMAGE_TAG"], "old")
        self.assertEqual((self.root / "compose.production.yaml").read_text(), "old compose")
        self.assertEqual((self.root / "secrets" / KEY_FILES[0]).read_text(), "old key")

    def test_automatic_protection_prevents_downgrade_before_activation(self):
        def execute(label, command, **kwargs):
            if label == "release-capabilities":
                return b"1\n1\n1\n1\n" if command[-1].endswith(":new") else b"<no value>\n" * 4
            return b""
        with patch.object(self.runtime, "execute", side_effect=execute), patch("release.internally_ready", return_value=True):
            self.run_deploy()
        with patch.object(self.runtime, "execute") as execute:
            with self.assertRaisesRegex(OperationError, "automatic protection"):
                rollback(self.runtime, 1)
            execute.assert_not_called()
        self.assertEqual(read_environment(self.root / ".env.production")["IMAGE_TAG"], "new")
        with patch.object(self.runtime, "execute", return_value=b"<no value>\n" * 4) as execute, patch("release.require_disk_space"):
            with self.assertRaisesRegex(OperationError, "automatic protection"):
                deploy(self.runtime, "legacy", "registry/app", "internal", None, 1)
            self.assertFalse(any(c.args[0] in ("release-migrate", "release-activate") for c in execute.call_args_list))

    def test_migration_failure_does_not_replace_active_configuration(self):
        def execute(label, *_args, **_kwargs):
            if label == "release-migrate":
                raise OperationError("migration unavailable")
            return b""
        with patch.object(self.runtime, "execute", side_effect=execute), patch("release.internally_ready", return_value=True):
            with self.assertRaisesRegex(OperationError, "migration unavailable"):
                self.run_deploy()
        self.assert_old_runtime()

    def test_failed_backup_blocks_pull_migration_and_activation(self):
        with patch("release.create_backup", side_effect=OperationError("backup unavailable")), \
                patch.object(self.runtime, "execute") as execute:
            with self.assertRaisesRegex(OperationError, "backup unavailable"):
                self.run_deploy()
        execute.assert_not_called()
        self.assert_old_runtime()

    def test_backup_integrity_failure_blocks_migration(self):
        with patch("release.verify_backup", side_effect=OperationError("content verification failed")), \
                patch.object(self.runtime, "execute") as execute:
            with self.assertRaisesRegex(OperationError, "content verification failed"):
                self.run_deploy()
        execute.assert_not_called()

    def test_low_space_blocks_backup_and_deployment(self):
        from release import require_disk_space
        from types import SimpleNamespace
        with patch("release.shutil.disk_usage", return_value=SimpleNamespace(free=7 * 1024**3, total=40 * 1024**3)):
            with self.assertRaisesRegex(OperationError, "free disk"):
                require_disk_space(self.runtime)

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

    def test_ark_mount_is_used_by_candidate_activation_and_restored_on_rollback(self):
        credential = self.root / 'isolated-credential.json'
        credential.write_text('{"accessKey":"fixture","secretKey":"fixture"}')
        credential.chmod(0o600)
        with (self.bundle / '.env.production').open('a') as output:
            output.write('ARK_MANAGEMENT_CREDENTIAL_HOST_FILE=' + str(credential.resolve()) + '\n'
                         'ARK_MANAGEMENT_CREDENTIAL_FILE=/run/ark-management/credentials.json\n'
                         'AI_ENDPOINT_WRITES_ENABLED=true\n')
        (self.bundle / 'compose.ark-management.yaml').write_text('credential overlay')
        original_stat = Path.stat
        credential_path = str(credential.resolve())

        def stat(path, *args, **kwargs):
            info = original_stat(path, *args, **kwargs)
            if str(path) == credential_path:
                fields = list(info)
                fields[4] = 65532
                return os.stat_result(fields)
            return info

        with patch.object(Path, 'stat', stat), patch.object(self.runtime, 'execute', return_value=b'') as execute, \
                patch('release.internally_ready', return_value=True):
            self.run_deploy()
            calls = [(call.args[0], call.args[1]) for call in execute.call_args_list]
            for label in ('release-config', 'release-migrate', 'release-activate'):
                command = next(command for name, command in calls if name == label)
                self.assertTrue(any(item.endswith('/compose.ark-management.yaml') for item in command))
            execute.reset_mock()
            rollback(self.runtime, 1)
            command = next(call.args[1] for call in execute.call_args_list if call.args[0] == 'release-activate')
            self.assertFalse(any(item.endswith('/compose.ark-management.yaml') for item in command))
            self.assertNotIn('AI_ENDPOINT_WRITES_ENABLED', read_environment(self.root / '.env.production'))
            execute.reset_mock()
            rollback(self.runtime, 1)
            command = next(call.args[1] for call in execute.call_args_list if call.args[0] == 'release-activate')
            self.assertTrue(any(item.endswith('/compose.ark-management.yaml') for item in command))
        self.assertTrue(credential.exists())

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
