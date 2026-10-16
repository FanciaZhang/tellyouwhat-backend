import hashlib
from contextlib import ExitStack
import io
import json
import os
import shutil
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
            for name in ('deploy/albums/development.Dockerfile', 'go.mod', 'go.sum', 'cmd/albumgateway/main.go', 'cmd/albumworker/main.go',
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
        if label == 'albums-development-running-image':
            source = command[-1].split(':')[-1]
            folder = self.root / '.albums-development/releases' / source
            return json.dumps([{'Id': 'sha256:fixture', 'Config': {'Labels': {
                'org.opencontainers.image.revision': source,
                'cn.tellyouwhat.source-tar-sha256': hashlib.sha256((folder / 'source.tar').read_bytes()).hexdigest()}}}]).encode()
        if label == 'albums-development-running':
            service = command[-1].removeprefix(dev.PROJECT + '-').removesuffix('-1')
            return json.dumps([{'State': {'Running': True, 'OOMKilled': False}, 'RestartCount': 0,
                                'Image': 'sha256:fixture', 'Config': {'Image': dev.IMAGE_PREFIX + service + ':' + self.source}}]).encode()
        return b''

    def test_unhealthy_worker_stops_the_unpublished_stack(self):
        self.runtime.execute.side_effect = lambda label, *args, **kwargs: b'[{"State":{"Running":false}}]' if label == 'albums-development-running' else self.execute(label, *args, **kwargs)
        with self.assertRaisesRegex(OperationError, 'did not start cleanly'):
            self.activate()
        self.assertEqual(self.runtime.execute.call_args.args[1][-5:], ['stop', '-t', '40', 'albumgateway', 'albumworker'])

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
        builds = [cmd for cmd in commands if cmd[:2] == ['docker', 'build']]
        self.assertEqual({cmd[cmd.index('--target') + 1] for cmd in builds}, set(dev.SERVICES))
        self.assertTrue(all('deploy/albums/development.Dockerfile' in cmd and '--memory=1024m' in cmd for cmd in builds))
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


class DevelopmentUpgradeTests(unittest.TestCase):
    save = DevelopmentBootstrapTests.save

    def setUp(self):
        DevelopmentBootstrapTests.setUp(self)
        self.old_source, self.old = self.source, self.folder
        self.source = 'b' * 40
        self.folder = self.old.parent / self.source
        shutil.copytree(self.old, self.folder)
        self.compose.update(ALBUM_TEST_IMAGE_TAG=self.source, ALBUM_TEST_ENV_FILE=str(self.folder / 'runtime.env'))
        self.save()
        self.previous = {'source': self.old_source, 'release': str(self.old), 'migrations': 1,
                         'latest_migration': '0001_example.sql', 'accounts': 3, 'uploads': 8, 'previous': None}
        self.state_path = self.runtime.state / 'albums-development.json'
        self.state_path.write_text(json.dumps(self.previous))
        self.state_path.chmod(0o600)
        owners = self.runtime.state / 'artifact-owners'
        owners.mkdir()
        self.owner_path = owners / 'albums-test.json'
        self.owner_path.write_text(json.dumps({'current': {'source': self.old_source, 'paths': [str(self.old)]}}))
        self.owner_path.chmod(0o600)
        self.runtime.backups = self.root / 'backups'
        self.running = self.old_source
        self.fail_new = False
        self.schema = b'0001_example.sql\n'
        self.runtime.execute.side_effect = self.execute
        stack = ExitStack()
        self.addCleanup(stack.close)
        self.backup = stack.enter_context(patch.object(dev, 'create_backup', return_value={'filename': 'backup.enc'}))
        self.verify = stack.enter_context(patch.object(dev, 'verify_backup', return_value={
            'recovery_albums_development': {
                'current/selection.json': hashlib.sha256((self.old / 'selection.json').read_bytes()).hexdigest(),
                'checkpoint/albums-123.sql.gz.enc': 'digest'}}))
        stack.enter_context(patch.object(dev, 'ready'))
        stack.enter_context(patch.object(dev, 'assert_route'))
        stack.enter_context(patch.object(dev.fcntl, 'flock', side_effect=BlockingIOError))

    def execute(self, label, command, **kwargs):
        if label == 'albums-development-running-image':
            return DevelopmentBootstrapTests.execute(self, label, command, **kwargs)
        if label == 'albums-development-update-schema':
            return self.schema
        if label == 'albums-development-update-start':
            self.running = self.source
        if label == 'albums-development-update-rollback':
            self.running = self.old_source
        if label == 'albums-development-running':
            service = command[-1].removeprefix(dev.PROJECT + '-').removesuffix('-1')
            return json.dumps([{'State': {'Running': not(self.fail_new and self.running == self.source),
                                         'StartedAt': '2026-10-16T00:00:00Z'},
                                'RestartCount': 0, 'Image': 'sha256:fixture', 'Config': {'Image': dev.IMAGE_PREFIX + service + ':' + self.running}}]).encode()
        if label == 'albums-development-worker-startup':
            return b'{"msg":"album workers started"}\n'
        return b''

    def activate(self):
        return dev.activate(self.runtime, self.source, self.old_source)

    def test_updates_nonempty_stack_without_migration_or_replacing_databases(self):
        result = self.activate()
        self.assertEqual(result['source'], self.source)
        self.assertEqual(result['previous'], {k: v for k, v in self.previous.items() if k != 'previous'})
        self.assertEqual(json.loads(self.state_path.read_text()), result)
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        self.assertIn('albums-development-worker-startup', labels)
        self.assertNotIn('albums-development-migrate', labels)
        self.assertNotIn('albums-development-databases', labels)
        starts = [c.args[1] for c in self.runtime.execute.call_args_list if 'up' in c.args[1]]
        self.assertEqual(len(starts), 1)
        self.assertEqual(starts[0][-2:], ['albumgateway', 'albumworker'])
        self.assertIn('--no-deps', starts[0])

    def test_rejects_changed_credentials_configuration_schema_and_topology(self):
        for filename in ('compose.env', 'runtime.env', 'compose.albums-test.yaml', 'source.tar'):
            with self.subTest(filename=filename):
                path = self.folder / filename
                before = path.read_bytes()
                if filename == 'compose.env':
                    self.compose['ALBUM_TEST_MYSQL_PASSWORD'] = '4' * 64
                elif filename == 'runtime.env':
                    self.app['ALBUM_POLL_INTERVAL'] = '10s'
                elif filename == 'source.tar':
                    with tarfile.open(path, 'a') as archive:
                        item = tarfile.TarInfo('migrations/0002_new.sql')
                        archive.addfile(item)
                else:
                    path.write_bytes(b'changed topology')
                self.save()
                with self.assertRaises(OperationError):
                    self.activate()
                self.backup.assert_not_called()
                self.assertFalse(any(c.args[0].startswith('albums-development-build') for c in self.runtime.execute.call_args_list))
                self.compose['ALBUM_TEST_MYSQL_PASSWORD'] = '1' * 64
                self.app.pop('ALBUM_POLL_INTERVAL', None)
                path.write_bytes(before)
                self.save()

    def test_rejects_unregistered_or_drifted_live_state(self):
        for change in ('owner', 'schema', 'running'):
            with self.subTest(change=change):
                old_owner = self.owner_path.read_bytes()
                if change == 'owner': self.owner_path.write_text('{"current":{}}')
                if change == 'schema': self.schema = b'0001_example.sql\n0002_drift.sql\n'
                if change == 'running': self.running = 'c' * 40
                with self.assertRaises(OperationError): self.activate()
                self.backup.assert_not_called()
                self.owner_path.write_bytes(old_owner)
                self.schema = b'0001_example.sql\n'
                self.running = self.old_source

    def test_missing_or_wrong_recovery_snapshot_prevents_build_and_activation(self):
        for recovery in ({}, {'current/selection.json': 'wrong', 'checkpoint/albums-123.sql.gz.enc': 'digest'}):
            with self.subTest(recovery=recovery):
                self.verify.return_value = {'recovery_albums_development': recovery}
                with self.assertRaisesRegex(OperationError, 'authenticated'):
                    self.activate()
                self.assertFalse(any(c.args[0].startswith('albums-development-build') for c in self.runtime.execute.call_args_list))

    def test_failed_new_worker_restores_previous_images_without_database_restore(self):
        self.fail_new = True
        with self.assertRaisesRegex(OperationError, 'did not start cleanly'):
            self.activate()
        self.assertEqual(self.running, self.old_source)
        self.assertEqual(json.loads(self.state_path.read_text()), self.previous)
        commands = [c.args[1] for c in self.runtime.execute.call_args_list]
        rollback = next(c.args[1] for c in self.runtime.execute.call_args_list if c.args[0] == 'albums-development-update-rollback')
        self.assertIn(str(self.old / 'compose.env'), rollback)
        self.assertEqual(rollback[-2:], ['albumgateway', 'albumworker'])
        self.assertFalse(any('down' in c or '--volumes' in c or ('run' in c and 'migrate' in c) for c in commands))

    def test_retagged_rollback_image_blocks_update_before_backup(self):
        execute = self.execute
        self.runtime.execute.side_effect = lambda label, *args, **kwargs: b'[{"Id":"another-image"}]' if label == 'albums-development-running-image' else execute(label, *args, **kwargs)
        with self.assertRaisesRegex(OperationError, 'rollback image'):
            self.activate()
        self.backup.assert_not_called()

    def test_worker_process_without_started_message_is_not_ready(self):
        execute = self.execute
        self.runtime.execute.side_effect = lambda label, *args, **kwargs: b'' if label == 'albums-development-worker-startup' else execute(label, *args, **kwargs)
        with patch.object(dev.time, 'sleep'):
            with self.assertRaisesRegex(OperationError, 'finish startup'):
                dev.verify_running(self.runtime, self.old_source, startup=True)


if __name__ == '__main__':
    unittest.main()
