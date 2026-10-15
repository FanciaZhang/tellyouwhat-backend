import contextlib
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import Mock, patch

import test_routes as routes
from ops_common import OperationError


class TestRouteTests(unittest.TestCase):
    def setUp(self):
        folder = tempfile.TemporaryDirectory()
        self.addCleanup(folder.cleanup)
        self.root = Path(folder.name)
        self.main = self.root / 'Caddyfile'
        self.fragment = self.root / 'test.caddy'
        self.main.write_text('production.example {\n reverse_proxy 127.0.0.1:18180\n}\n')
        self.before = self.main.read_text()
        self.runtime = Mock(root=self.root, state=self.root / '.operations')
        self.runtime.state.mkdir()
        (self.runtime.state / 'blue-green.json').write_text('{"phase":"STABLE","current":{"slot":"green"}}')
        self.runtime.lock.side_effect = lambda *a, **kw: contextlib.nullcontext()
        self.calls = []
        self.fail_reload = False
        self.fail_validate = False
        self.runtime.execute.side_effect = self.execute
        assertion = patch.object(routes, 'assert_route')
        assertion.start()
        self.addCleanup(assertion.stop)
        for name, value in [('ROOT', self.main), ('FRAGMENT', self.fragment)]:
            p = patch.object(routes, name, value)
            p.start()
            self.addCleanup(p.stop)

    def execute(self, label, command):
        self.calls.append(label)
        if label == 'test-route-validate' and self.fail_validate:
            raise OperationError('invalid configuration')
        if label == 'test-route-reload' and self.fail_reload:
            raise OperationError('reload failed')
        if 'install' in command:
            shutil.copyfile(command[-2], command[-1])
        elif 'mv' in command:
            Path(command[-2]).replace(command[-1])
        elif 'rm' in command:
            Path(command[-1]).unlink(missing_ok=True)
        return b''

    def configure(self):
        with patch.object(routes, 'ready'):
            return routes.configure(self.runtime, ['journal'], 'journal-voice-v36')

    def test_adds_only_test_import_and_validates_before_active_file_change(self):
        self.configure()
        self.assertEqual(self.main.read_text(), self.before.rstrip() + '\n\nimport ' + str(self.fragment) + '\n')
        self.assertIn('api-test.journal.tellyouwhat.cn', self.fragment.read_text())
        self.assertNotIn('api.journal.tellyouwhat.cn', self.fragment.read_text())
        self.assertLess(self.calls.index('test-route-validate'), self.calls.index('test-route-commit'))
        self.runtime.record.assert_called_once()

    def test_invalid_configuration_does_not_change_active_files(self):
        self.fail_validate = True
        with self.assertRaises(OperationError): self.configure()
        self.assertEqual(self.main.read_text(), self.before)
        self.assertFalse(self.fragment.exists())
        self.runtime.record.assert_not_called()

    def test_reload_failure_restores_original_root_and_removes_new_fragment(self):
        self.fail_reload = True
        with self.assertRaisesRegex(OperationError, 'previous Caddy configuration restored'): self.configure()
        self.assertEqual(self.main.read_text(), self.before)
        self.assertFalse(self.fragment.exists())
        self.assertIn('test-route-restore', self.calls)
        self.runtime.record.assert_not_called()

    def test_existing_unknown_fragment_is_not_overwritten(self):
        self.fragment.write_text('unmanaged configuration')
        with self.assertRaisesRegex(OperationError, 'managed state'): self.configure()
        self.assertEqual(self.fragment.read_text(), 'unmanaged configuration')
        self.assertNotIn('test-route-stage', self.calls)

    def test_nonstable_production_release_rejects_route_change(self):
        (self.runtime.state / 'blue-green.json').write_text('{"phase":"DRAINING"}')
        with self.assertRaisesRegex(OperationError, 'stable'): self.configure()
        self.assertEqual(self.main.read_text(), self.before)
        self.assertEqual(self.calls, [])

    def test_production_configuration_cannot_pass_as_test_service(self):
        for app, env in [('journal', ['DATABASE_DSN=production']),
                         ('health', ['APP_ENV=production']),
                         ('albums', ['ALBUM_DEPLOYMENT_ENVIRONMENT=production'])]:
            with self.subTest(app=app):
                self.runtime.execute.side_effect = None
                self.runtime.execute.return_value = json.dumps([{'State': {'Running': True}, 'Config': {'Env': env}}]).encode()
                with self.assertRaises(OperationError): routes.development_environment(self.runtime, app)

    def test_render_rejects_unknown_or_empty_apps(self):
        for apps in [[], ['production'], ['journal', 'unknown']]:
            with self.assertRaises(OperationError): routes.render(apps)

    def test_readiness_redirect_does_not_forward_authorization(self):
        self.assertIsNone(routes.RejectRedirects().redirect_request(
            Mock(), None, 302, 'redirect', {}, 'https://other.example/'))


if __name__ == '__main__':
    unittest.main()
