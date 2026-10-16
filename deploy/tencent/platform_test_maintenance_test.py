import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from ops_common import OperationError
import platform_test_maintenance as maintenance
import platform_test_storage as storage


class PlatformTestMaintenanceTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.state = self.root / '.operations'
        self.state.mkdir()
        self.folder = self.root / 'selected'
        self.folder.mkdir()
        (self.folder / 'runtime.env').write_text('PLATFORM_APPS=health,journal,birthday\n')
        self.runtime = Mock(root=self.root, state=self.state)
        self.source = 'a' * 40
        self.marker = self.state / 'platform-test.json'
        self.marker.write_text(json.dumps({'source': self.source, 'release': str(self.folder)}))
        self.marker.chmod(0o600)
        self.owner = self.state / 'artifact-owners/platform-test.json'
        self.owner.parent.mkdir()
        self.owner.write_text(json.dumps({'current': {'source': self.source, 'paths': [str(self.folder)]}}))
        self.owner.chmod(0o600)

    def maintain(self, configured=True):
        with patch.object(maintenance, 'selection', return_value=(self.folder, {}, [])), \
                patch.object(maintenance, 'available', return_value=configured):
            return maintenance.maintain(self.runtime)

    def test_no_stack_or_retained_old_topology_does_not_launch_new_service(self):
        self.assertFalse(self.maintain(False)['configured'])
        self.runtime.execute.assert_not_called()
        self.marker.unlink()
        self.assertFalse(maintenance.maintain(self.runtime)['configured'])
        self.runtime.execute.assert_not_called()

    def test_maintenance_runs_selected_test_image_independently_of_gateway(self):
        self.maintain()
        command = self.runtime.execute.call_args.args[1]
        self.assertIn(str(self.folder / 'compose.platform-test.yaml'), command)
        self.assertEqual(command[-4:], ['run', '--rm', '--no-deps', 'maintenance'])
        self.assertNotIn('up', command)
        self.assertNotIn('gateway', command)
        self.runtime.record.assert_called_once_with('platform-test-maintenance', configured=True, source=self.source, passed=True)

    def test_failed_cleanup_never_records_success(self):
        self.runtime.execute.side_effect = OperationError('fixture cleanup failure')
        with self.assertRaises(OperationError):
            self.maintain()
        self.runtime.record.assert_not_called()

    def test_wrong_registration_cannot_select_another_release(self):
        self.owner.write_text(json.dumps({'current': {'source': 'b' * 40, 'paths': [str(self.folder)]}}))
        with self.assertRaisesRegex(OperationError, 'registered active release'):
            self.maintain()
        self.runtime.execute.assert_not_called()

    def test_runtime_mount_requires_development_path_user_and_writable_bind(self):
        container = {'Config': {'User': '65532:65532', 'Env': ['APP_ENV=development', 'JOURNAL_RECORDING_ROOT=' + storage.ROOT]},
                     'Mounts': [{'Source': storage.ROOT, 'Destination': storage.ROOT, 'Type': 'bind', 'RW': True}]}
        storage.verify_gateway(container)
        for section, key, value in [('Config', 'User', '0:0'), ('Config', 'Env', ['APP_ENV=production']),
                                    ('Mounts', 'Source', storage.ROOT.replace('development', 'production')),
                                    ('Mounts', 'RW', False), ('Mounts', 'Type', 'volume')]:
            changed = copy.deepcopy(container)
            (changed[section][0] if section == 'Mounts' else changed[section])[key] = value
            with self.subTest(key=key), self.assertRaises(OperationError):
                storage.verify_gateway(changed)
        container['Mounts'] = []
        with self.assertRaises(OperationError):
            storage.verify_gateway(container)


if __name__ == '__main__':
    unittest.main()
