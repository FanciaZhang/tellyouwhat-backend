import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import albums_production as resources
from ops_common import OperationError, Runtime, atomic_json, atomic_text


class AlbumsResourceTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = (Path(temporary.name) / 'backend').resolve()
        self.root.mkdir(mode=0o700)
        (self.root / '.env.production').write_text('APP_ENV=production\n')
        self.runtime = Runtime(self.root, self.root / 'backups')
        self.source = 'a' * 40
        self.folder = self.root / '.albums-production/releases' / self.source
        self.folder.mkdir(parents=True, mode=0o700)
        self.folder.parent.chmod(0o700)
        self.folder.parent.parent.chmod(0o700)
        self.base = self.root.parent / 'albums-production'
        self.base.mkdir(mode=0o700)
        self.config = {'ALBUM_PRODUCTION_ENV_FILE': str(self.folder / 'runtime.env'),
            'ALBUM_PRODUCTION_SECRET_DIRECTORY': str(self.base / 'secrets'), 'ALBUM_PRODUCTION_IMAGE_PREFIX': resources.PROJECT,
            'ALBUM_PRODUCTION_IMAGE_TAG': self.source, 'ALBUM_PRODUCTION_NETWORK_CIDR': '172.29.94.0/24',
            'ALBUM_PRODUCTION_MYSQL_USER': 'albums_production', 'ALBUM_PRODUCTION_MYSQL_PASSWORD': '1' * 64,
            'ALBUM_PRODUCTION_MYSQL_ROOT_PASSWORD': '2' * 64, 'ALBUM_PRODUCTION_REDIS_PASSWORD': '3' * 64}
        app = dict(resources.REGISTRY, ALBUM_COS_BUCKET='tellyouwhat-albums-production-1251995834',
            ALBUM_COS_REGION='ap-guangzhou', ALBUM_COS_ARCHIVE_SECRET_ID='fixture-archive', ALBUM_COS_ARCHIVE_SECRET_KEY='fixture-archive-key',
            ALBUM_COS_UPLOAD_SECRET_ID='fixture-upload', ALBUM_COS_UPLOAD_SECRET_KEY='fixture-upload-key', ALBUM_APPLE_SIGN_IN_KEY_ID='ABCDEFGHIJ')
        backup = {'MYSQL_HOST': 'localhost', 'MYSQL_PORT': '3306', 'MYSQL_DATABASE': 'albums_production',
                  'MYSQL_USER': 'albums_production_backup', 'MYSQL_PASSWORD': '4' * 64, 'ALBUM_BACKUP_ENCRYPTION_KEY': '5' * 64}
        for path, values in ((self.folder / 'runtime.env', app), (self.folder / 'compose.env', self.config), (self.base / 'backup.env', backup)):
            atomic_text(path, ''.join(k + '=' + v + '\n' for k, v in values.items()))
        atomic_text(self.folder / 'compose.albums-production.yaml', 'services: {}\n')
        names = ['Dockerfile', 'go.mod', 'go.sum', 'cmd/albumworker/main.go', 'cmd/migrate/main.go',
                 'deploy/albums/backup.py', 'deploy/albums/deletion_ledger.py', 'compose.albums-production.yaml', 'migrations/0001_fixture.sql']
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name in names:
                data = b'services: {}\n' if name.endswith('.yaml') else b'fixture\n'
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                archive.addfile(entry, io.BytesIO(data))
        (self.folder / 'source.tar').chmod(0o600)
        self.manifest()
        self.runtime.execute = Mock(side_effect=self.execute)
        self.fail_at = None
        self.containers = b''
        self.networks = []
        self.valid_checkpoint = True
        # Real signing ownership and container execution have separate deployment acceptance.
        for name, value in [('production_keys', set()), ('read_state', {'phase': 'STABLE', 'current': {'slot': 'green'}}),
                            ('assert_route', None), ('storage_check', None), ('require_memory_capacity', None),
                            ('validate_compose', None), ('pull_images', {'albumworker': 'sha256:fixture-worker', 'migrate': 'sha256:fixture-migrate'}), ('verify_worker', None)]:
            target = patch.object(resources, name, return_value=value)
            target.start()
            self.addCleanup(target.stop)
        for name, effect in [('create_backup', lambda runtime: {'filename': 'fixture.enc'}),
                             ('verify_backup', lambda *args: {'recovery_albums_production': {
                                'current/selection.json': hashlib.sha256((self.folder / 'selection.json').read_bytes()).hexdigest(),
                                **({'checkpoint/albums-fixture.sql.gz.enc': 'digest'} if self.valid_checkpoint else {})}})]:
            target = patch.object(resources, name, side_effect=effect)
            target.start()
            self.addCleanup(target.stop)

    def manifest(self):
        atomic_json(self.folder / 'selection.json', {'source': self.source, 'files': {
            name: hashlib.sha256((self.folder / name).read_bytes()).hexdigest()
            for name in resources.PRODUCTION_FILES if name != 'selection.json'}})

    def execute(self, label, command, **kwargs):
        if label == self.fail_at:
            raise OperationError('injected ' + label)
        if label == 'albums-production-existing':
            return self.containers
        if label == 'albums-production-networks':
            return b'fixture-network' if self.networks else b''
        if label == 'albums-production-network-ranges':
            return json.dumps(self.networks).encode()
        if label == 'albums-production-schema':
            return b'0001_fixture.sql\n'
        return b''

    def test_fresh_resources_are_registered_backed_up_then_admitted_without_an_api(self):
        result = resources.bootstrap(self.runtime, self.source, '123')
        self.assertTrue(result['passed'])
        self.assertFalse(result['gateway_changed'])
        state = json.loads((self.runtime.state / 'albums-production.json').read_text())
        self.assertEqual(state['phase'], 'ready')
        self.assertFalse(state['public_route_verified'])
        owner = json.loads((self.runtime.state / 'artifact-owners/albums-production.json').read_text())
        self.assertEqual(owner['current']['source'], self.source)
        commands = [call.args[1] for call in self.runtime.execute.call_args_list]
        self.assertFalse(any('albumgateway' in command or '--volumes' in command or 'down' in command for command in commands))
        sql = next(call.kwargs['input'] for call in self.runtime.execute.call_args_list if call.args[0] == 'albums-production-backup-user')
        self.assertIn(b'GRANT SELECT, SHOW VIEW ON albums_production.*', sql)
        self.assertNotIn('4' * 64, str(commands))

    def test_existing_resources_or_conflicting_subnet_reject_before_build_and_backup(self):
        for condition in ('container', 'subnet'):
            self.containers = b'existing' if condition == 'container' else b''
            self.networks = [{'IPAM': {'Config': [{'Subnet': '172.29.94.0/24'}]}}] if condition == 'subnet' else []
            with self.subTest(condition=condition), self.assertRaises(OperationError):
                resources.bootstrap(self.runtime, self.source, '123')
            resources.pull_images.assert_not_called()
            resources.create_backup.assert_not_called()

    def test_failed_migration_keeps_data_and_protected_source_and_stops_only_worker(self):
        self.fail_at = 'albums-production-migrate'
        with self.assertRaises(OperationError):
            resources.bootstrap(self.runtime, self.source, '123')
        self.assertTrue((self.runtime.state / 'artifact-owners/albums-production.json').exists())
        self.assertFalse((self.runtime.state / 'albums-production.json').exists())
        stop = next(call.args[1] for call in self.runtime.execute.call_args_list if call.args[0] == 'albums-production-stop-failed')
        self.assertEqual(stop[-4:], ['stop', '-t', '40', 'albumworker'])
        self.assertTrue((self.base / 'database.env').is_file())

    def test_incomplete_post_creation_backup_keeps_worker_stopped_and_resources_unadmitted(self):
        self.valid_checkpoint = False
        with self.assertRaisesRegex(OperationError, 'authenticated recovery'):
            resources.bootstrap(self.runtime, self.source, '123')
        state = json.loads((self.runtime.state / 'albums-production.json').read_text())
        self.assertEqual(state['phase'], 'prepared')
        self.assertFalse(any(call.args[0] == 'albums-production-worker' for call in self.runtime.execute.call_args_list))
        with self.assertRaisesRegex(OperationError, 'reinitialize'):
            resources.bootstrap(self.runtime, self.source, '123')

    def test_selection_rejects_other_environment_and_shared_backup_secrets(self):
        app_path = self.folder / 'runtime.env'
        app_path.write_text(app_path.read_text().replace('cn.tellyouwhat.albums', 'cn.tellyouwhat.albums.dev'))
        self.manifest()
        with self.assertRaisesRegex(OperationError, 'another environment'):
            resources.selection(self.runtime, self.source)
        app_path.write_text(app_path.read_text().replace('cn.tellyouwhat.albums.dev', 'cn.tellyouwhat.albums'))
        self.manifest()
        settings = self.base / 'backup.env'
        settings.write_text(settings.read_text().replace('4' * 64, '1' * 64))
        with self.assertRaisesRegex(OperationError, 'backup credentials'):
            resources.selection(self.runtime, self.source)


