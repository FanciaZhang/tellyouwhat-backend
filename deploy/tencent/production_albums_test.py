import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import Mock, patch

import ops_albums as albums
from ops_common import OperationError, Runtime, atomic_json
from release import copy_runtime, deploy


class ProductionAlbumsTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve() / 'backend'
        self.root.mkdir(mode=0o700)
        (self.root / '.env.production').write_text('APP_ENV=production\n')
        self.runtime = Runtime(self.root)
        self.source = 'a' * 40
        self.bundle = self.root / '.incoming-fixture'
        self.bundle.mkdir(mode=0o700)
        self.config = {'APP_ENV': 'production', 'PUBLIC_PROXY_MODE': 'external',
                       'PLATFORM_APPS': 'health,journal,birthday,albums', **albums.REGISTRY,
                       'ALBUM_RESOURCE_SOURCE': self.source, 'MYSQL_HOST': '10.0.0.6', 'REDIS_HOST': '10.0.0.4'}
        self.write_env(self.bundle / '.env.production', self.config)
        self.overlay = Path(__file__).resolve().parents[2] / albums.OVERLAY
        shutil.copy2(self.overlay, self.bundle / albums.OVERLAY)
        self.original = self.root / '.albums-production/releases' / self.source
        self.original.mkdir(parents=True, mode=0o700)
        for p in (self.original.parent, self.original.parent.parent):
            p.chmod(0o700)
        self.secrets = self.root.parent / 'albums-production/secrets'
        self.secrets.mkdir(parents=True)
        replacement = patch.object(albums, 'SECRETS', self.secrets)
        replacement.start()
        self.addCleanup(replacement.stop)
        self.app = {k: v for k, v in albums.REGISTRY.items() if k != 'ALBUM_DEPLOYMENT_ENVIRONMENT'}
        self.app.update(ALBUM_COS_BUCKET='tellyouwhat-albums-production-1251995834', ALBUM_COS_REGION='ap-guangzhou',
                        ALBUM_COS_ARCHIVE_SECRET_KEY='synthetic-production-credential')
        self.compose = {'ALBUM_PRODUCTION_MYSQL_USER': 'albums_production', 'ALBUM_PRODUCTION_MYSQL_PASSWORD': '1' * 64,
                        'ALBUM_PRODUCTION_REDIS_PASSWORD': '2' * 64, 'ALBUM_PRODUCTION_MYSQL_ROOT_PASSWORD': '3' * 64,
                        'ALBUM_PRODUCTION_SECRET_DIRECTORY': str(self.secrets), 'ALBUM_PRODUCTION_NETWORK_CIDR': '172.29.94.0/24'}
        self.write_env(self.original / 'runtime.env', self.app)
        self.write_env(self.original / 'compose.env', self.compose)
        self.write(self.original / 'source.tar', b'synthetic source selection')
        self.write(self.original / 'compose.albums-production.yaml', b'services: {}\n')
        self.manifest()
        atomic_json(self.runtime.state / 'albums-production.json', {'source': self.source, 'release': str(self.original)})
        registry = self.runtime.state / 'artifact-owners'
        registry.mkdir()
        atomic_json(registry / 'albums-production.json', {'current': {'source': self.source, 'paths': [str(self.original)]}})
        self.runtime.execute = Mock(side_effect=self.execute)

    def write(self, path, body):
        path.write_bytes(body)
        path.chmod(0o600)

    def write_env(self, path, data):
        self.write(path, ''.join(k + '=' + v + '\n' for k, v in data.items()).encode())

    def manifest(self):
        names = set(albums.PRODUCTION_FILES) - {'selection.json'}
        atomic_json(self.original / 'selection.json', {'source': self.source,
                    'files': {n: hashlib.sha256((self.original / n).read_bytes()).hexdigest() for n in names}})

    def execute(self, label, command, **kwargs):
        if command[:3] == ['docker', 'network', 'inspect']:
            gateway = '172.18.0.1' if 'blue' in command[-1] else '172.19.0.1'
            return json.dumps([{'Name': command[-1], 'IPAM': {'Config': [{'Gateway': gateway}]}}]).encode()
        service = command[-1].removeprefix(albums.PROJECT + '-').removesuffix('-1')
        env = ({'MYSQL_DATABASE': 'albums_production', 'MYSQL_USER': 'albums_production', 'MYSQL_PASSWORD': '1' * 64}
               if service == 'mysql' else {'REDIS_PASSWORD': '2' * 64} if service == 'redis' else {
                   **self.app, 'ALBUM_DEPLOYMENT_ENVIRONMENT': 'production',
                   'ALBUM_DATABASE_DSN': 'albums_production:' + '1' * 64 + '@tcp(mysql:3306)/albums_production?charset=utf8mb4&collation=utf8mb4_0900_ai_ci',
                   'ALBUM_REDIS_URL': 'redis://:' + '2' * 64 + '@redis:6379/0'})
        return json.dumps([{'State': {'Running': True}, 'NetworkSettings': {'Networks': {albums.NETWORK: {}}},
                           'Config': {'Labels': {'com.docker.compose.project': albums.PROJECT},
                                      'Image': albums.PROJECT + '-albumworker:' + self.source,
                                      'Env': [k + '=' + v for k, v in env.items()]}}]).encode()

    def test_capacity_reserves_extra_memory_only_for_albums_gateway(self):
        import blue_green
        with patch.object(Path, 'read_text', return_value='MemTotal: 4194304 kB\nMemAvailable: 1966080 kB\n'):
            blue_green.require_memory_capacity()
            with self.assertRaises(blue_green.CapacityError):
                blue_green.require_memory_capacity(gateway_mib=512)

    def test_existing_health_journal_releases_do_not_touch_albums(self):
        albums.stage(self.runtime, self.root)
        self.assertEqual(albums.compose_overlay(self.root, {}), [])
        self.runtime.execute.assert_not_called()

    def test_staging_is_private_repeatable_and_preserves_a_matched_snapshot(self):
        albums.stage(self.runtime, self.bundle)
        original = (self.bundle / 'albums.env').read_bytes()
        albums.stage(self.runtime, self.bundle)
        self.assertEqual((self.bundle / 'albums.env').read_bytes(), original)
        self.assertEqual((self.bundle / 'albums.env').stat().st_mode & 0o777, 0o600)
        self.assertIn(b'@tcp(tellyouwhat-albums-production-mysql-1:', original)
        self.assertNotIn(('3' * 64).encode(), original)
        self.assertNotIn(b'synthetic-production-credential', (self.bundle / '.env.production').read_bytes())
        self.assertEqual(albums.compose_overlay(self.bundle, self.config), ['-f', str(self.bundle / albums.OVERLAY)])
        target = self.root / 'snapshot'
        copy_runtime(self.bundle, target)
        self.assertEqual((target / 'albums.env').read_bytes(), original)
        self.assertTrue((target / albums.OVERLAY).is_file())

    def test_unregistered_or_modified_resource_selection_cannot_be_staged(self):
        for kind in ('manifest', 'registration'):
            with self.subTest(kind=kind):
                target = self.original / 'runtime.env' if kind == 'manifest' else self.runtime.state / 'albums-production.json'
                before = target.read_bytes()
                self.write(target, before + b' ' if kind == 'manifest' else b'{}')
                with self.assertRaises(OperationError):
                    albums.stage(self.runtime, self.bundle)
                self.assertFalse((self.bundle / 'albums.env').exists())
                self.write(target, before)

    def test_no_overwrite_of_preexisting_mismatched_or_symlink_credentials(self):
        path = self.bundle / 'albums.env'
        self.write(path, b'preserved user configuration')
        with self.assertRaises(OperationError):
            albums.stage(self.runtime, self.bundle)
        self.assertEqual(path.read_bytes(), b'preserved user configuration')
        path.unlink()
        path.symlink_to(self.original / 'runtime.env')
        with self.assertRaises(OperationError):
            albums.stage(self.runtime, self.bundle)
        self.assertTrue(path.is_symlink())

    def test_cross_environment_or_shared_credentials_are_rejected(self):
        albums.stage(self.runtime, self.bundle)
        for changes in ({'APP_ENV': 'development'}, {'ALBUM_APPLE_BUNDLE_ID': 'cn.tellyouwhat.albums.dev'},
                        {'ALBUM_COS_ARCHIVE_SECRET_KEY': 'leaked'}, {'MYSQL_HOST': 'mysql'},
                        {'ALBUM_RESOURCE_SOURCE': '../other'}, {'ALBUM_DEPLOYMENT_ENVIRONMENT': 'test'}):
            with self.subTest(changes=changes), self.assertRaises(OperationError):
                albums.validate(self.bundle, dict(self.config, **changes))
        with self.assertRaises(OperationError):
            albums.require_configuration(self.runtime, self.bundle, dict(self.config, MYSQL_PASSWORD='1' * 64))
        with self.assertRaises(OperationError):
            albums.validate(self.bundle, {'ALBUM_COS_ARCHIVE_SECRET_KEY': 'leaked'})

    def test_missing_running_resources_fail_before_release_backup_or_changes(self):
        albums.stage(self.runtime, self.bundle)
        self.runtime.execute.side_effect = lambda *args, **kwargs: b'[{"State":{"Running":false}}]'
        with patch('release.validate', return_value=self.config), patch('release.create_backup') as backup:
            with self.assertRaises(OperationError):
                deploy(self.runtime, 'fixture', 'registry/app', 'internal', str(self.bundle), 1)
        backup.assert_not_called()
        self.assertTrue(all(call.args[1][:2] == ['docker', 'inspect'] or call.args[1][:3] == ['docker', 'network', 'inspect']
                            for call in self.runtime.execute.call_args_list))

    def test_worker_configuration_and_mounts_must_match_selected_resources(self):
        albums.stage(self.runtime, self.bundle)
        for change in ('credential', 'mount'):
            def inspect(label, command, **kwargs):
                data = json.loads(self.execute(label, command, **kwargs))
                if command[-1].endswith('albumworker-1'):
                    if change == 'credential':
                        data[0]['Config']['Env'] = [v.replace('synthetic-production-credential', 'other-credential') for v in data[0]['Config']['Env']]
                    else:
                        data[0]['Mounts'] = [{'Destination': '/album-secrets'}]
                return json.dumps(data).encode()
            self.runtime.execute.side_effect = inspect
            with self.assertRaises(OperationError):
                albums.require_resources(self.runtime, self.bundle, self.config)

    def test_backup_configuration_does_not_require_a_running_worker(self):
        albums.stage(self.runtime, self.bundle)
        self.runtime.execute.reset_mock()
        albums.require_configuration(self.runtime, self.bundle, self.config)
        self.runtime.execute.assert_not_called()

    def test_worker_cannot_receive_gateway_keys_network_or_credentials(self):
        for service in ({'environment': {'ALBUM_COS_ARCHIVE_SECRET_KEY': 'bad'}},
                        {'networks': {'albums-production': {}}}, {'volumes': [{'target': '/album-secrets'}]}):
            with self.assertRaises(OperationError):
                albums.verify_compose(self.config, {'services': {'worker': service}})
        albums.verify_compose(self.config, {'services': {'worker': {'environment': albums.REGISTRY}}})

    def test_live_gateway_must_match_private_mount_network_and_proxy(self):
        albums.stage(self.runtime, self.bundle)
        app = dict(line.split('=', 1) for line in (self.bundle / 'albums.env').read_text().splitlines())
        container = {'Config': {'Env': [k + '=' + v for k, v in app.items()]},
                     'NetworkSettings': {'Networks': {albums.NETWORK: {}, 'tellyouwhat-blue_tellyouwhat-backend': {'Gateway': '172.18.0.1'}}},
                     'Mounts': [{'Destination': '/album-secrets', 'Source': str(self.secrets), 'Type': 'bind', 'RW': False}]}
        self.runtime.execute.side_effect = lambda *args, **kwargs: json.dumps([container]).encode()
        albums.verify_gateway(self.runtime, self.bundle, self.config, 'blue')
        container['Mounts'][0]['RW'] = True
        with self.assertRaises(OperationError):
            albums.verify_gateway(self.runtime, self.bundle, self.config, 'blue')
        container['Mounts'][0]['RW'] = False
        container['NetworkSettings']['Networks']['tellyouwhat-blue_tellyouwhat-backend']['Gateway'] = '172.20.0.1'
        with self.assertRaises(OperationError):
            albums.verify_gateway(self.runtime, self.bundle, self.config, 'blue')


if __name__ == '__main__':
    unittest.main()
