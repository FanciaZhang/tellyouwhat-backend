from pathlib import Path
import tempfile
import unittest

from ops_common import install_operations, OperationError


class OperationsOwnershipTests(unittest.TestCase):
    def test_production_updates_shared_tools_and_preserves_registered_test_tools(self):
        with tempfile.TemporaryDirectory() as directory:
            root, stage = Path(directory) / 'host', Path(directory) / 'candidate'
            installed, source = root / 'deploy/tencent', stage / 'deploy/tencent'
            installed.mkdir(parents=True)
            source.mkdir(parents=True)
            (root / '.operations').mkdir()
            (root / '.operations/platform-test.json').write_text('independent test selection')
            for name in ('ops_backup.py', 'platform_test_deploy.py', 'test_routes.py'):
                (installed / name).write_text('installed ' + name)
                (source / name).write_text('candidate ' + name)
            (source / 'platform_test_future.py').write_text('future test helper')
            install_operations(stage, root)
            self.assertEqual((installed / 'ops_backup.py').read_text(), 'candidate ops_backup.py')
            for name in ('platform_test_deploy.py', 'test_routes.py'):
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