class AlbumsResourceBoundaryTests(unittest.TestCase):
    def test_rendered_compose_rejects_missing_persistence_public_ports_and_foreign_mounts(self):
        import copy
        source = 'a' * 40
        services = {name: {'networks': {'albums-production': {}},
                          'image': resources.PROJECT + '-' + name + ':' + source}
                    for name in ('mysql', 'redis', 'migrate', 'albumworker')}
        services['mysql']['volumes'] = [{'type': 'volume', 'source': 'albums-production-mysql', 'target': '/var/lib/mysql'}]
        good = {'name': resources.PROJECT, 'services': services,
                'networks': {'albums-production': {'ipam': {'config': [{'subnet': '172.29.94.0/24'}]}}},
                'volumes': {'albums-production-mysql': {'name': resources.PROJECT + '_albums-production-mysql'}}}
        runtime = Mock()
        runtime.execute.return_value = json.dumps(good).encode()
        resources.validate_compose(runtime, Path('/synthetic'), source)
        for change in ('no-volume', 'external-volume', 'port', 'mount', 'wrong-image'):
            bad = copy.deepcopy(good)
            if change == 'no-volume': bad['services']['mysql']['volumes'] = []
            if change == 'external-volume': bad['volumes']['albums-production-mysql']['external'] = True
            if change == 'port': bad['services']['mysql']['ports'] = [{'published': '3306'}]
            if change == 'mount': bad['services']['albumworker']['volumes'] = [{'type': 'bind', 'source': '/', 'target': '/host'}]
            if change == 'wrong-image': bad['services']['albumworker']['image'] = 'wrong:latest'
            runtime.execute.return_value = json.dumps(bad).encode()
            with self.subTest(change=change), self.assertRaises(OperationError):
                resources.validate_compose(runtime, Path('/synthetic'), source)

    def test_release_entry_holds_both_locks_and_cleans_only_resource_owner(self):
        import contextlib
        import fcntl
        import sys
        import release
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / '.env.production').write_text('APP_ENV=production\n')
            def bootstrap(runtime, source, source_run):
                for name in ('operations', 'deployment'):
                    with (runtime.state / (name + '.lock')).open() as handle:
                        with self.assertRaises(BlockingIOError):
                            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return {'passed': True, 'gateway_changed': False}
            with patch.object(sys, 'argv', ['release.py', 'deploy', '--backend-dir', str(root), '--albums-resources', 'a' * 40, '--source-run', '123']), \
                    patch('albums_production.bootstrap', side_effect=bootstrap) as run, \
                    patch.object(release, 'require_disk_space'), patch.object(release, 'deploy') as shared, \
                    patch.object(release, 'cleanup', return_value={'completed_at': 1}) as cleanup, \
                    patch.object(release.subprocess, 'run'), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(release.main(), 0)
                self.assertEqual(cleanup.call_args.args[1], 'albums-production')
                run.assert_called_once()
                shared.assert_not_called()

    def test_worker_readiness_requires_source_bound_image_and_startup_log(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = 'a' * 40
            folder = root / '.albums-production/releases' / source
            folder.mkdir(parents=True)
            (folder / 'source.tar').write_bytes(b'synthetic source')
            digest = hashlib.sha256(b'synthetic source').hexdigest()
            runtime = Mock(root=root)
            container = {'State': {'Running': True, 'StartedAt': '2026-10-16T00:00:00Z'}, 'Image': 'sha256:fixture',
                         'Config': {'Image': resources.PROJECT + '-albumworker:' + source},
                         'NetworkSettings': {'Networks': {resources.NETWORK: {}}}}
            image = {'Id': 'sha256:fixture', 'Config': {'Labels': {'org.opencontainers.image.revision': source,
                     'cn.tellyouwhat.source-tar-sha256': digest}}}
            def execute(label, command, **kwargs):
                if label == 'albums-production-worker-state': return json.dumps([container]).encode()
                if label == 'albums-production-worker-image': return json.dumps([image]).encode()
                self.assertIn('2>&1', command[2])
                return b'{"msg":"album workers started"}\n'
            runtime.execute.side_effect = execute
            resources.verify_worker(runtime, source)
            image['Id'] = 'sha256:another-image'
            with self.assertRaisesRegex(OperationError, 'selected source'):
                resources.verify_worker(runtime, source)
            image['Id'] = 'sha256:fixture'
            container['RestartCount'] = 1
            with self.assertRaisesRegex(OperationError, 'failed to start'):
                resources.verify_worker(runtime, source)

    def test_published_images_must_match_source_archive_run_and_existing_local_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            (folder / 'source.tar').write_bytes(b'synthetic source')
            digest = hashlib.sha256(b'synthetic source').hexdigest()
            source = 'a' * 40
            runtime = Mock()
            defect = None
            def execute(label, command, **kwargs):
                if label == 'albums-production-image':
                    repository = command[-1].split(':')[0]
                    return json.dumps([{'Id': 'image-id', 'RepoDigests': [repository + '@sha256:' + 'b' * 64],
                        'Config': {'Labels': {'org.opencontainers.image.revision': source,
                        'cn.tellyouwhat.source-tar-sha256': 'wrong' if defect == 'archive' else digest,
                        'cn.tellyouwhat.source-run': 'other' if defect == 'run' else '123'}}}]).encode()
                if label == 'albums-production-existing-image': return b'old-id' if defect == 'local-image' else b''
                if label == 'albums-production-local-image': return b'[{"Id":"old-id"}]'
                return b''
            runtime.execute.side_effect = execute
            images = resources.pull_images(runtime, folder, source, '123')
            self.assertEqual(set(images), {'albumworker', 'migrate'})
            self.assertTrue(all('@sha256:' in image for image in images.values()))
            for defect in ('archive', 'run', 'local-image'):
                runtime.execute.reset_mock()
                with self.subTest(defect=defect), self.assertRaises(OperationError):
                    resources.pull_images(runtime, folder, source, '123')
                self.assertFalse(any(c.args[0] == 'albums-production-image-tag' for c in runtime.execute.call_args_list))
