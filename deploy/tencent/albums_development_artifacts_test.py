import hashlib
import json
import os
from pathlib import Path
import stat
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import albums_development_artifacts as artifacts
from ops_common import OperationError


class AlbumsDevelopmentRecoveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.root = self.base / 'backend'
        self.state = self.root / '.operations'
        self.state.mkdir(parents=True, mode=0o700)
        self.runtime = Mock(root=self.root, state=self.state)

    def private(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(value)
        path.chmod(0o600)

    def prepare(self):
        source = 'a' * 40
        self.folder = self.root / '.albums-development/releases' / source
        self.folder.mkdir(parents=True)
        files = {name: b'private configuration' for name in artifacts.FILES if name != 'selection.json'}
        selection = {'source': source, 'files': {k: hashlib.sha256(v).hexdigest() for k, v in files.items()}}
        files['selection.json'] = json.dumps(selection).encode()
        for name, data in files.items():
            self.private(self.folder / name, data)
        self.private(self.state / 'albums-development.json', json.dumps({'source': source, 'release': str(self.folder)}).encode())
        self.private(self.state / 'artifact-owners/albums-test.json', json.dumps({'current': {'source': source, 'paths': [str(self.folder)]}}).encode())
        self.private(self.base / 'albums-development/database.env', b'private database config')
        self.keys = {self.base / 'albums-development/secrets' / name: ('synthetic:' + name).encode() for name in artifacts.KEYS}
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
        with patch.object(Path, 'stat', metadata):
            return artifacts.backup_configuration(self.runtime, self.base / 'output')

    def test_unused_deployment_needs_no_new_backup_files(self):
        self.assertEqual(artifacts.backup_configuration(self.runtime, self.base / 'output'), {})
        self.runtime.execute.assert_not_called()

    def test_selected_configuration_and_keys_are_verified_after_archive_roundtrip(self):
        self.prepare()
        manifest = self.backup()
        self.assertEqual(len(manifest), 11)
        self.assertEqual(self.runtime.execute.call_count, 3)
        archive_path = self.base / 'recovery.tar'
        with tarfile.open(archive_path, 'w') as archive:
            archive.add(self.base / 'output', arcname='recovery/albums-development')
        with tarfile.open(archive_path) as archive:
            artifacts.verify_recovery(archive, manifest)
            bad = dict(manifest, **{'database.env': '0' * 64})
            with self.assertRaisesRegex(OperationError, 'content verification'):
                artifacts.verify_recovery(archive, bad)
            bad = dict(manifest)
            bad.pop('secrets/apple-sign-in.p8')
            with self.assertRaisesRegex(OperationError, 'incomplete'):
                artifacts.verify_recovery(archive, bad)
        self.assertTrue(all(p.stat().st_mode & 0o077 == 0 for p in (self.base / 'output').rglob('*') if p.is_file()))

    def test_changed_inputs_and_unregistered_selection_are_not_backed_up(self):
        self.prepare()
        (self.folder / 'runtime.env').write_bytes(b'changed')
        with self.assertRaisesRegex(OperationError, 'differs'):
            self.backup()
        self.assertFalse((self.base / 'output').exists())
        self.private(self.state / 'artifact-owners/albums-test.json', b'{}')
        with self.assertRaisesRegex(OperationError, 'not registered'):
            self.backup()

    def test_unprotected_credentials_are_rejected(self):
        self.prepare()
        with self.assertRaisesRegex(OperationError, 'credential'):
            artifacts.backup_configuration(self.runtime, self.base / 'output')
        self.runtime.execute.assert_not_called()


if __name__ == '__main__':
    unittest.main()
