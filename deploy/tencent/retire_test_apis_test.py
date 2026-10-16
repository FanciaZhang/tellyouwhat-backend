import copy
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import Mock, patch

import retire_test_apis as retire
from ops_common import OperationError


class RetirementTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        self.runtime = Mock(root=root, state=root / '.operations', backups=root / 'backups')
        self.runtime.state.mkdir(mode=0o700)
        self.runtime.backups.mkdir()
        for name in ('operations', 'deployment'):
            (self.runtime.state / (name + '.lock')).touch(mode=0o600)
        self.source = 'a' * 40
        self.folder = root / '.platform-test/releases' / self.source
        self.folder.mkdir(parents=True)
        self.save('platform-test.json', {'source': self.source, 'apps': list(retire.test_routes.ROUTES)})
        self.shared_owner = {'source': self.source, 'paths': [str(self.folder)], 'images': ['shared:image']}
        self.save('artifact-owners/platform-test.json', {'current': self.shared_owner})
        self.save('blue-green.json', {'phase': 'STABLE', 'current': {'slot': 'blue'}})
        installed = root / 'deploy/tencent'
        installed.mkdir(parents=True)
        for name in ('retire_test_apis.py', 'journal_development_artifacts.py'):
            shutil.copyfile(Path(retire.__file__).with_name(name), installed / name)
        self.expected = []
        for index, (name, owner) in enumerate(retire.TARGETS.items(), 1):
            self.expected.append({'Id': str(index) * 64, 'Name': '/' + name, 'Image': 'sha256:' + owner,
                                  'State': {'Running': True, 'Status': 'running'}})
            self.save('artifact-owners/' + owner + '.json',
                      {'owner': owner, 'current': {'images': ['sha256:' + owner]}, 'previous': None})
        self.original = copy.deepcopy(self.expected)
        self.routes = {'routes': [{'match': [{'host': [host]}], 'handle': [
            {'handler': 'reverse_proxy', 'upstreams': [{'dial': '127.0.0.1:18790'}]}]}
            for host, _ in retire.test_routes.ROUTES.values()]}
        self.events = []
        self.fail_stop = False
        self.fail_after_backup = False
        self.backups = 0
        self.runtime.execute.side_effect = self.execute
        self.runtime.record.side_effect = lambda name, **values: values
        for target, name, kwargs in (
                (retire, 'require_operator', {}),
                (retire.fcntl, 'flock', {'side_effect': BlockingIOError}),
                (retire.platform, 'selection', {'return_value': (self.folder, {}, [])}),
                (retire.platform, 'verify_running', {}),
                (retire.platform.albums, 'require_resources', {}),
                (retire.blue_green, 'assert_route', {}),
                (retire.blue_green, 'proxy_config', {'side_effect': lambda: self.routes}),
                (retire.test_routes, 'ready', {})):
            patcher = patch.object(target, name, **kwargs)
            patcher.start()
            self.addCleanup(patcher.stop)
        patcher = patch.object(retire, 'checkpoint', side_effect=self.checkpoint)
        self.checkpoint_patch = patcher
        self.checkpoint_mock = patcher.start()
        self.addCleanup(patcher.stop)

    def save(self, name, value):
        path = self.runtime.state / name
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        path.write_text(json.dumps(value))
        path.chmod(0o600)

    def checkpoint(self, runtime):
        self.backups += 1
        self.events.append('backup-' + str(self.backups))
        if self.fail_after_backup and self.backups == 2:
            raise OperationError('offsite backup failed')
        return 'backup-' + str(self.backups)

    def execute(self, label, command, **kwargs):
        if label == 'retirement-owned-images':
            return json.dumps([{'Id': ref} for ref in command[3:]]).encode()
        if label == 'retirement-containers':
            return json.dumps(self.expected).encode()
        if label == 'retirement-stop':
            self.events.append('stop')
            for c in self.expected:
                if c['Id'] in command[4:]:
                    c['State'].update(Running=False, Status='exited')
                    if self.fail_stop:
                        raise OperationError('partial stop failed')
            return b''
        if label == 'retirement-restore':
            self.events.append('restore')
            for c in self.expected:
                if c['Id'] in command[2:]:
                    c['State'].update(Running=True, Status='running')
            return b''
        self.fail('unexpected operation: ' + label)

    def test_retirement_preserves_containers_and_checks_both_backups(self):
        result = retire.retire(self.runtime, self.source)
        self.assertTrue(result['passed'])
        self.assertEqual(set(result['stopped']), set(retire.TARGETS))
        self.assertEqual(self.events, ['backup-1', 'stop', 'backup-2'])
        self.assertEqual([c['Id'] for c in self.expected], [c['Id'] for c in self.original])
        self.assertTrue(all(not c['State']['Running'] for c in self.expected))
        command = next(c.args[1] for c in self.runtime.execute.call_args_list if c.args[0] == 'retirement-stop')
        self.assertEqual(set(command[4:]), {c['Id'] for c in self.original})
        self.assertFalse(any('rm' in c.args[1] or 'prune' in c.args[1] for c in self.runtime.execute.call_args_list))

    def test_repeat_retirement_keeps_stopped_services_stopped(self):
        for c in self.expected:
            c['State'].update(Running=False, Status='exited')
        retire.retire(self.runtime, self.source)
        self.assertEqual(self.events, ['backup-1', 'backup-2'])

    def test_old_live_route_including_catchall_blocks_retirement(self):
        self.routes['routes'].append({'handle': [{'upstreams': [{'dial': '127.0.0.1:18787'}]}]})
        with self.assertRaisesRegex(OperationError, 'live proxy'):
            retire.retire(self.runtime, self.source)
        self.assertEqual(self.events, [])

    def test_missing_shared_host_or_different_source_blocks_retirement(self):
        with self.assertRaisesRegex(OperationError, 'registered four-App'):
            retire.retire(self.runtime, 'b' * 40)
        self.routes['routes'].pop()
        with self.assertRaisesRegex(OperationError, 'live proxy'):
            retire.retire(self.runtime, self.source)
        self.assertEqual(self.events, [])

    def test_unmatched_image_or_data_container_name_cannot_be_stopped(self):
        for change in ({'Image': 'sha256:unregistered'}, {'Name': '/tellyouwhat-albums-development-mysql-1'}):
            self.expected = copy.deepcopy(self.original)
            self.expected[0].update(change)
            with self.assertRaises(OperationError):
                retire.retire(self.runtime, self.source)
        self.assertEqual(self.events, [])

    def test_missing_scheduled_backup_update_blocks_before_mutation(self):
        (self.runtime.root / 'deploy/tencent/journal_development_artifacts.py').write_text('old implementation')
        with self.assertRaisesRegex(OperationError, 'scheduled backup'):
            retire.retire(self.runtime, self.source)
        self.assertEqual(self.events, [])

    def test_pre_backup_failure_never_stops_services(self):
        self.checkpoint_mock.side_effect = OperationError('backup failed')
        with self.assertRaisesRegex(OperationError, 'backup failed'):
            retire.retire(self.runtime, self.source)
        self.assertTrue(all(c['State']['Running'] for c in self.expected))
        self.assertEqual(self.events, [])

    def test_failed_post_backup_restores_only_previously_running_services(self):
        self.expected[0]['State'].update(Running=False, Status='exited')
        self.fail_after_backup = True
        with self.assertRaisesRegex(OperationError, 'offsite backup'):
            retire.retire(self.runtime, self.source)
        self.assertEqual(self.events, ['backup-1', 'stop', 'backup-2', 'restore'])
        self.assertFalse(self.expected[0]['State']['Running'])
        self.assertTrue(all(c['State']['Running'] for c in self.expected[1:]))
        self.runtime.record.assert_not_called()

    def test_partial_stop_failure_restores_original_running_set(self):
        self.fail_stop = True
        with self.assertRaisesRegex(OperationError, 'partial stop'):
            retire.retire(self.runtime, self.source)
        self.assertTrue(all(c['State']['Running'] for c in self.expected))
        self.assertEqual(self.events, ['backup-1', 'stop', 'restore'])

    def test_inventory_change_during_backup_never_stops_a_replacement(self):
        def change(runtime):
            self.expected[0]['Id'] = 'f' * 64
            return 'backup'
        self.checkpoint_mock.side_effect = change
        with self.assertRaisesRegex(OperationError, 'inventory changed'):
            retire.retire(self.runtime, self.source)
        self.assertTrue(all(c['State']['Running'] for c in self.expected))

    def test_direct_retirement_without_owner_locks_is_rejected(self):
        with patch.object(retire.fcntl, 'flock', return_value=None):
            with self.assertRaisesRegex(OperationError, 'both locks'):
                retire.retire(self.runtime, self.source)
        self.assertEqual(self.events, [])

    def test_checkpoint_requires_all_recovery_selections(self):
        self.checkpoint_patch.stop()
        manifest = {'recovery_' + name: {'verified': True} for name in (
            'platform_test', 'journal_development', 'albums_development', 'test_routing')}
        with patch.object(retire, 'create_backup', return_value={'filename': 'snapshot'}), \
                patch.object(retire, 'verify_backup', return_value=manifest), \
                patch.object(retire, 'verify_recovery_bundle') as verify:
            self.assertEqual(retire.checkpoint(self.runtime), 'snapshot')
            verify.assert_called_once()
            del manifest['recovery_journal_development']
            with self.assertRaisesRegex(OperationError, 'complete authenticated'):
                retire.checkpoint(self.runtime)


if __name__ == '__main__':
    unittest.main()
