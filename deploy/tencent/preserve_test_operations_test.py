import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
from types import SimpleNamespace
import unittest

from ops_common import OperationError
from preserve_test_operations import FILES, preserve


class PreserveTestOperationsTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.state = self.root / '.operations'
        self.state.mkdir(mode=0o700)
        self.runtime = SimpleNamespace(root=self.root, state=self.state)
        self.bundle = self.root / ('.incoming-' + 'a' * 40)
        self.installed = self.root / 'deploy/tencent'
        self.staged = self.bundle / 'deploy/tencent'
        self.installed.mkdir(parents=True)
        self.staged.mkdir(parents=True)
        self.source = 'b' * 40
        self.folder = self.root / '.platform-test/releases' / self.source
        self.folder.mkdir(parents=True, mode=0o700)
        for folder in [self.folder.parent, self.folder.parent.parent]:
            folder.chmod(0o700)
        self.private(self.state / 'platform-test.json', {'source': self.source, 'release': str(self.folder)})
        (self.state / 'artifact-owners').mkdir(mode=0o700)
        self.private(self.state / 'artifact-owners/platform-test.json',
                     {'current': {'source': self.source, 'paths': [str(self.folder)]}})
        for folder in [self.installed, self.staged]:
            (folder / 'ops_backup.py').write_bytes(b'matched backup policy')
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name in FILES:
                data = ('registered ' + name).encode()
                (self.installed / name).write_bytes(data)
                (self.staged / name).write_bytes(b'older validator')
                member = tarfile.TarInfo('deploy/tencent/' + name)
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
        (self.folder / 'source.tar').chmod(0o600)
        self.private(self.folder / 'selection.json', {'source': self.source,
            'files': {'source.tar': hashlib.sha256((self.folder / 'source.tar').read_bytes()).hexdigest()}})

    def private(self, path, value):
        path.write_text(json.dumps(value))
        path.chmod(0o600)

    def test_preserves_exact_registered_tools_without_changing_host(self):
        result = preserve(self.runtime, self.bundle)
        self.assertEqual(result['source'], self.source)
        self.assertEqual(set(result['files']), set(FILES))
        for name in FILES:
            self.assertEqual((self.staged / name).read_bytes(), (self.installed / name).read_bytes())
        self.assertEqual(json.loads((self.bundle / 'test-operations.json').read_text()), result)

    def test_unregistered_or_changed_tools_fail_before_staging(self):
        for changed in ['owner', 'archive', 'installed', 'backup', 'symlink']:
            with self.subTest(changed=changed):
                self.setUp()
                if changed == 'owner':
                    self.private(self.state / 'artifact-owners/platform-test.json', {'current': {'source': 'c' * 40}})
                elif changed == 'archive':
                    with (self.folder / 'source.tar').open('ab') as output:
                        output.write(b'changed')
                elif changed == 'installed':
                    (self.installed / FILES[-1]).write_bytes(b'changed')
                elif changed == 'backup':
                    (self.staged / 'ops_backup.py').write_bytes(b'different policy')
                else:
                    (self.staged / FILES[-1]).unlink()
                    (self.staged / FILES[-1]).symlink_to(self.installed / FILES[-1])
                with self.assertRaises(OperationError):
                    preserve(self.runtime, self.bundle)
                self.assertEqual((self.staged / FILES[0]).read_bytes(), b'older validator')

    def test_production_without_test_registration_needs_no_overlay(self):
        (self.state / 'platform-test.json').unlink()
        self.assertEqual(preserve(self.runtime, self.bundle), {})

    def test_rejects_unselected_bundle(self):
        with self.assertRaises(OperationError):
            preserve(self.runtime, self.root / '.incoming-other')
