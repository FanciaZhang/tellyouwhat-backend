import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import platform_test_deploy as dev
from ops_common import OperationError


class PlatformTestBootstrapTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name) / 'backend'
        self.root.mkdir(mode=0o700)
        self.source = 'a' * 40
        self.folder = self.root / '.platform-test/releases' / self.source
        self.folder.mkdir(parents=True, mode=0o700)
        for parent in (self.folder.parent, self.folder.parent.parent):
            parent.chmod(0o700)
        self.runtime = Mock(root=self.root, state=self.root / '.operations')
        self.runtime.state.mkdir()
        (self.runtime.state / 'blue-green.json').write_text('{"phase":"STABLE","current":{"path":"production"}}')
        for name in ('operations', 'deployment'):
            (self.runtime.state / (name + '.lock')).touch(mode=0o600)
        self.app = {'BIRTHDAY_AI_ENABLED': 'true', 'BIRTHDAY_AI_ONLY': 'true',
                    'BIRTHDAY_APPLE_BUNDLE_ID': 'cn.tellyouwhat.birthday.dev', 'BIRTHDAY_APPLE_TEAM_ID': 'T6DD96HY2B',
                    'BIRTHDAY_API_HOSTS': dev.HOST, 'BIRTHDAY_APP_ATTEST_ENV': 'production',
                    'AI_PROJECT_MONTHLY_BUDGET_CNY': '10', 'BIRTHDAY_ALLOWED_APP_BUILDS': '2,3',
                    'BIRTHDAY_DEV_ACTIVATION_SECRET': 'a' * 64}
        self.compose = {'PLATFORM_TEST_ENV_FILE': str(self.folder / 'runtime.env'), 'PLATFORM_TEST_IMAGE_TAG': self.source,
                        'PLATFORM_TEST_API_PORT': '18790', 'PLATFORM_TEST_NETWORK_CIDR': '172.29.93.0/24',
                        'PLATFORM_TEST_MYSQL_PASSWORD': '1' * 64, 'PLATFORM_TEST_MYSQL_ROOT_PASSWORD': '2' * 64,
                        'PLATFORM_TEST_REDIS_PASSWORD': '3' * 64}
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name in ('deploy/tencent/platform-test.Dockerfile', 'go.mod', 'go.sum', 'cmd/gateway/main.go',
                         'cmd/migrate/main.go', 'compose.platform-test.yaml', 'migrations/0001_example.sql'):
                data = b'name: fixture\n' if name == 'compose.platform-test.yaml' else b'x'
                item = tarfile.TarInfo(name)
                item.size = len(data)
                archive.addfile(item, io.BytesIO(data))
        (self.folder / 'compose.platform-test.yaml').write_bytes(b'name: fixture\n')
        self.save()

    def save(self):
        for name, values in [('runtime.env', self.app), ('compose.env', self.compose)]:
            (self.folder / name).write_text(''.join(k + '=' + v + '\n' for k, v in values.items()))
        record = {'source': self.source, 'files': {n: hashlib.sha256((self.folder / n).read_bytes()).hexdigest() for n in dev.FILES}}
        (self.folder / 'selection.json').write_text(json.dumps(record))
        for p in self.folder.iterdir():
            p.chmod(0o600)

    def test_selected_source_rejects_mixed_identity_credentials_and_configuration(self):
        self.assertEqual(dev.selection(self.runtime, self.source)[2], ['0001_example.sql'])
        for target, key, value in [(self.app, 'BIRTHDAY_APPLE_BUNDLE_ID', 'cn.tellyouwhat.birthday'),
                                   (self.app, 'BIRTHDAY_APP_ATTEST_ENV', 'development'),
                                   (self.app, 'AI_PROJECT_MONTHLY_BUDGET_CNY', '400'),
                                   (self.app, 'TOS_SECRET_KEY', 'synthetic'),
                                   (self.app, 'DATABASE_DSN', 'production'),
                                   (self.compose, 'PLATFORM_TEST_NETWORK_CIDR', '172.29.92.0/24')]:
            old = target.get(key)
            target[key] = value
            self.save()
            with self.subTest(key=key), self.assertRaises(OperationError):
                dev.selection(self.runtime, self.source)
            if old is None:
                target.pop(key)
            else:
                target[key] = old
        self.save()
        (self.folder / 'source.tar').write_bytes(b'tampered')
        with self.assertRaisesRegex(OperationError, 'changed'):
            dev.selection(self.runtime, self.source)

    def test_private_boundary_and_source_compose_are_checked(self):
        (self.folder / 'runtime.env').chmod(0o640)
        with self.assertRaises(OperationError):
            dev.selection(self.runtime, self.source)
        (self.folder / 'compose.platform-test.yaml').write_bytes(b'other: source\n')
        self.save()
        with self.assertRaisesRegex(OperationError, 'Compose differs'):
            dev.selection(self.runtime, self.source)

    def execute(self, label, command, **kwargs):
        if label == 'platform-test-schema':
            return b'1\t0001_example.sql\n1\n0\n'
        if label == 'platform-test-running':
            return b'[{"State":{"Running":true,"OOMKilled":false},"RestartCount":0}]'
        return b''

    def activate(self):
        with patch.object(dev.fcntl, 'flock', side_effect=BlockingIOError), patch.object(dev, 'assert_route'), patch.object(dev, 'ready'):
            return dev.activate(self.runtime, self.source)

    def test_fresh_stack_starts_only_birthday_and_preserves_existing_services(self):
        self.runtime.execute.side_effect = self.execute
        result = self.activate()
        self.assertEqual(result['apps'], ['birthday'])
        self.assertFalse(result['real_app_attest_verified'])
        commands = [c.args[1] for c in self.runtime.execute.call_args_list]
        self.assertFalse(any('down' in c or '--volumes' in c or 'birthday-private-development' in c for c in commands))
        starts = [c for c in commands if 'up' in c]
        self.assertEqual(starts[-1][-1], 'gateway')
        with self.assertRaisesRegex(OperationError, 'already exists'):
            self.activate()

    def test_failed_start_keeps_database_and_stops_only_new_gateway(self):
        self.runtime.execute.side_effect = self.execute
        with patch.object(dev.fcntl, 'flock', side_effect=BlockingIOError), patch.object(dev, 'assert_route'), \
                patch.object(dev, 'ready', side_effect=OperationError('not ready')):
            with self.assertRaisesRegex(OperationError, 'not ready'):
                dev.activate(self.runtime, self.source)
        self.assertFalse((self.runtime.state / 'platform-test.json').exists())
        self.assertEqual(self.runtime.execute.call_args.args[1][-4:], ['stop', '-t', '30', 'gateway'])

    def test_unlocked_execution_and_preexisting_data_are_rejected_before_build(self):
        with self.assertRaisesRegex(OperationError, 'both owner locks'):
            dev.activate(self.runtime, self.source)
        self.runtime.execute.side_effect = lambda label, *a, **k: b'volume' if label == 'platform-test-volumes' else b''
        with self.assertRaisesRegex(OperationError, 'already exists'):
            self.activate()
        self.assertFalse(any(c.args[0].startswith('platform-test-build') for c in self.runtime.execute.call_args_list))


if __name__ == '__main__':
    unittest.main()
