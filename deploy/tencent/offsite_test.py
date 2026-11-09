import hashlib
import io
import json
from pathlib import Path
import tempfile
import tarfile
import shutil
import time
import unittest
from unittest.mock import Mock, patch

from ops_common import OperationError, Runtime
from ops_cos import transfer, validate_bucket
from ops_offsite import download, publish, transport, verify_recovery_bundle, create_recovery_bundle
from ops_backup import file_digest, manifest_signature


class MemoryCOS:
    def __init__(self, corrupt=None, after_put=None):
        self.corrupt, self.after_put = corrupt, after_put
        self.objects, self.puts, self.streams = {}, [], []

    def put_object(self, **kwargs):
        self.puts.append(kwargs['Key'])
        self.objects[kwargs['Key']] = kwargs['Body'].read()
        if self.after_put:
            self.after_put(len(self.puts))

    def get_object(self, **kwargs):
        data = self.objects[kwargs['Key']]
        if len(self.puts) == self.corrupt:
            data = data[:-1] + bytes([data[-1] ^ 1])
        body = io.BytesIO(data)
        self.streams.append(body)
        return {'Body': Mock(get_raw_stream=Mock(return_value=body))}


class OffsiteSafetyTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / "backend"
        self.root.mkdir()
        (self.root / ".env.production").write_text("MYSQL_DATABASE=offsite_fixture\nBACKUP_ENCRYPTION_KEY=" + "x" * 64 + "\n")
        self.runtime = Runtime(self.root, self.root / "backups")
        self.runtime.backups.mkdir()

    def publication(self):
        source = self.runtime.backups / 'mysql-20261102T000000Z-abcdef.sql.gz.enc'
        recovery = source.parent / ('recovery-' + source.name.replace('.sql.gz.enc', '.tar.gz.enc'))
        metadata = Path(str(source) + '.json')
        source.write_bytes(b'controlled encrypted database')
        recovery.write_bytes(b'controlled encrypted recovery selection')
        manifest = {'filename': source.name, 'database': 'offsite_fixture', 'scope': 'control-plane',
                    'created_at': int(time.time()), 'table_rows': {'apps': 1},
                    'sha256': file_digest(source), 'recovery_filename': recovery.name,
                    'recovery_sha256': file_digest(recovery)}
        metadata.write_text(json.dumps({'manifest': manifest, 'hmac_sha256': manifest_signature('x' * 64, manifest)}))
        return [source, recovery, metadata], manifest

    def bridge(self, client):
        def send(runtime, action, paths, **options):
            self.assertEqual(action, 'upload')
            return {'bucket': 'fixture', 'objects': transfer(client, 'fixture', 'backups/', action, paths, **options)}
        return patch('ops_offsite.transport', side_effect=send)

    def test_failed_payload_readback_stops_before_publishing_the_inventory(self):
        for stage in (1, 2):
            with self.subTest(stage=stage):
                paths, manifest = self.publication()
                before = [p.read_bytes() for p in paths]
                client = MemoryCOS(corrupt=stage)
                with self.bridge(client), self.assertRaises(OperationError):
                    publish(self.runtime, paths[0], manifest)
                self.assertEqual(client.puts, ['backups/' + p.name for p in paths[:stage]])
                self.assertEqual([p.read_bytes() for p in paths], before)
                self.assertTrue(all(s.closed for s in client.streams))
                self.assertFalse((self.runtime.state / 'offsite.json').exists())

    def test_matching_authenticated_inputs_publish_the_inventory_last(self):
        paths, manifest = self.publication()
        client = MemoryCOS()
        with self.bridge(client):
            result = publish(self.runtime, paths[0], manifest)
        self.assertTrue(result['passed'])
        self.assertEqual(result['verified_objects'], 3)
        self.assertEqual(client.puts, ['backups/' + p.name for p in paths])
        self.assertTrue(all(s.closed for s in client.streams))
        self.assertTrue(all(client.objects['backups/' + p.name] == p.read_bytes() for p in paths))

    def test_callers_late_changes_cannot_rebind_the_verified_publication(self):
        paths, manifest = self.publication()
        expected = manifest['sha256']
        def change(stage):
            if stage == 1:
                manifest['sha256'] = '0' * 64
                manifest['recovery_scope'] = 'production'
        with self.bridge(MemoryCOS(after_put=change)):
            result = publish(self.runtime, paths[0], manifest)
        self.assertEqual(result['sha256'], expected)
        self.assertEqual(result['operation'], 'offsite')

    def test_private_transport_request_carries_the_content_binding(self):
        from ops_offsite import config_path
        paths, _ = self.publication()
        expected = {p.name: file_digest(p) for p in paths}
        interpreter = self.runtime.state / 'backup-venv/bin/python'
        interpreter.parent.mkdir(parents=True)
        interpreter.touch()
        configuration = config_path(self.runtime)
        configuration.parent.mkdir()
        configuration.write_text('synthetic private transport configuration')
        with patch.object(self.runtime, 'execute', return_value=b'{"passed":true,"objects":[]}') as execute:
            transport(self.runtime, 'upload', paths, expected=expected)
        request = json.loads(execute.call_args.kwargs['input'])
        self.assertEqual(request['expected'], expected)
        self.assertEqual(request['paths'], [str(p) for p in paths])
        self.assertEqual(request['action'], 'upload')

    def test_changed_later_input_prevents_the_inventory_upload(self):
        for change in ('recovery', 'replace-source', 'same-bytes'):
            with self.subTest(change=change):
                paths, manifest = self.publication()
                def modify(stage):
                    if stage != 1:
                        return
                    if change == 'recovery':
                        paths[1].write_bytes(b'new recovery selection')
                    elif change == 'replace-source':
                        replacement = paths[0].with_suffix('.replacement')
                        replacement.write_bytes(b'replaced encrypted database')
                        replacement.replace(paths[0])
                    else:
                        paths[0].write_bytes(paths[0].read_bytes())
                client = MemoryCOS(after_put=modify)
                with self.bridge(client), self.assertRaises(OperationError):
                    publish(self.runtime, paths[0], manifest)
                self.assertEqual(client.puts, ['backups/' + paths[0].name])
                self.assertTrue(all(p.exists() for p in paths))
                self.assertFalse((self.runtime.state / 'offsite.json').exists())

    def test_untrusted_inventory_or_callers_manifest_never_starts_upload(self):
        for change in ('hmac', 'source', 'caller'):
            with self.subTest(change=change):
                paths, manifest = self.publication()
                if change == 'hmac':
                    envelope = json.loads(paths[-1].read_text())
                    envelope['hmac_sha256'] = '0' * 64
                    paths[-1].write_text(json.dumps(envelope))
                elif change == 'source':
                    paths[0].write_bytes(b'not the authenticated content')
                else:
                    manifest = dict(manifest, sha256='0' * 64)
                with patch('ops_offsite.transport') as send, self.assertRaises(OperationError):
                    publish(self.runtime, paths[0], manifest)
                send.assert_not_called()

    def test_duplicate_receipts_and_local_changes_cannot_record_success(self):
        for change in ('duplicate', 'local', 'size'):
            with self.subTest(change=change):
                paths, manifest = self.publication()
                def send(runtime, action, inputs, **options):
                    objects = [{'filename': p.name, 'sha256': file_digest(p), 'bytes': p.stat().st_size} for p in inputs]
                    if change == 'duplicate':
                        objects.append(dict(objects[0]))
                    elif change == 'local':
                        paths[0].write_bytes(b'changed after transfer')
                    else:
                        objects[0]['bytes'] += 1
                    return {'bucket': 'fixture', 'objects': objects}
                with patch('ops_offsite.transport', side_effect=send), self.assertRaises(OperationError):
                    publish(self.runtime, paths[0], manifest)
                self.assertFalse((self.runtime.state / 'offsite.json').exists())

    def test_sdk_upload_requires_complete_authenticated_content_expectations(self):
        paths, _ = self.publication()
        expected = {p.name: file_digest(p) for p in paths}
        for wrong in (None, {}, {paths[0].name: expected[paths[0].name]},
                      dict(expected, extra='0' * 64), dict(expected, **{paths[0].name: '0' * 64})):
            with self.subTest(expected=wrong):
                client = MemoryCOS()
                with self.assertRaises(OperationError):
                    transfer(client, 'fixture', 'backups/', 'upload', paths, expected=wrong)
                self.assertEqual(client.puts, [])

    def client(self, public=False, days=14):
        client = Mock()
        client.get_bucket_acl.return_value = {"Owner": {"ID": "owner"}, "Grant": [{"Grantee": {"ID": "owner"}}]}
        if public:
            client.get_bucket_acl.return_value["Grant"].append({"Grantee": {"URI": "AllUsers"}})
        client.get_bucket_lifecycle.return_value = {"Rule": [{"Status": "Enabled", "Filter": {"Prefix": "backups/"}, "Expiration": {"Days": days}}]}
        return client

    def test_public_bucket_and_wrong_retention_are_rejected_before_transfer(self):
        for client in (self.client(public=True), self.client(days=1), self.client(days=30)):
            with self.subTest(client=client), self.assertRaises(OperationError):
                validate_bucket(client, "private-backup", "backups/")
            client.put_object.assert_not_called()
        validate_bucket(self.client(), "private-backup", "backups/")

    def test_production_checkpoint_skips_independent_test_and_development_readers(self):
        with patch('test_routes.backup_configuration', side_effect=OperationError('invalid test routing')), \
                patch('platform_test_artifacts.backup_configuration', side_effect=OperationError('new test format')), \
                patch('apple_delivery_artifacts.backup_configuration', side_effect=OperationError('unrelated package')), \
                patch('journal_development_artifacts.backup_configuration', side_effect=OperationError('unrelated journal')), \
                patch('albums_artifacts.backup_configuration', return_value={}) as albums:
            result = create_recovery_bundle(self.runtime, 'mysql-20261023T000000Z-abcdef.sql.gz.enc', scope='production')
            self.assertEqual(result['recovery_platform_test'], {})
            self.assertEqual(result['recovery_apple_delivery'], {})
            self.assertEqual(albums.call_args.args[-1], 'production')
            self.assertEqual(albums.call_count, 1)
            with self.assertRaisesRegex(OperationError, 'invalid test routing'):
                create_recovery_bundle(self.runtime, 'mysql-20261023T000001Z-abcdef.sql.gz.enc')

    def test_plaintext_and_symlink_uploads_are_rejected(self):
        client = self.client()
        source = self.runtime.backups / "private.sql"
        source.write_bytes(b"plaintext")
        link = self.runtime.backups / "mysql-20261013T035318Z-abcdef.sql.gz.enc"
        link.symlink_to(source)
        for path in (source, link):
            with self.assertRaisesRegex(OperationError, 'accepts only owned'):
                transfer(client, "backup", "backups/", "upload", [path], expected={path.name: file_digest(path)})
        client.put_object.assert_not_called()

    def test_download_stream_is_closed_and_file_permissions_are_private(self):
        client = self.client()
        body = io.BytesIO(b"ciphertext")
        client.get_object.return_value = {"Body": Mock(get_raw_stream=Mock(return_value=body))}
        destination = self.runtime.backups / "mysql-20261013T035318Z-abcdef.sql.gz.enc"
        result = transfer(client, "backup", "backups/", "download", [destination])
        self.assertEqual(result[0]["sha256"], hashlib.sha256(b"ciphertext").hexdigest())
        self.assertTrue(body.closed)
        self.assertEqual(destination.stat().st_mode & 0o777, 0o600)

    def test_offsite_success_requires_readback_of_every_object(self):
        paths, manifest = self.publication()
        source = paths[0]
        with patch("ops_offsite.transport", return_value={"bucket": "private", "objects": []}):
            with self.assertRaisesRegex(OperationError, "readback differs"):
                publish(self.runtime, source, manifest)
        self.assertFalse((self.runtime.state / "offsite.json").exists())

    def test_required_offsite_configuration_cannot_silently_fall_back_to_local(self):
        with self.assertRaisesRegex(OperationError, "missing"):
            transport(self.runtime, "upload", [])

    def test_tampered_remote_manifest_is_rejected_before_database_download(self):
        name = "mysql-20261013T035318Z-abcdef.sql.gz.enc"
        directory = self.root / "download"
        directory.mkdir()
        def fetch(_runtime, _action, paths):
            paths[0].write_text(json.dumps({"manifest": {"filename": name}, "hmac_sha256": "0" * 64}))
            return {}
        with patch("ops_offsite.transport", side_effect=fetch) as fetcher:
            with self.assertRaisesRegex(OperationError, "authentication failed"):
                download(self.runtime, name, directory)
        self.assertEqual(fetcher.call_count, 1)

    def test_signed_but_invalid_remote_manifest_stops_before_payload_download(self):
        changes = (
            {'database': 'another_application'},
            {'recovery_filename': 'recovery-mysql-20261101T000000Z-abcdef.tar.gz.enc'},
            {'recovery_filename': ''},
            {'recovery_sha256': 'invalid'},
            {'sha256': 'invalid'},
            {'recovery_scope': 'another_environment'},
            {'scope': 'user-media'},
            {'created_at': True},
            {'table_rows': {'apps': -1}},
            {'included_data_tables': ['apps'], 'excluded_data_tables': ['apps']},
        )
        for change in changes:
            with self.subTest(change=change), tempfile.TemporaryDirectory(dir=self.root) as directory:
                paths, manifest = self.publication()
                manifest.update(change)
                envelope = json.dumps({'manifest': manifest, 'hmac_sha256': manifest_signature('x' * 64, manifest)})
                client = MemoryCOS()
                client.objects = {'backups/' + p.name: p.read_bytes() for p in paths[:2]}
                if manifest['recovery_filename']:
                    client.objects['backups/' + manifest['recovery_filename']] = paths[1].read_bytes()
                client.objects['backups/' + paths[2].name] = envelope.encode()
                def fetch(_runtime, action, destinations):
                    return transfer(client, 'fixture', 'backups/', action, destinations)
                with patch('ops_offsite.transport', side_effect=fetch) as fetcher:
                    with self.assertRaises(OperationError):
                        download(self.runtime, paths[0].name, directory)
                self.assertEqual(fetcher.call_count, 1)
                self.assertEqual([p.name for p in Path(directory).iterdir()], [paths[2].name])
                self.assertTrue(all(s.closed for s in client.streams))

    def test_remote_download_preserves_valid_scopes_and_checks_both_payloads(self):
        for scope, corrupt in ((None, None), ('host', None), ('production', None), ('host', 0), ('host', 1)):
            with self.subTest(scope=scope, corrupt=corrupt), tempfile.TemporaryDirectory(dir=self.root) as directory:
                paths, manifest = self.publication()
                if scope is None:
                    manifest.pop('scope')
                else:
                    manifest['recovery_scope'] = scope
                paths[2].write_text(json.dumps({'manifest': manifest, 'hmac_sha256': manifest_signature('x' * 64, manifest)}))
                client = MemoryCOS()
                client.objects = {'backups/' + p.name: p.read_bytes() for p in paths}
                if corrupt is not None:
                    client.objects['backups/' + paths[corrupt].name] += b'changed encrypted payload'
                def fetch(_runtime, action, destinations):
                    return transfer(client, 'fixture', 'backups/', action, destinations)
                with patch('ops_offsite.transport', side_effect=fetch) as fetcher:
                    if corrupt is None:
                        result = download(self.runtime, paths[0].name, directory)
                        self.assertEqual(result, Path(directory) / paths[0].name)
                        self.assertTrue(all((Path(directory) / p.name).read_bytes() == p.read_bytes() for p in paths))
                    else:
                        with self.assertRaisesRegex(OperationError, 'content verification failed'):
                            download(self.runtime, paths[0].name, directory)
                self.assertEqual(fetcher.call_count, 2)
                self.assertTrue(all(s.closed for s in client.streams))

    def test_test_route_files_are_verified_against_the_authenticated_manifest(self):
        from release import KEY_FILES
        bundle = self.root / 'fixture'
        current = bundle / 'current'
        (current / 'secrets').mkdir(parents=True)
        (current / '.env.production').write_text('APP_ENV=production\n')
        (current / 'compose.production.yaml').write_text('services: {}\n')
        for name in KEY_FILES: (current / 'secrets' / name).write_text('synthetic signing fixture')
        (bundle / 'selection.json').write_text('{"slots":{"current":{"images":{}}}}')
        routes = bundle / 'test-routing'
        routes.mkdir()
        files = {'Caddyfile': b'import /etc/caddy/tellyouwhat-test.caddy\n',
                 'test.caddy': b'api-test.journal.example {}\n', 'state.json': b'{"apps":["journal"]}'}
        for name, data in files.items(): (routes / name).write_bytes(data)
        encrypted = self.runtime.backups / 'recovery-fixture.enc'
        def archive():
            with tarfile.open(encrypted, 'w:gz') as output: output.add(bundle, arcname='recovery')
        manifest = {'recovery_filename': encrypted.name, 'recovery_images': {'current': {}},
                    'recovery_test_routing': {name: hashlib.sha256(data).hexdigest() for name, data in files.items()}}
        def decrypt(_runtime, source, target, **kwargs): shutil.copyfile(source, target)
        archive()
        with patch('ops_backup.crypt', side_effect=decrypt):
            self.assertTrue(verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', manifest))
            (routes / 'test.caddy').write_text('changed configuration')
            archive()
            with self.assertRaisesRegex(OperationError, 'authenticated manifest'):
                verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', manifest)
            (routes / 'test.caddy').unlink()
            archive()
            with self.assertRaisesRegex(OperationError, 'missing'):
                verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', manifest)
            legacy = dict(manifest)
            legacy.pop('recovery_test_routing')
            self.assertTrue(verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', legacy))
            (current / '.env.production').write_text('APP_ENV=production\nJOURNAL_RECORDING_ENABLED=false\n'
                                                    'JOURNAL_RECORDING_ROOT=/opt/tellyouwhat/journal-recordings/production\n')
            archive()
            with self.assertRaisesRegex(OperationError, 'recording recovery mount'):
                verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', legacy)
            (current / 'compose.journal-recording.yaml').write_text('services: {}\n')
            archive()
            self.assertTrue(verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', legacy))
            (current / '.env.production').write_text('APP_ENV=production\nPLATFORM_APPS=health,journal,birthday,albums\n'
                                                    'ALBUM_RESOURCE_SOURCE=' + 'a' * 40 + '\n')
            archive()
            with self.assertRaisesRegex(OperationError, 'full production checkpoint'):
                verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', legacy)
            album = bundle / 'albums-production'
            album.mkdir()
            (album / 'state.json').write_text(json.dumps({'source': 'a' * 40}))
            matched = dict(legacy, recovery_albums_production={'fixture': 'verified separately'})
            with patch('albums_artifacts.verify_recovery'):
                archive()
                with self.assertRaisesRegex(OperationError, 'configuration is missing'):
                    verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', matched)
                (current / 'albums.env').write_text('ALBUM_API_HOST=api.albums.tellyouwhat.cn\n')
                (current / 'compose.shared-albums-production.yaml').write_text('services: {}\n')
                archive()
                self.assertTrue(verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', matched))
                (album / 'state.json').write_text(json.dumps({'source': 'b' * 40}))
                archive()
                with self.assertRaisesRegex(OperationError, 'differs from its data selection'):
                    verify_recovery_bundle(self.runtime, self.runtime.backups / 'snapshot.enc', matched)


if __name__ == "__main__":
    unittest.main()
