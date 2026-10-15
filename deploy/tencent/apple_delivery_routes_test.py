import copy
import hashlib
import json
from pathlib import Path
import shutil
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import apple_delivery_artifacts as artifacts
import apple_delivery_routes as routes
from apple_delivery_deploy import FILES, IMAGE, ORIGIN, environment
from ops_artifacts import register
from ops_common import OperationError, atomic_json, atomic_text


def configuration():
    return {'logging': {'logs': {
        'default': {'exclude': ['http.log.access.apple_delivery', 'http.log.error.apple_delivery']},
        'apple_delivery': {'writer': {'output': 'discard'}, 'include': ['http.log.access.apple_delivery']}}},
        'apps': {'http': {'servers': {'srv0': {'logs': {'logger_names': {'install.tellyouwhat.cn': ['apple_delivery']}}}}}}}


class AppleDeliveryRouteTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.runtime = SimpleNamespace(root=self.root, state=self.root / '.operations', execute=Mock(side_effect=self.execute))
        self.runtime.state.mkdir(mode=0o700)
        atomic_json(self.runtime.state / 'blue-green.json', {'phase': 'STABLE', 'current': {'slot': 'green'}})
        for folder in (self.root / '.apple-delivery', self.root / '.apple-delivery/releases', self.root / 'active'):
            folder.mkdir(mode=0o700)
        self.main = self.root / 'active/Caddyfile'
        self.main.write_text('production.example {\n reverse_proxy 127.0.0.1:18180\n}\n')
        self.before = self.main.read_text()
        self.targets = {name: self.root / 'active' / ('apple-' + name if name == 'delivery.caddy' else 'apple-delivery-logging.caddy')
                        for name in ('delivery.caddy', 'logging.caddy')}
        self.prefix = '{\n    import ' + str(self.targets['logging.caddy']) + '\n}\n\n'
        for name, value in [('CADDY_ROOT', self.main), ('CADDY_FILES', self.targets), ('ROOT_PREFIX', self.prefix)]:
            context = patch.object(artifacts, name, value)
            context.start(); self.addCleanup(context.stop)
        for name in ('ready', 'assert_route'):
            context = patch.object(routes, name)
            context.start(); self.addCleanup(context.stop)
        self.source = 'a' * 40
        self.prepare(self.source)
        self.events = []
        self.fail = None
        self.adapted = configuration()
        self.active = {'apps': {'http': {'servers': {'production': {}}}}}
        self.baseline = copy.deepcopy(self.active)
        self.base_override = None
        context = patch.object(routes, 'active_configuration', side_effect=lambda: self.active)
        context.start(); self.addCleanup(context.stop)

    def prepare(self, source, revised=False):
        folder = self.root / '.apple-delivery/releases' / source
        folder.mkdir(mode=0o700)
        files = {}
        for name in FILES:
            if name.endswith('.caddy'):
                data = (Path(__file__).parent / name).read_bytes()
                if revised: data += b'# revised synthetic route fixture\n'
            else:
                data = ('synthetic fixture for ' + name).encode()
            atomic_text(folder / name, data.decode())
            files[name] = hashlib.sha256(data).hexdigest()
        atomic_json(folder / 'selection.json', {'source': source, 'image': IMAGE + source, 'origin': ORIGIN, 'files': files})
        atomic_text(folder / '.env', ''.join(key + '=' + value + '\n' for key, value in environment(self.runtime, source).items()))
        state_file = self.runtime.state / 'apple-delivery.json'
        prior = json.loads(state_file.read_text()) if state_file.exists() else None
        atomic_json(state_file, {'source': source, 'image': IMAGE + source, 'origin': ORIGIN, 'release': str(folder),
                                'previous': {key: value for key, value in prior.items() if key != 'previous'} if prior else None})
        register(self.runtime, 'apple-development', [str(folder)], [IMAGE + source], source)

    def execute(self, label, command, **kwargs):
        self.events.append(label)
        if label == self.fail:
            raise OperationError('synthetic ' + label + ' failure')
        if label == 'apple-route-adapt':
            return json.dumps(self.adapted).encode()
        if label == 'apple-route-baseline':
            self.baseline = copy.deepcopy(self.active)
            return json.dumps(self.base_override if self.base_override is not None else self.active).encode()
        if label == 'apple-route-reload':
            self.active = copy.deepcopy(self.adapted)
        if label == 'apple-route-restore':
            self.active = copy.deepcopy(self.baseline)
        if 'install' in command:
            shutil.copyfile(command[-2], command[-1])
        elif 'mv' in command:
            Path(command[-2]).replace(command[-1])
        elif 'unlink' in command:
            Path(command[-1]).unlink()
        return b''

    def test_activation_validates_whole_config_and_backs_up_exact_proxy_files(self):
        self.assertTrue(routes.configure(self.runtime, self.source)['changed'])
        self.assertEqual(self.main.read_text(), self.prefix + self.before.rstrip() + '\n\nimport ' + str(self.targets['delivery.caddy']) + '\n')
        self.assertLess(self.events.index('apple-route-validate'), self.events.index('test-route-commit'))
        record = json.loads((self.runtime.state / 'apple-delivery-routing.json').read_text())
        hashes = artifacts.backup_configuration(self.runtime, self.root / 'snapshot')
        for name, path in self.targets.items():
            self.assertEqual(record[name], hashlib.sha256(path.read_bytes()).hexdigest())
            self.assertEqual(hashes[name], record[name])
        self.assertIn('Caddyfile', hashes)
        self.assertIn('routing.json', hashes)
        self.assertFalse(routes.configure(self.runtime, self.source)['changed'])
        self.assertEqual(self.events.count('apple-route-reload'), 1)
        self.assertFalse(list((self.root / '.apple-delivery/releases' / self.source).glob('.routing-*')))

    def test_mismatch_diagnostics_include_paths_without_configuration_values(self):
        paths = routes.difference_paths({'private': {'credential': 'synthetic-before'}, 'nullable': None},
                                       {'private': {'credential': 'synthetic-after'}})
        self.assertEqual(paths, ['/nullable', '/private/credential'])
        self.assertNotIn('synthetic', json.dumps(paths))

    def test_unsafe_logging_is_rejected_before_active_files_change(self):
        variants = []
        value = configuration();value['logging']['logs']['default']['level'] = 'DEBUG';variants.append(value)
        value = configuration();value['logging']['logs']['default']['exclude'].pop();variants.append(value)
        value = configuration();value['logging']['logs']['apple_delivery']['writer']['output'] = 'stderr';variants.append(value)
        value = configuration();value['logging']['logs']['capture'] = {'writer': {'output': 'stderr'}};variants.append(value)
        value = configuration();value['apps']['http']['servers']['srv0']['logs']['logger_names']['install.tellyouwhat.cn'] = ['other'];variants.append(value)
        value = configuration();value['apps']['http']['servers']['srv0']['logs']['skip_hosts'] = ['install.tellyouwhat.cn'];variants.append(value)
        value = configuration();value['apps']['http']['servers']['srv0']['trace'] = True;variants.append(value)
        for adapted in variants:
            with self.subTest(adapted=adapted):
                self.adapted = adapted
                with self.assertRaises(OperationError): routes.configure(self.runtime, self.source)
                self.assertEqual(self.main.read_text(), self.before)
                self.assertFalse(any(path.exists() for path in self.targets.values()))
        self.assertNotIn('test-route-commit', self.events)

    def test_invalid_caddy_does_not_touch_active_files(self):
        self.fail = 'apple-route-validate'
        with self.assertRaises(OperationError): routes.configure(self.runtime, self.source)
        self.assertEqual(self.main.read_text(), self.before)
        self.assertFalse(any(path.exists() for path in self.targets.values()))

    def test_running_config_drift_blocks_activation(self):
        self.base_override = {'unmanaged': 'configuration'}
        with self.assertRaisesRegex(OperationError, 'running Caddy differs'):
            routes.configure(self.runtime, self.source)
        self.assertEqual(self.main.read_text(), self.before)
        self.assertNotIn('test-route-commit', self.events)

    def test_successful_reload_without_expected_active_config_is_recovered(self):
        execute = self.runtime.execute.side_effect
        def ignore_reload(label, command, **kwargs):
            result = execute(label, command, **kwargs)
            if label == 'apple-route-reload': self.active = copy.deepcopy(self.baseline)
            return result
        self.runtime.execute.side_effect = ignore_reload
        with self.assertRaisesRegex(OperationError, 'previous configuration restored'):
            routes.configure(self.runtime, self.source)
        self.assertEqual(self.main.read_text(), self.before)
        self.assertEqual(self.active, self.baseline)
        self.assertFalse((self.runtime.state / 'apple-delivery-routing.json').exists())

    def test_reload_failure_restores_root_and_removes_new_fragments(self):
        self.fail = 'apple-route-reload'
        with self.assertRaisesRegex(OperationError, 'previous configuration restored'):
            routes.configure(self.runtime, self.source)
        self.assertEqual(self.main.read_text(), self.before)
        self.assertFalse(any(path.exists() for path in self.targets.values()))
        self.assertFalse((self.runtime.state / 'apple-delivery-routing.json').exists())
        self.assertIn('apple-route-restore', self.events)

    def test_partial_install_removes_pending_file_and_preserves_existing_root(self):
        execute = self.runtime.execute.side_effect
        failed = False
        def fail_one_commit(label, command, **kwargs):
            nonlocal failed
            if label == 'test-route-commit' and not failed:
                failed = True
                raise OperationError('synthetic first rename failure')
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = fail_one_commit
        with self.assertRaisesRegex(OperationError, 'previous configuration restored'):
            routes.configure(self.runtime, self.source)
        self.assertEqual(self.main.read_text(), self.before)
        self.assertFalse(list(self.main.parent.glob('.*.pending')))
        self.assertFalse(any(path.exists() for path in self.targets.values()))

    def test_update_failure_restores_prior_managed_fragments_and_record(self):
        routes.configure(self.runtime, self.source)
        before = self.main.read_text()
        record_file = self.runtime.state / 'apple-delivery-routing.json'
        prior = record_file.read_bytes()
        previous = {name: path.read_bytes() for name, path in self.targets.items()}
        new_source = 'b' * 40
        self.prepare(new_source, revised=True)
        self.fail = 'apple-route-reload'
        with self.assertRaisesRegex(OperationError, 'previous configuration restored'):
            routes.configure(self.runtime, new_source)
        self.assertEqual(self.main.read_text(), before)
        self.assertEqual(record_file.read_bytes(), prior)
        self.assertEqual({name: path.read_bytes() for name, path in self.targets.items()}, previous)

    def test_managed_drift_and_source_change_are_rejected(self):
        with self.assertRaisesRegex(OperationError, 'source changed'):
            routes.configure(self.runtime, 'b' * 40)
        self.assertEqual(self.events, [])
        routes.configure(self.runtime, self.source)
        self.events.clear()
        self.targets['logging.caddy'].write_text('unmanaged logger')
        with self.assertRaisesRegex(OperationError, 'configuration has changed'):
            routes.configure(self.runtime, self.source)
        self.assertEqual(self.events, [])

    def test_late_state_failure_restores_route_and_recovery_record(self):
        def fail_after_commit(path, value):
            atomic_json(path, value)
            raise OSError('synthetic directory fsync failure')
        with patch.object(routes, 'atomic_json', side_effect=fail_after_commit):
            with self.assertRaisesRegex(OperationError, 'previous configuration restored'):
                routes.configure(self.runtime, self.source)
        self.assertEqual(self.main.read_text(), self.before)
        self.assertFalse((self.runtime.state / 'apple-delivery-routing.json').exists())


if __name__ == '__main__': unittest.main()
