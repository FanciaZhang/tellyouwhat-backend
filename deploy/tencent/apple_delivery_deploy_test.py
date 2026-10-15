import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
import tarfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import apple_delivery_deploy as delivery
from ops_common import OperationError, atomic_json, atomic_text
from ops_artifacts import register, plan_cleanup, GRACE_SECONDS
from apple_delivery_artifacts import backup_configuration, verify_recovery


class AppleDeliveryDeploymentTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.state = self.root / '.operations'
        self.state.mkdir(mode=0o700)
        atomic_json(self.state / 'blue-green.json', {'phase': 'STABLE', 'current': {'slot': 'green'}})
        (self.root / '.apple-delivery/releases').mkdir(mode=0o700, parents=True)
        (self.root / '.apple-delivery').chmod(0o700)
        self.source = 'b' * 40
        self.folder, self.value = self.release(self.source)
        self.runtime = SimpleNamespace(root=self.root, state=self.state, execute=Mock(return_value=b''))
        self.assert_route = patch.object(delivery, 'assert_route').start()
        self.addCleanup(patch.stopall)
        self.ready = patch.object(delivery, 'ready').start()
        patch('apple_delivery_artifacts.CADDY_ROOT', self.root / 'inactive-Caddyfile').start()
        patch('apple_delivery_artifacts.CADDY_FILES', {'delivery.caddy': self.root / 'inactive-delivery.caddy',
                                                    'logging.caddy': self.root / 'inactive-logging.caddy'}).start()

    def release(self, source):
        folder = self.root / '.apple-delivery/releases' / source
        folder.mkdir(mode=0o700)
        contents = {name: ('synthetic fixture for ' + name).encode() for name in delivery.FILES}
        for name, data in contents.items():
            atomic_text(folder / name, data.decode())
        value = {'source': source, 'image': delivery.IMAGE + source, 'origin': delivery.ORIGIN,
                 'files': {name: hashlib.sha256(data).hexdigest() for name, data in contents.items()}}
        atomic_json(folder / 'selection.json', value)
        return folder, value

    def prior(self):
        folder, value = self.release('a' * 40)
        previous = {key: value[key] for key in ('source', 'image', 'origin')}
        previous.update(release=str(folder), previous=None)
        atomic_json(self.state / 'apple-delivery.json', previous)
        atomic_json(self.state / 'artifact-owners/apple-development.json', {'current': {'images': [value['image']]}})
        atomic_text(folder / '.env', ''.join(key + '=' + val + '\n' for key, val in delivery.environment(self.runtime, value['source']).items()))
        return folder, previous

    def calls(self):
        return [call.args[0] for call in self.runtime.execute.call_args_list]

    def test_first_activation_binds_recovery_to_private_configuration(self):
        result = delivery.activate(self.runtime, self.source)
        self.assertEqual(result['image'], self.value['image'])
        self.assertIsNone(result['previous'])
        self.assertEqual(json.loads((self.state / 'apple-delivery.json').read_text()), result)
        self.assertEqual((self.folder / '.env').stat().st_mode & 0o777, 0o600)
        initialize = self.calls().index('apple-delivery-initialize')
        start = self.calls().index('apple-delivery-start')
        self.assertLess(initialize, start)
        self.ready.assert_called_once()
        self.assertEqual(self.assert_route.call_count, 2)
        build = next(c for c in self.runtime.execute.call_args_list if c.args[0] == 'apple-delivery-build')
        with tarfile.open(fileobj=io.BytesIO(build.kwargs['input'])) as archive:
            self.assertEqual(set(archive.getnames()), {'Dockerfile', 'service'})
            self.assertEqual(archive.getmember('service').mode, 0o555)
        self.assertEqual((self.folder / 'service').stat().st_mode & 0o777, 0o600)

    def test_activation_failure_restores_previous_image_and_matching_configuration(self):
        old_folder, previous = self.prior()
        self.ready.side_effect = [OperationError('new service failed'), None]
        with self.assertRaisesRegex(OperationError, 'prior service restored'):
            delivery.activate(self.runtime, self.source)
        self.assertEqual(json.loads((self.state / 'apple-delivery.json').read_text()), previous)
        call = next(c for c in self.runtime.execute.call_args_list if c.args[0] == 'apple-delivery-restore')
        self.assertIn(str(old_folder / '.env'), call.args[1])
        self.assertIn(str(old_folder / 'compose.apple-delivery.yaml'), call.args[1])
        self.assertEqual(call.kwargs['env']['APPLE_DELIVERY_IMAGE_TAG'], previous['source'])

    def test_first_failed_activation_removes_only_its_compose_project(self):
        self.ready.side_effect = OperationError('not ready')
        with self.assertRaisesRegex(OperationError, 'prior service restored'):
            delivery.activate(self.runtime, self.source)
        self.assertFalse((self.state / 'apple-delivery.json').exists())
        call = next(c for c in self.runtime.execute.call_args_list if c.args[0] == 'apple-delivery-remove-failed')
        self.assertIn('tellyouwhat-apple-delivery', call.args[1])
        self.assertEqual(call.args[1][-1], 'down')
        self.assertNotIn('--volumes', call.args[1])

    def test_failed_recovery_is_visible(self):
        self.prior()
        self.ready.side_effect = OperationError('unavailable')
        with self.assertRaisesRegex(OperationError, 'operator recovery required'):
            delivery.activate(self.runtime, self.source)

    def test_failure_after_state_rename_restores_previous_state_and_service(self):
        _, previous = self.prior()
        def fail_after_commit(path, value):
            atomic_json(path, value)
            if path.name == 'apple-delivery.json' and value.get('source') == self.source:
                raise OSError('synthetic directory fsync failure')
        with patch.object(delivery, 'atomic_json', side_effect=fail_after_commit):
            with self.assertRaisesRegex(OperationError, 'prior service restored'):
                delivery.activate(self.runtime, self.source)
        self.assertEqual(json.loads((self.state / 'apple-delivery.json').read_text()), previous)
        self.assertIn('apple-delivery-restore', self.calls())

    def test_unsafe_or_changed_input_fails_before_docker(self):
        for change in ('digest', 'permissions', 'identity'):
            with self.subTest(change=change):
                if change == 'digest':
                    (self.folder / 'service').write_text('changed bytes')
                elif change == 'permissions':
                    atomic_text(self.folder / 'service', 'synthetic fixture for service')
                    (self.folder / 'service').chmod(0o644)
                else:
                    (self.folder / 'service').chmod(0o600)
                    value = {**self.value, 'source': 'c' * 40}
                    atomic_json(self.folder / 'selection.json', value)
                with self.assertRaises(OperationError): delivery.activate(self.runtime, self.source)
                self.runtime.execute.assert_not_called()

    def test_unmanaged_service_wrong_operator_and_nonstable_release_are_rejected(self):
        self.runtime.execute.return_value = b'existing-container'
        with self.assertRaisesRegex(OperationError, 'unmanaged'):
            delivery.activate(self.runtime, self.source)
        self.runtime.execute.reset_mock()
        with patch('ops_artifacts.os.geteuid', return_value=os.geteuid() + 1):
            with self.assertRaisesRegex(OperationError, 'directory owner'):
                delivery.activate(self.runtime, self.source)
        self.runtime.execute.assert_not_called()
        atomic_json(self.state / 'blue-green.json', {'phase': 'DRAINING'})
        with self.assertRaisesRegex(OperationError, 'stable'):
            delivery.activate(self.runtime, self.source)
        self.runtime.execute.assert_not_called()

    def test_redeployment_keeps_previous_and_rejects_image_with_different_binary(self):
        old_folder, previous = self.prior()
        current = delivery.activate(self.runtime, self.source)
        self.assertEqual(current['previous']['image'], previous['image'])
        atomic_json(self.state / 'artifact-owners/apple-development.json', {'current': {'images': [self.value['image']]}})
        again = delivery.activate(self.runtime, self.source)
        self.assertEqual(again, current)
        self.runtime.execute.side_effect = lambda label, command, **kwargs: (
            b'image-id' if label == 'apple-delivery-image-inventory' else b'[{"Config":{"Labels":{}}}]')
        with self.assertRaisesRegex(OperationError, 'different bytes'):
            delivery.activate(self.runtime, self.source)

    def test_backup_preserves_both_verified_binaries_and_matching_configuration(self):
        old_folder, old = self.release('a' * 40)
        for source, folder, value in [(old['source'], old_folder, old), (self.source, self.folder, self.value)]:
            delivery.activate(self.runtime, source)
            register(self.runtime, 'apple-development', [str(folder)], [value['image']], source)
        target = self.root / 'snapshot'
        hashes = backup_configuration(self.runtime, target)
        self.assertIn('current/service', hashes)
        self.assertIn('previous/service', hashes)
        self.assertEqual((target / 'current/service').read_bytes(), (self.folder / 'service').read_bytes())
        archive = self.root / 'snapshot.tar.gz'
        with tarfile.open(archive, 'w:gz') as output: output.add(target, arcname='recovery/apple-delivery')
        with tarfile.open(archive) as contents: verify_recovery(contents, hashes)
        (target / 'previous/.env').write_text('changed configuration')
        with tarfile.open(archive, 'w:gz') as output: output.add(target, arcname='recovery/apple-delivery')
        with tarfile.open(archive) as contents:
            with self.assertRaisesRegex(OperationError, 'authenticated manifest'): verify_recovery(contents, hashes)
        missing = dict(hashes)
        del missing['previous/service']
        with tarfile.open(archive) as contents:
            with self.assertRaisesRegex(OperationError, 'incomplete'): verify_recovery(contents, missing)

    def test_backup_rejects_changed_configuration_and_unregistered_selection(self):
        delivery.activate(self.runtime, self.source)
        register(self.runtime, 'apple-development', [str(self.folder)], [self.value['image']], self.source)
        (self.folder / '.env').write_text('APPLE_DELIVERY_IMAGE_TAG=production\n')
        with self.assertRaisesRegex(OperationError, 'environment differs'):
            backup_configuration(self.runtime, self.root / 'snapshot')
        atomic_text(self.folder / '.env', ''.join(key + '=' + val + '\n' for key, val in delivery.environment(self.runtime, self.source).items()))
        atomic_json(self.state / 'artifact-owners/apple-development.json', {'current': {'images': ['production:keep']}})
        with self.assertRaisesRegex(OperationError, 'owner registration'):
            backup_configuration(self.runtime, self.root / 'snapshot')

    def test_retention_preserves_current_previous_data_and_unknown_files(self):
        current = self.folder
        previous, _ = self.release('a' * 40)
        obsolete, _ = self.release('c' * 40)
        unknown = current.parent / 'user-files'
        unknown.mkdir()
        data = current.parent.parent / 'data'
        data.mkdir()
        for path in (current, previous, obsolete, unknown, data):
            os.utime(path, (1000, 1000))
        atomic_json(self.state / 'artifact-owners/apple-development.json', {
            'owner': 'apple-development', 'current': {'paths': [str(current)]}, 'previous': {'paths': [str(previous)]}})
        plan = plan_cleanup(self.root, {}, [], [], owners=['apple-development'], now=1001 + GRACE_SECONDS)
        self.assertEqual([entry['path'] for entry in plan['files']], [str(obsolete)])


if __name__ == '__main__': unittest.main()
