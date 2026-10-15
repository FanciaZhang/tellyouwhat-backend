import hashlib
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import development as dev
from ops_common import OperationError


class DevelopmentBootstrapTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name) / 'backend'
        self.root.mkdir(mode=0o700)
        self.source = 'a' * 40
        self.folder = self.root / '.albums-development/releases' / self.source
        self.folder.mkdir(parents=True, mode=0o700)
        for parent in (self.folder.parent, self.folder.parent.parent):
            parent.chmod(0o700)
        self.runtime = Mock(root=self.root, state=self.root / '.operations')
        self.runtime.state.mkdir()
        (self.runtime.state / 'blue-green.json').write_text(json.dumps({'phase': 'STABLE', 'current': {'path': 'production'}}))
        for name in ('operations', 'deployment'):
            (self.runtime.state / (name + '.lock')).touch(mode=0o600)
        self.app = dict(ALBUM_API_HOST=dev.HOST, ALBUM_APPLE_BUNDLE_ID='cn.tellyouwhat.albums.dev',
                        ALBUM_APPLE_TEAM_ID='T6DD96HY2B', ALBUM_APPLE_SIGN_IN_KEY_ID='9H4R3FJ9D8',
                        ALBUM_COS_BUCKET='tellyouwhat-albums-1251995834', ALBUM_COS_REGION='ap-guangzhou')
        for key in ('SUBSCRIPTIONS', 'SHARING', 'TRASH', 'ACCOUNT_MEDIA_ERASURE', 'ACCOUNT_RECORD_ERASURE',
                    'RETENTION_PUSH', 'PHOTO_PROCESSING', 'VIDEO_PROCESSING', 'SUBSCRIPTION_REBINDING'):
            self.app['ALBUM_' + key + '_ENABLED'] = 'false'
        self.compose = dict(ALBUM_TEST_ENV_FILE=str(self.folder / 'runtime.env'),
                            ALBUM_TEST_SECRET_DIRECTORY=str(self.root.parent / 'albums-development/secrets'),
                            ALBUM_TEST_IMAGE_PREFIX=dev.IMAGE_PREFIX[:-1], ALBUM_TEST_IMAGE_TAG=self.source,
                            ALBUM_TEST_API_PORT='18092', ALBUM_TEST_NETWORK_CIDR='172.29.92.0/24',
                            ALBUM_TEST_PROXY_CIDR='172.29.92.1/32', ALBUM_TEST_MYSQL_USER='albums_development_test',
                            ALBUM_TEST_MYSQL_PASSWORD='1' * 64, ALBUM_TEST_MYSQL_ROOT_PASSWORD='2' * 64,
                            ALBUM_TEST_REDIS_PASSWORD='3' * 64)
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name in ('Dockerfile', 'go.mod', 'go.sum', 'cmd/albumgateway/main.go', 'cmd/albumworker/main.go',
                         'cmd/migrate/main.go', 'migrations/0001_example.sql'):
                item = tarfile.TarInfo(name)
                item.size = 1
                archive.addfile(item, io.BytesIO(b'x'))
        (self.folder / 'compose.albums-test.yaml').write_text('name: fixture\n')
        self.save()

    def save(self):
        for name, values in [('runtime.env', self.app), ('compose.env', self.compose)]:
            (self.folder / name).write_text(''.join(k + '=' + v + '\n' for k, v in values.items()))
        record = {'source': self.source, 'files': {name: hashlib.sha256((self.folder / name).read_bytes()).hexdigest() for name in dev.FILES}}
        (self.folder / 'selection.json').write_text(json.dumps(record))
        for p in self.folder.iterdir():
            p.chmod(0o600)

    def test_selection_rejects_mixed_identity_credentials_and_tampered_source(self):
        self.assertEqual(dev.selection(self.runtime, self.source)[2], ['0001_example.sql'])
        for target, key, value in [(self.app, 'ALBUM_APPLE_BUNDLE_ID', 'cn.tellyouwhat.albums'),
                                   (self.app, 'ALBUM_SUBSCRIPTIONS_ENABLED', 'true'),
                                   (self.app, 'ALBUM_PHOTO_PROCESSING_ENABLED', 'true'),
                                   (self.compose, 'ALBUM_TEST_MYSQL_USER', 'albums'),
                                   (self.compose, 'ALBUM_TEST_SECRET_DIRECTORY', '/production'),
                                   (self.app, 'ACCIDENTAL_DSN', 'user:' + '2' * 64 + '@host')]:
            with self.subTest(key=key):
                before = target.get(key)
                target[key] = value
                self.save()
                with self.assertRaises(OperationError):
                    dev.selection(self.runtime, self.source)
                if before is None:
                    target.pop(key)
                else:
                    target[key] = before
        self.save()
        (self.folder / 'source.tar').write_bytes(b'changed')
        with self.assertRaisesRegex(OperationError, 'changed'):
            dev.selection(self.runtime, self.source)

    def test_private_and_archive_boundaries_fail_closed(self):
        for mode in (0o640, 0o666):
            (self.folder / 'runtime.env').chmod(mode)
            with self.assertRaises(OperationError):
                dev.selection(self.runtime, self.source)
        self.save()
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            item = tarfile.TarInfo('../escape')
            archive.addfile(item)
        self.save()
        with self.assertRaisesRegex(OperationError, 'unsafe'):
            dev.selection(self.runtime, self.source)

    def execute(self, label, command, **kwargs):
        if label == 'albums-development-schema':
            return b'1\t0001_example.sql\n0\n0\n'
        return b''

    def activate(self):
        with patch.object(dev.fcntl, 'flock', side_effect=BlockingIOError), patch.object(dev, 'assert_route'), patch.object(dev, 'ready'):
            return dev.activate(self.runtime, self.source)

    def test_fresh_bootstrap_records_only_new_identity_after_schema_and_readiness(self):
        self.runtime.execute.side_effect = self.execute
        result = self.activate()
        self.assertEqual(result['api_host'], dev.HOST)
        self.assertIsNone(result['previous'])
        self.assertFalse(result['backup_verified'])
        self.assertFalse(result['real_apple_login_verified'])
        commands = [call.args[1] for call in self.runtime.execute.call_args_list]
        self.assertFalse(any('tellyouwhat-albums-test-next' in part for cmd in commands for part in cmd))
        self.assertFalse(any('down' in cmd or '--volumes' in cmd for cmd in commands))
        with self.assertRaisesRegex(OperationError, 'already exists'):
            self.activate()

    def test_startup_failure_stops_only_new_api_worker_and_keeps_data(self):
        self.runtime.execute.side_effect = self.execute
        with patch.object(dev.fcntl, 'flock', side_effect=BlockingIOError), patch.object(dev, 'assert_route'), \
                patch.object(dev, 'ready', side_effect=OperationError('not ready')):
            with self.assertRaisesRegex(OperationError, 'not ready'):
                dev.activate(self.runtime, self.source)
        self.assertFalse((self.runtime.state / 'albums-development.json').exists())
        final = self.runtime.execute.call_args.args[1]
        self.assertEqual(final[-5:], ['stop', '-t', '40', 'albumgateway', 'albumworker'])
        self.assertIn(dev.PROJECT, final)

    def test_bootstrap_rejects_existing_data_and_unowned_execution(self):
        with self.assertRaisesRegex(OperationError, 'both owner locks'):
            dev.activate(self.runtime, self.source)
        self.runtime.execute.side_effect = lambda label, *args, **kwargs: b'volume\n' if label == 'albums-development-volumes' else b''
        with self.assertRaisesRegex(OperationError, 'data already exists'):
            self.activate()
        self.assertFalse(any(call.args[0].startswith('albums-development-build') for call in self.runtime.execute.call_args_list))


if __name__ == '__main__':
    unittest.main()
