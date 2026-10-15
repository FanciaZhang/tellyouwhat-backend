import hashlib
import io
import json
from pathlib import Path
import tempfile
import tarfile
import shutil
import unittest
from unittest.mock import Mock, patch

from ops_common import OperationError, Runtime
from ops_cos import transfer, validate_bucket
from ops_offsite import download, publish, transport, verify_recovery_bundle


class OffsiteSafetyTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / "backend"
        self.root.mkdir()
        (self.root / ".env.production").write_text("BACKUP_ENCRYPTION_KEY=" + "x" * 64 + "\n")
        self.runtime = Runtime(self.root, self.root / "backups")
        self.runtime.backups.mkdir()

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

    def test_plaintext_and_symlink_uploads_are_rejected(self):
        client = self.client()
        source = self.runtime.backups / "private.sql"
        source.write_bytes(b"plaintext")
        link = self.runtime.backups / "mysql-20261013T035318Z-abcdef.sql.gz.enc"
        link.symlink_to(source)
        for path in (source, link):
            with self.assertRaises(OperationError):
                transfer(client, "backup", "backups/", "upload", [path])
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
        source = self.runtime.backups / "mysql-20261013T035318Z-abcdef.sql.gz.enc"
        source.write_bytes(b"ciphertext")
        Path(str(source) + ".json").write_text("manifest")
        recovery = self.runtime.backups / ("recovery-" + source.name.replace(".sql.gz.enc", ".tar.gz.enc"))
        recovery.write_bytes(b"encrypted configuration")
        manifest = {"recovery_filename": recovery.name, "sha256": hashlib.sha256(source.read_bytes()).hexdigest()}
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


if __name__ == "__main__":
    unittest.main()
