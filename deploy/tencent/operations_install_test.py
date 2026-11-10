from pathlib import Path
import tempfile
import shutil
import unittest

from ops_common import install_operations, OperationError


class OperationsOwnershipTests(unittest.TestCase):
    def test_shared_recovery_caller_cannot_advance_before_its_registered_validators(self):
        with tempfile.TemporaryDirectory() as directory:
            root, stage = Path(directory) / 'host', Path(directory) / 'candidate'
            (root / '.operations').mkdir(parents=True)
            (root / '.operations/platform-test.json').write_text('registered test owner')
            names = ('tencent/albums_checkpoint.py', 'tencent/albums_artifacts.py', 'tencent/albums_recovery.py',
                     'albums/backup.py', 'albums/deletion_ledger.py', 'albums/asset_deletion_ledger.py',
                     'albums/collection_deletion_ledger.py', 'tencent/ops_offsite.py', 'tencent/ops_backup.py')
            for name in names:
                for base in (stage, root):
                    path = base / 'deploy' / name; path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text('candidate ' + name)
            (root / 'deploy/tencent/ops_offsite.py').write_text('previous shared caller')
            (root / 'deploy/tencent/ops_backup.py').write_text('previous shared SQL tool')
            validator = root / 'deploy/albums/backup.py'
            validator.write_text('previous portable SQL validator')
            before = {p: p.read_bytes() for p in (root / 'deploy').rglob('*.py')}
            with self.assertRaisesRegex(OperationError, 'matched registered test recovery'):
                install_operations(stage, root)
            self.assertEqual({p: p.read_bytes() for p in before}, before)
            shutil.copy2(stage / 'deploy/albums/backup.py', validator)
            install_operations(stage, root)
            self.assertEqual((root / 'deploy/tencent/ops_offsite.py').read_text(), 'candidate tencent/ops_offsite.py')
            self.assertEqual((root / 'deploy/tencent/ops_backup.py').read_text(), 'candidate tencent/ops_backup.py')

    def test_production_updates_shared_tools_and_preserves_registered_test_tools(self):
        with tempfile.TemporaryDirectory() as directory:
            root, stage = Path(directory) / 'host', Path(directory) / 'candidate'
            installed, source = root / 'deploy/tencent', stage / 'deploy/tencent'
            installed.mkdir(parents=True)
            source.mkdir(parents=True)
            (root / '.operations').mkdir()
            (root / '.operations/platform-test.json').write_text('independent test selection')
            for name in ('ops_backup.py', 'platform_test_deploy.py', 'test_routes.py', 'albums_checkpoint.py', 'albums_artifacts.py', 'albums_recovery.py'):
                (installed / name).write_text('installed ' + name)
                (source / name).write_text('candidate ' + name)
            (source / 'platform_test_future.py').write_text('future test helper')
            install_operations(stage, root)
            self.assertEqual((installed / 'ops_backup.py').read_text(), 'candidate ops_backup.py')
            for name in ('platform_test_deploy.py', 'test_routes.py', 'albums_checkpoint.py', 'albums_artifacts.py', 'albums_recovery.py'):
                self.assertEqual((installed / name).read_text(), 'installed ' + name)
            self.assertFalse((installed / 'platform_test_future.py').exists())
            install_operations(root, root)

    def test_first_install_bootstraps_tools_but_never_follows_a_symlink(self):
        with tempfile.TemporaryDirectory() as directory:
            root, stage = Path(directory) / 'host', Path(directory) / 'candidate'
            source = stage / 'deploy/tencent'
            source.mkdir(parents=True)
            (source / 'platform_test_deploy.py').write_text('bootstrap')
            install_operations(stage, root)
            self.assertEqual((root / 'deploy/tencent/platform_test_deploy.py').read_text(), 'bootstrap')
            (source / 'unsafe.py').symlink_to(source / 'platform_test_deploy.py')
            with self.assertRaises(OperationError):
                install_operations(stage, root)


if __name__ == '__main__':
    unittest.main()
