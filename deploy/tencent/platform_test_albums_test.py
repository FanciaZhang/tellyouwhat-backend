import copy
import hashlib
import io
import json
import tarfile
from unittest.mock import Mock, patch
from pathlib import Path
import tempfile
import unittest

import platform_test_albums as albums
from ops_common import OperationError


class AlbumsSharedGatewayRuntimeTests(unittest.TestCase):
    def media_fixture(self, folder):
        settings = {'ALBUM_COS_BUCKET': 'tellyouwhat-albums-1251995834', 'ALBUM_COS_REGION': 'ap-guangzhou',
            'ALBUM_MAX_ORIGINAL_BYTES': '1000000000', 'ALBUM_MAX_DERIVED_BYTES': '100000000',
            'ALBUM_COS_ARCHIVE_SECRET_ID': 'synthetic-archive-id', 'ALBUM_COS_ARCHIVE_SECRET_KEY': 'synthetic-archive-key',
            'ALBUM_COS_UPLOAD_SECRET_ID': 'synthetic-upload-id', 'ALBUM_COS_UPLOAD_SECRET_KEY': 'synthetic-upload-key',
            'ALBUM_CI_VIDEO_QUEUE_ID': albums.WORKER_VALUES['ALBUM_CI_VIDEO_QUEUE_ID'],
            'ALBUM_APPLE_SIGN_IN_KEY_FILE': '/album-secrets/private.p8'}
        (folder / 'runtime.env').write_text('PLATFORM_APPS=health,journal,birthday,albums\n')
        (folder / 'albums.env').write_text(''.join(k + '=' + v + '\n' for k,v in settings.items()))
        values = albums.worker_values(settings)
        (folder / 'album-worker.env').write_text(''.join(k + '=' + v + '\n' for k,v in values.items()))
        (folder / 'compose.env').write_text('PLATFORM_ALBUM_MYSQL_PASSWORD=' + '4' * 64 + '\n')
        recipe = (Path(__file__).resolve().parents[2] / albums.WORKER_COMPOSE).read_bytes()
        (folder / albums.WORKER_COMPOSE).write_bytes(recipe)
        env = {**values, 'ALBUM_DEPLOYMENT_ENVIRONMENT': 'test',
            'ALBUM_DATABASE_DSN': 'albums_development_test:' + '4' * 64 + '@tcp(' + albums.PROJECT + '-mysql-1:3306)/albums_test?charset=utf8mb4&collation=utf8mb4_0900_ai_ci'}
        container = {'Config': {'Image': 'tellyouwhat-platform-test-albumworker:' + 'b' * 40,
            'User': '65532:65532', 'Labels': {'com.docker.compose.project': albums.PROJECT},
            'Env': [k + '=' + v for k,v in env.items()]},
            'State': {'Running': True, 'StartedAt': '2026-10-31T00:00:00Z'},
            'HostConfig': {'ReadonlyRootfs': True, 'Memory': 384 * 1024**2},
            'NetworkSettings': {'Networks': {albums.NETWORK: {}}}}
        return settings, container

    def test_media_selection_pins_recipe_and_copies_only_object_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            settings, _ = self.media_fixture(folder)
            compose = {'PLATFORM_ALBUM_WORKER_ENV_FILE': str(folder / 'album-worker.env')}
            archive = Mock()
            archive.extractfile.side_effect = lambda name: io.BytesIO((folder / name).read_bytes())
            albums.validate_worker(folder, compose, archive)
            copied = albums.worker_values(settings)
            self.assertNotIn('ALBUM_APPLE_SIGN_IN_KEY_FILE', copied)
            for key, value in [('ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS', 'fake-grant'),
                               ('ALBUM_ACCOUNT_MEDIA_ERASURE_ENABLED', 'true'), ('ALBUM_VIDEO_PROCESSING_ENABLED', 'false')]:
                with (folder / 'album-worker.env').open('a') as output:
                    output.write(key + '=' + value + '\n')
                with self.subTest(key=key), self.assertRaises(OperationError):
                    albums.validate_worker(folder, compose, archive)
                (folder / 'album-worker.env').write_text(''.join(k + '=' + v + '\n' for k,v in copied.items()))
            archive.extractfile.side_effect = lambda _: io.BytesIO(b'changed source')
            with self.assertRaises(OperationError):
                albums.validate_worker(folder, compose, archive)
            (folder / 'album-worker.env').unlink()
            with self.assertRaises(OperationError):
                albums.validate_worker(folder, compose, archive)

    def test_media_worker_requires_actual_scoping_and_successful_provider_startup(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            _, container = self.media_fixture(folder)
            startup = {'msg': 'album workers started', 'photo_processing': True, 'video_processing': True,
                       'derived_cleanup': False, 'account_media_erasure': False}
            runtime = Mock()
            def execute(label, command):
                return json.dumps([container] if label.endswith('inspect') else startup).encode()
            runtime.execute.side_effect = execute
            albums.verify_worker(runtime, folder, 'b' * 40)
            changes = [('Mounts', [{'Source': '/production', 'Destination': '/secrets'}]),
                       ('NetworkSettings', {'Networks': {albums.NETWORK: {}, 'production': {}}}),
                       ('HostConfig', {'ReadonlyRootfs': False, 'Memory': 384 * 1024**2}),
                       ('RestartCount', 1), ('State', {'Running': False}),
                       ('Config', {**container['Config'], 'Image': 'old-image'}),
                       ('Config', {**container['Config'], 'Env': container['Config']['Env'] + ['HEALTH_SCHEMA_MANIFEST_PATH=/config/schema-manifest.json']}),
                       ('Config', {**container['Config'], 'Env': container['Config']['Env'] + ['ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS=fake']})]
            for key, value in changes:
                original = copy.deepcopy(container)
                container[key] = value
                with self.subTest(key=key), self.assertRaises(OperationError):
                    albums.verify_worker(runtime, folder, 'b' * 40)
                container.clear(); container.update(original)
            startup['video_processing'] = False
            with patch.object(albums.time, 'sleep'), self.assertRaisesRegex(OperationError, 'startup checks'):
                albums.verify_worker(runtime, folder, 'b' * 40)

    def test_media_update_drains_and_recovery_starts_only_old_worker(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.media_fixture(folder)
            runtime = Mock(root=folder)
            runtime.execute.side_effect = lambda label, command, **kw: (b'[{"State":{"Running":false,"ExitCode":0}}]'
                if label.endswith('stopped') else b'')
            albums.start_worker(runtime, folder)
            commands = [call.args[1] for call in runtime.execute.call_args_list]
            self.assertEqual(commands[0], ['docker', 'stop', '--time', '40', albums.PROJECT + '-albumworker-1'])
            self.assertEqual(commands[-1][-5:], ['up', '-d', '--no-build', '--no-deps', 'albumworker'])
            self.assertNotIn('--remove-orphans', commands[-1])
            runtime.execute.side_effect = lambda label, command, **kw: (b'[{"State":{"Running":false,"ExitCode":137}}]'
                if label.endswith('stopped') else b'')
            with self.assertRaisesRegex(OperationError, 'drain cleanly'):
                albums.start_worker(runtime, folder)
            (folder / albums.WORKER_COMPOSE).unlink(); (folder / 'album-worker.env').unlink()
            (folder / 'compose.env').write_text('PLATFORM_ALBUM_SOURCE=' + 'a' * 40 + '\n')
            albums.start_worker(runtime, folder, recovering=True)
            command = runtime.execute.call_args.args[1]
            self.assertIn(str(folder / '.albums-development/releases' / ('a' * 40) / 'compose.albums-test.yaml'), command)
            self.assertEqual(command[-1], 'albumworker')
            self.assertNotIn('albumgateway', command)

    def test_sharing_transition_only_changes_reviewed_test_settings(self):
        with tempfile.TemporaryDirectory() as directory:
            before, after = Path(directory) / 'before', Path(directory) / 'after'
            before.mkdir(); after.mkdir()
            old = {'ALBUM_SUBSCRIPTIONS_ENABLED': 'true', 'ALBUM_APP_STORE_APP_APPLE_ID': '6824089160',
                   'ALBUM_APP_STORE_ISSUER_ID': '1f4f7f07-26b1-4226-9518-5bd19f8440b8',
                   'ALBUM_APP_STORE_KEY_ID': 'YTX5GYG2Z5', 'ALBUM_APP_STORE_KEY_FILE': '/album-secrets/album-app-store.p8',
                   'ALBUM_APP_STORE_ROOTS_FILE': '/album-secrets/apple-app-store-roots.pem',
                   'ALBUM_COS_BUCKET': 'tellyouwhat-albums-1251995834', 'ALBUM_SHARING_ENABLED': 'false'}
            values = {**old, 'ALBUM_SHARING_ENABLED': 'true', 'ALBUM_SHARE_SITE_ORIGIN': albums.SHARE_SITE,
                      'ALBUM_SHARE_KEY_FILE': '/album-secrets/share-key.base64'}
            def save(folder, settings):
                (folder / 'albums.env').write_text(''.join(k + '=' + v + '\n' for k, v in settings.items()))
            save(before, old); save(after, values)
            albums.require_transition(before, after)
            self.assertEqual(albums.sharing_keys(values), {'share-key.base64'})
            self.assertEqual(albums.sharing_keys(old), set())
            for key, bad in [('ALBUM_COS_BUCKET', 'production'), ('ALBUM_ACCOUNT_MEDIA_ERASURE_ENABLED', 'true'),
                             ('ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS', 'fake-grant'), ('ALBUM_SUBSCRIPTIONS_ENABLED', 'false'),
                             ('ALBUM_SHARE_SITE_ORIGIN', 'https://albums.tellyouwhat.cn'),
                             ('ALBUM_SHARE_KEY_FILE', '/production/share-key.base64'),
                             ('ALBUM_SHARE_PREVIOUS_KEY_FILE', '/album-secrets/credential-key.base64'),
                             ('ALBUM_SHARING_ENABLED', 'yes')]:
                save(after, {**values, key: bad})
                with self.subTest(key=key), self.assertRaises(OperationError):
                    albums.require_transition(before, after)
            values['ALBUM_SHARE_PREVIOUS_KEY_FILE'] = '/album-secrets/share-previous-key.base64'
            save(after, values); albums.require_transition(before, after)
            self.assertEqual(albums.sharing_keys(values), {'share-key.base64', 'share-previous-key.base64'})

    def test_schema_plan_pins_base_sql_extension_and_recovery_receipts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            original = root / '.albums-development/releases' / ('a' * 40)
            original.mkdir(parents=True)
            folder = root / 'candidate'; folder.mkdir()
            (folder / 'compose.env').write_text('PLATFORM_ALBUM_SOURCE=' + 'a' * 40 + '\n')
            overlay = Path(__file__).resolve().parents[2] / albums.OVERLAY
            (folder / albums.OVERLAY).write_bytes(overlay.read_bytes())
            base = {'migrations/0064_album_identity_retirement.sql': b'base fixture'}
            product = (overlay.parent / albums.PRODUCT_MIGRATION).read_bytes()
            def archive(target, files):
                with tarfile.open(target / 'source.tar', 'w') as output:
                    for name, body in files.items():
                        member = tarfile.TarInfo(name); member.size = len(body)
                        output.addfile(member, io.BytesIO(body))
            archive(original, base)
            archive(folder, {**base, albums.PRODUCT_MIGRATION: product,
                             'migrations/0065_journal_illustration_jobs.sql': b'unrelated'})
            baseline = '0064_album_identity_retirement.sql'
            upgraded = baseline + '\n0073_album_development_subscription_products.sql\n'
            runtime = Mock(root=root)
            runtime.execute.return_value = (baseline + '\n').encode()
            names, extension, pending = albums.schema_plan(runtime, folder, {})
            self.assertTrue(pending)
            self.assertEqual(names, upgraded.splitlines())
            self.assertEqual(extension, {albums.PRODUCT_MIGRATION: hashlib.sha256(product).hexdigest()})
            runtime.execute.return_value = upgraded.encode()
            self.assertFalse(albums.schema_plan(runtime, folder, {})[2])  # receipt committed before state write
            self.assertFalse(albums.schema_plan(runtime, folder, {'album_schema_extensions': extension})[2])
            archive(folder, base)  # Old service remains valid after a forward-only upgrade.
            self.assertFalse(albums.schema_plan(runtime, folder, {'album_schema_extensions': extension})[2])
            for actual in ('', baseline + '\n0065_journal_illustration_jobs.sql\n', upgraded + baseline + '\n'):
                runtime.execute.return_value = actual.encode()
                with self.subTest(actual=actual), self.assertRaises(OperationError):
                    albums.schema_plan(runtime, folder, {'album_schema_extensions': extension})
            runtime.execute.return_value = upgraded.encode()
            for files, state in [({**base, albums.PRODUCT_MIGRATION: b'changed'}, {}),
                                 ({**base, baseline: b'extra'}, {'album_schema_extensions': {'unknown': 'bad'}}),
                                 ({'migrations/0064_album_identity_retirement.sql': b'changed'}, {})]:
                archive(folder, files)
                with self.assertRaises(OperationError):
                    albums.schema_plan(runtime, folder, state)
            archive(folder, {**base, albums.PRODUCT_MIGRATION: product})
            previous = Path(__file__).parent / 'testdata/previous-platform-albums-test.yaml'
            (folder / albums.OVERLAY).write_bytes(previous.read_bytes())
            with self.assertRaisesRegex(OperationError, 'isolated job'):
                albums.schema_plan(runtime, folder, {})

            (folder / albums.OVERLAY).write_bytes(overlay.read_bytes())
            copies = (overlay.parent / albums.COPY_MIGRATION).read_bytes()
            archive(folder, {**base, albums.PRODUCT_MIGRATION: product, albums.COPY_MIGRATION: copies})
            all_names = upgraded.splitlines() + [Path(albums.COPY_MIGRATION).name]
            for actual in (baseline.splitlines(), upgraded.splitlines(), all_names):
                runtime.execute.return_value = ('\n'.join(actual) + '\n').encode()
                names, copied_extension, pending = albums.schema_plan(runtime, folder, {})
                self.assertEqual(names, all_names)
                self.assertEqual(pending, actual != all_names)
                self.assertEqual(copied_extension, {**extension, albums.COPY_MIGRATION: hashlib.sha256(copies).hexdigest()})
            archive(folder, base)
            self.assertFalse(albums.schema_plan(runtime, folder, {'album_schema_extensions': copied_extension})[2])
            for files in ({**base, albums.COPY_MIGRATION: copies},
                          {**base, albums.PRODUCT_MIGRATION: product, albums.COPY_MIGRATION: b'changed'}):
                archive(folder, files)
                with self.assertRaises(OperationError):
                    albums.schema_plan(runtime, folder, {})
            archive(folder, {**base, albums.PRODUCT_MIGRATION: product, albums.COPY_MIGRATION: copies})
            for actual in ([baseline, Path(albums.COPY_MIGRATION).name], upgraded.splitlines()):
                runtime.execute.return_value = ('\n'.join(actual) + '\n').encode()
                with self.assertRaises(OperationError):
                    albums.schema_plan(runtime, folder, {'album_schema_extensions': copied_extension})

            registered = Path(__file__).resolve().parent / 'testdata/registered-recovery-schema'
            metadata = {name: ((registered / Path(name).name) if (registered / Path(name).name).is_file() else (overlay.parent / name)).read_bytes() for name in albums.SCHEMA_MIGRATIONS}
            receipts = [(name, hashlib.sha256(body).hexdigest()) for name, body in metadata.items()]
            all_names = [baseline] + [Path(name).name for name in metadata]
            archive(folder, {**base, **metadata})
            for recorded in range(len(receipts) + 1):
                state = {'album_schema_extensions': dict(receipts[:recorded])}
                for applied in range(len(receipts) + 1):
                    runtime.execute.return_value = ('\n'.join(all_names[:applied + 1]) + '\n').encode()
                    with self.subTest(recorded=recorded, applied=applied):
                        if applied < recorded:
                            with self.assertRaises(OperationError):
                                albums.schema_plan(runtime, folder, state)
                        else:
                            names, extension, pending = albums.schema_plan(runtime, folder, state)
                            self.assertEqual(names, all_names)
                            self.assertEqual(extension, dict(receipts))
                            self.assertEqual(pending, applied != len(receipts))

            runtime.execute.return_value = ('\n'.join(all_names) + '\n').encode()
            for name in albums.SCHEMA_MIGRATIONS[2:]:
                with self.subTest(tampered=name):
                    archive(folder, {**base, **metadata, name: metadata[name] + b'\n-- changed'})
                    runtime.execute.reset_mock()
                    with self.assertRaises(OperationError):
                        albums.schema_plan(runtime, folder, {})
                    runtime.execute.assert_not_called()
            for name in albums.SCHEMA_MIGRATIONS[:-1]:
                with self.subTest(missing=name):
                    archive(folder, {**base, **{k: v for k, v in metadata.items() if k != name}})
                    runtime.execute.reset_mock()
                    with self.assertRaises(OperationError):
                        albums.schema_plan(runtime, folder, {})
                    runtime.execute.assert_not_called()

            archive(folder, {**base, **metadata})
            gapped = dict(receipts); del gapped[albums.SCHEMA_MIGRATIONS[2]]
            runtime.execute.reset_mock()
            with self.assertRaises(OperationError):
                albums.schema_plan(runtime, folder, {'album_schema_extensions': gapped})
            runtime.execute.assert_not_called()
            runtime.execute.return_value = ('\n'.join([n for n in all_names if n != Path(albums.SCHEMA_MIGRATIONS[2]).name]) + '\n').encode()
            with self.assertRaises(OperationError):
                albums.schema_plan(runtime, folder, {})
            archive(folder, base)
            runtime.execute.return_value = ('\n'.join(all_names) + '\n').encode()
            self.assertFalse(albums.schema_plan(runtime, folder, {'album_schema_extensions': dict(receipts)})[2])

    def test_actual_connections_and_read_only_secret_mount_are_required(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            (folder / 'albums.env').write_text('')
            (folder / 'compose.env').write_text('PLATFORM_ALBUM_MYSQL_PASSWORD=' + '4' * 64 +
                                               '\nPLATFORM_ALBUM_REDIS_PASSWORD=' + '5' * 64 + '\n')
            env = {**albums.REGISTRY, 'ALBUM_DEPLOYMENT_ENVIRONMENT': 'test',
                   'DATABASE_DSN': 'platform_test:synthetic@tcp(tellyouwhat-platform-test-mysql-1:3306)/platform_test?parseTime=true',
                   'REDIS_URL': 'redis://:synthetic@tellyouwhat-platform-test-redis-1:6379/0',
                   'ALBUM_DATABASE_DSN': 'albums_development_test:' + '4' * 64 + '@tcp(tellyouwhat-albums-development-mysql-1:3306)/albums_test?charset=utf8mb4&collation=utf8mb4_0900_ai_ci',
                   'ALBUM_REDIS_URL': 'redis://:' + '5' * 64 + '@tellyouwhat-albums-development-redis-1:6379/0'}
            container = {'Config': {'Env': [k + '=' + v for k, v in env.items()]},
                         'NetworkSettings': {'Networks': {'tellyouwhat-platform-test_platform-test': {}, albums.NETWORK: {}}},
                         'Mounts': [{'Type': 'bind', 'Source': '/opt/tellyouwhat/albums-development/secrets',
                                     'Destination': '/album-secrets', 'RW': False}]}
            albums.verify_gateway(container, folder)
            for field, bad in [('DATABASE_DSN', 'platform_test:synthetic@tcp(mysql:3306)/platform_test'),
                               ('ALBUM_REDIS_URL', 'redis://localhost:6379/0'),
                               ('ALBUM_APPLE_BUNDLE_ID', 'cn.tellyouwhat.albums'),
                               ('ALBUM_DEPLOYMENT_ENVIRONMENT', 'production')]:
                changed = copy.deepcopy(container)
                changed['Config']['Env'] = [k + '=' + v for k, v in {**env, field: bad}.items()]
                with self.subTest(field=field), self.assertRaises(OperationError):
                    albums.verify_gateway(changed, folder)
            for field, bad in [('RW', True), ('Source', '/opt/tellyouwhat/albums-production/secrets')]:
                changed = copy.deepcopy(container)
                changed['Mounts'][0][field] = bad
                with self.subTest(field=field), self.assertRaises(OperationError):
                    albums.verify_gateway(changed, folder)
            container['NetworkSettings']['Networks']['production'] = {}
            with self.assertRaises(OperationError):
                albums.verify_gateway(container, folder)
            container['NetworkSettings']['Networks'].pop('production')
            settings = {'ALBUM_SHARING_ENABLED': 'true', 'ALBUM_SUBSCRIPTIONS_ENABLED': 'true',
                        'ALBUM_SHARE_SITE_ORIGIN': albums.SHARE_SITE, 'ALBUM_SHARE_KEY_FILE': '/album-secrets/share-key.base64'}
            (folder / 'albums.env').write_text(''.join(k + '=' + v + '\n' for k, v in settings.items()))
            with self.assertRaises(OperationError):
                albums.verify_gateway(container, folder)
            container['Config']['Env'] += [k + '=' + v for k, v in settings.items()]
            albums.verify_gateway(container, folder)
            container['Config']['Env'].append('ALBUM_SHARE_SITE_ORIGIN=https://albums.tellyouwhat.cn')
            with self.assertRaises(OperationError):
                albums.verify_gateway(container, folder)


if __name__ == '__main__':
    unittest.main()
