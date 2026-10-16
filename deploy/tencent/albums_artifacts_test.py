import hashlib
import json
import os
from pathlib import Path
import stat
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import albums_artifacts as artifacts
from ops_common import OperationError


class AlbumsDevelopmentRecoveryTests(unittest.TestCase):
    environment = 'development'
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.root = self.base / 'backend'
        self.state = self.root / '.operations'
        self.state.mkdir(parents=True, mode=0o700)
        self.runtime = Mock(root=self.root, state=self.state)
        self.label = 'albums-' + self.environment
        self.owner = 'albums-test' if self.environment == 'development' else self.label
        self.files = artifacts.FILES if self.environment == 'development' else artifacts.PRODUCTION_FILES

    def private(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(value)
        path.chmod(0o600)

    def prepare(self):
        source = 'a' * 40
        self.folder = self.root / ('.' + self.label) / 'releases' / source
        self.folder.mkdir(parents=True)
        files = {name: b'private configuration' for name in self.files if name != 'selection.json'}
        if self.environment == 'production':
            files['runtime.env'] = (b'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums\n'
                                    b'ALBUM_API_HOST=api.albums.tellyouwhat.cn\n'
                                    b'ALBUM_COS_BUCKET=tellyouwhat-albums-production-1251995834\n')
        selection = {'source': source, 'files': {k: hashlib.sha256(v).hexdigest() for k, v in files.items()}}
        files['selection.json'] = json.dumps(selection).encode()
        for name, data in files.items():
            self.private(self.folder / name, data)
        self.private(self.state / (self.label + '.json'), json.dumps({'source': source, 'release': str(self.folder)}).encode())
        self.private(self.state / 'artifact-owners' / (self.owner + '.json'), json.dumps({'current': {'source': source, 'paths': [str(self.folder)]}}).encode())
        self.private(self.base / self.label / 'database.env', b'private database config')
        self.keys = {self.base / self.label / 'secrets' / name: ('synthetic:' + name).encode() for name in artifacts.KEYS}
        for path, data in self.keys.items():
            self.private(path, data)
        self.runtime.execute.side_effect = lambda label, command: self.keys[Path(command[-1])]

    def backup(self):
        original = Path.stat

        def metadata(path, *args, **kwargs):
            value = original(path, *args, **kwargs)
            if path in self.keys:
                fields = list(value)
                fields[0], fields[4], fields[5] = stat.S_IFREG | 0o440, 0, 65532
                return os.stat_result(fields)
            return value
        snapshot = 'checkpoint/albums-' + 'a' * 32 + '.sql.gz.enc'
        ledger = 'checkpoint/albums-deletions-' + 'b' * 32 + '.json'
        with patch.object(Path, 'stat', metadata), patch.object(artifacts, 'checkpoint', return_value={
                'backup.env': b'no decryption key', snapshot: b'encrypted SQL', snapshot + '.json': b'authenticated manifest', ledger: b'deletion fences'}):
            return artifacts.backup_configuration(self.runtime, self.base / 'output', self.environment)

    def test_unused_deployment_needs_no_new_backup_files(self):
        self.assertEqual(artifacts.backup_configuration(self.runtime, self.base / 'output', self.environment), {})
        self.runtime.execute.assert_not_called()

    def test_selected_configuration_and_keys_are_verified_after_archive_roundtrip(self):
        self.prepare()
        manifest = self.backup()
        self.assertEqual(len(manifest), 15)
        self.assertEqual(self.runtime.execute.call_count, 3)
        archive_path = self.base / 'recovery.tar'
        with tarfile.open(archive_path, 'w') as archive:
            archive.add(self.base / 'output', arcname='recovery/' + self.label)
        with tarfile.open(archive_path) as archive:
            artifacts.verify_recovery(archive, manifest, self.environment)
            bad = dict(manifest, **{'database.env': '0' * 64})
            with self.assertRaisesRegex(OperationError, 'content verification'):
                artifacts.verify_recovery(archive, bad, self.environment)
            bad = dict(manifest)
            bad.pop('secrets/apple-sign-in.p8')
            with self.assertRaisesRegex(OperationError, 'incomplete'):
                artifacts.verify_recovery(archive, bad, self.environment)
        self.assertTrue(all(p.stat().st_mode & 0o077 == 0 for p in (self.base / 'output').rglob('*') if p.is_file()))

    def test_changed_inputs_and_unregistered_selection_are_not_backed_up(self):
        self.prepare()
        (self.folder / 'runtime.env').write_bytes(b'changed')
        with self.assertRaisesRegex(OperationError, 'differs'):
            self.backup()
        self.assertFalse((self.base / 'output').exists())
        self.private(self.state / 'artifact-owners' / (self.owner + '.json'), b'{}')
        with self.assertRaisesRegex(OperationError, 'not registered'):
            self.backup()

    def test_unprotected_credentials_are_rejected(self):
        self.prepare()
        with self.assertRaisesRegex(OperationError, 'credential'):
            artifacts.backup_configuration(self.runtime, self.base / 'output', self.environment)
        self.runtime.execute.assert_not_called()


class AlbumsProductionRecoveryTests(AlbumsDevelopmentRecoveryTests):
    environment = 'production'

    def update_app(self, extra):
        path = self.folder / 'runtime.env'
        from ops_common import read_environment
        config = read_environment(path)
        config.update(line.split('=', 1) for line in extra.decode().splitlines())
        path.write_text(''.join(k + '=' + v + '\n' for k, v in config.items()))
        manifest_path = self.folder / 'selection.json'
        manifest = json.loads(manifest_path.read_text())
        manifest['files']['runtime.env'] = hashlib.sha256(path.read_bytes()).hexdigest()
        manifest_path.write_text(json.dumps(manifest))

    def test_wrong_identity_and_missing_enabled_key_block_backup(self):
        self.prepare()
        self.update_app(b'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums.dev\n')
        with self.assertRaisesRegex(OperationError, 'another environment'):
            self.backup()
        self.update_app(b'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums\nALBUM_SUBSCRIPTIONS_ENABLED=true\n')
        with self.assertRaisesRegex(OperationError, 'missing its recovery credential'):
            self.backup()
        self.update_app(b'ALBUM_SUBSCRIPTIONS_ENABLED=false\nALBUM_SHARE_KEY_FILE=/other-app/key\n')
        with self.assertRaisesRegex(OperationError, 'outside its recovery selection'):
            self.backup()
        self.runtime.execute.assert_not_called()

    def test_previous_configuration_and_optional_keys_are_included(self):
        import shutil
        self.prepare()
        previous_source = 'b' * 40
        previous = self.folder.parent / previous_source
        shutil.copytree(self.folder, previous)
        record = json.loads((previous / 'selection.json').read_text())
        record['source'] = previous_source
        (previous / 'selection.json').write_text(json.dumps(record))
        owner_path = self.state / 'artifact-owners' / (self.owner + '.json')
        owner = json.loads(owner_path.read_text())
        owner['previous'] = {'source': previous_source, 'paths': [str(previous)]}
        owner_path.write_text(json.dumps(owner))
        self.update_app(b'ALBUM_SUBSCRIPTIONS_ENABLED=true\n')
        for name in ('album-app-store.p8', 'apple-app-store-roots.pem'):
            key = self.base / self.label / 'secrets' / name
            self.keys[key] = b'synthetic key'
            self.private(key, self.keys[key])
        manifest = self.backup()
        self.assertEqual(len(manifest), 22)
        archive_path = self.base / 'production.tar'
        with tarfile.open(archive_path, 'w') as archive:
            archive.add(self.base / 'output', arcname='recovery/' + self.label)
        with tarfile.open(archive_path) as archive:
            artifacts.verify_recovery(archive, manifest, self.environment)
            with self.assertRaises(OperationError):
                artifacts.verify_recovery(archive, manifest, 'development')
            incomplete = dict(manifest)
            incomplete.pop('previous/compose.env')
            with self.assertRaisesRegex(OperationError, 'incomplete'):
                artifacts.verify_recovery(archive, incomplete, self.environment)
            incomplete = dict(manifest)
            incomplete.pop('secrets/album-app-store.p8')
            with self.assertRaisesRegex(OperationError, 'missing an enabled feature'):
                artifacts.verify_recovery(archive, incomplete, self.environment)


if __name__ == '__main__':
    unittest.main()
