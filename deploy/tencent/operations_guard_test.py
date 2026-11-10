import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from ops_common import OperationError
from operations_guard import SHARED, verify


class OperationsGuardTests(unittest.TestCase):
    def test_matching_toolchain_passes_and_changed_missing_or_linked_files_are_rejected(self):
        for fault in (None, 'installed-operator-group-write', 'old-bundle', 'old-installed', 'missing', 'symlink', 'writable', 'world-writable-installed'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as directory:
                base = Path(directory)
                root, bundle, registered = (base / n for n in ('host', 'candidate', 'registered'))
                for path in (root / 'deploy/tencent', bundle / 'deploy/tencent', registered, root / '.operations'):
                    path.mkdir(parents=True)
                runtime = SimpleNamespace(root=root, state=root / '.operations')
                runtime.state.chmod(0o700)
                (runtime.state / 'platform-test.json').write_text(json.dumps({'source': 'a' * 40}))
                (runtime.state / 'platform-test.json').chmod(0o600)
                with tarfile.open(registered / 'source.tar', 'w') as archive:
                    for name in SHARED:
                        data = ('registered ' + name).encode()
                        member = tarfile.TarInfo('deploy/tencent/' + name); member.size = len(data)
                        archive.addfile(member, io.BytesIO(data))
                        for folder in (root, bundle):
                            (folder / 'deploy/tencent' / name).write_bytes(data)
                victim = bundle / 'deploy/tencent/ops_backup.py'
                if fault == 'old-bundle': victim.write_text('obsolete backup API')
                if fault == 'old-installed': (root / 'deploy/tencent/ops_backup.py').write_text('obsolete backup API')
                if fault == 'missing': victim.unlink()
                if fault == 'symlink': victim.unlink(); victim.symlink_to(root / 'deploy/tencent/ops_backup.py')
                if fault == 'writable': victim.chmod(0o664)
                if fault == 'installed-operator-group-write': (root / 'deploy/tencent/ops_backup.py').chmod(0o664)
                if fault == 'world-writable-installed': (root / 'deploy/tencent/ops_backup.py').chmod(0o666)
                before = {p: p.read_bytes() for p in (root / 'deploy/tencent').iterdir()}
                with patch('operations_guard.selection', return_value=(registered, None, None)) as selected:
                    if fault in (None, 'installed-operator-group-write'):
                        self.assertEqual(verify(runtime, bundle)['source'], 'a' * 40)
                    else:
                        with self.assertRaises(OperationError): verify(runtime, bundle)
                    selected.assert_called_once_with(runtime, 'a' * 40)
                self.assertEqual({p: p.read_bytes() for p in before}, before)

    def test_unregistered_host_keeps_the_normal_first_install_path(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runtime = SimpleNamespace(root=root, state=root / '.operations')
            self.assertFalse(verify(runtime, root / 'candidate')['registered_test_operations'])

    def test_unverified_registered_source_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); state = root / '.operations'; state.mkdir()
            state.chmod(0o700)
            (state / 'platform-test.json').write_text(json.dumps({'source': 'a' * 40}))
            (state / 'platform-test.json').chmod(0o600)
            runtime = SimpleNamespace(root=root, state=state)
            with patch('operations_guard.selection', side_effect=OperationError('archive changed')):
                with self.assertRaises(OperationError): verify(runtime, root / 'candidate')
