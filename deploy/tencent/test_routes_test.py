import contextlib
import io
import hashlib
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
        elif 'unlink' in command:
            Path(command[-1]).unlink()
        return b''

    def configure(self):
        with patch.object(routes, 'ready'):
            return routes.configure(self.runtime, ['journal'])

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

    def test_atomic_install_failure_discards_only_its_pending_file(self):
        source = self.root / 'prepared.caddy'
        source.write_text('synthetic new configuration')
        destination = self.root / 'managed.caddy'
        destination.write_text('keep current configuration')
        execute = self.runtime.execute.side_effect
        def fail_commit(label, command):
            if label == 'test-route-commit': raise OperationError('synthetic rename failure')
            return execute(label, command)
        self.runtime.execute.side_effect = fail_commit
        with self.assertRaises(OperationError): routes.install(self.runtime, source, destination)
        self.assertEqual(destination.read_text(), 'keep current configuration')
        self.assertFalse((self.root / '.managed.caddy.pending').exists())

    def test_production_configuration_cannot_pass_as_test_service(self):
        for app, env in [('journal', ['DATABASE_DSN=production']),
                         ('health', ['APP_ENV=production']),
                         ('albums', ['ALBUM_DEPLOYMENT_ENVIRONMENT=production'])]:
            with self.subTest(app=app):
                self.runtime.execute.side_effect = None
                self.runtime.execute.return_value = json.dumps([{'State': {'Running': True}, 'Config': {'Env': env}}]).encode()
                with self.assertRaises(OperationError): routes.development_environment(self.runtime, app)

    def test_all_apps_require_the_same_shared_gateway_and_their_own_identity(self):
        env = {'APP_ENV': 'development', 'PLATFORM_APPS': 'health,journal,birthday,albums',
               'ALBUM_DEPLOYMENT_ENVIRONMENT': 'test'}
        for app, bundle in [('health', 'healthapp'), ('journal', 'journalapp'), ('birthday', 'birthday'), ('albums', 'albums')]:
            prefix = 'ALBUM' if app == 'albums' else app.upper()
            env[prefix + '_APPLE_BUNDLE_ID'] = 'cn.tellyouwhat.' + bundle + '.dev'
            env[prefix + ('_API_HOST' if app == 'albums' else '_API_HOSTS')] = routes.ROUTES[app][0]
        for app in routes.ROUTES:
            self.runtime.execute = Mock(return_value=json.dumps([{'State': {'Running': True},
                       'Config': {'Env': [k + '=' + v for k, v in env.items()]}}]).encode())
            self.assertEqual(routes.development_environment(self.runtime, app), env)
            self.assertEqual(self.runtime.execute.call_args.args[1], ['docker', 'inspect', 'tellyouwhat-platform-test-gateway-1'])
            missing = dict(env, PLATFORM_APPS=','.join(a for a in routes.ROUTES if a != app))
            self.runtime.execute.return_value = json.dumps([{'State': {'Running': True},
                       'Config': {'Env': [k + '=' + v for k, v in missing.items()]}}]).encode()
            with self.assertRaisesRegex(OperationError, 'not enabled'):
                routes.development_environment(self.runtime, app)
        text = routes.render(list(routes.ROUTES))
        self.assertEqual(text.count('reverse_proxy 127.0.0.1:18790'), 4)
        self.assertNotIn(':18787', text)
        self.assertNotIn(':18092', text)

    def test_shared_readiness_uses_each_host_and_never_sends_legacy_bearer(self):
        for app in routes.ROUTES:
            def response(request, timeout):
                self.assertEqual(request.full_url, 'http://127.0.0.1:18790/readyz')
                self.assertEqual(request.get_header('Host'), routes.ROUTES[app][0])
                self.assertIsNone(request.get_header('Authorization'))
                result = io.BytesIO(b'{"code":"ok"}' if app == 'albums' else b'{"status":"ready"}')
                result.status = 200
                result.geturl = lambda: request.full_url
                return result
            opener = Mock()
            opener.open.side_effect = response
            with patch.object(routes, 'development_environment'), patch('urllib.request.build_opener', return_value=opener):
                routes.ready(self.runtime, app)
        for body in [b'[]', b'{"status":"ready"}', b'{"code":"error"}']:
            result = io.BytesIO(body)
            result.status = 200
            result.geturl = lambda: 'http://127.0.0.1:18790/readyz'
            opener = Mock()
            opener.open.return_value = result
            with patch.object(routes, 'development_environment'), patch('urllib.request.build_opener', return_value=opener):
                with self.assertRaises(OperationError):
                    routes.ready(self.runtime, 'albums')

    def old_managed_route(self):
        old = routes.render(['journal']).replace(':18790', ':18787')
        self.fragment.write_text(old)
        self.main.write_text(self.before + 'import ' + str(self.fragment) + '\n')
        (self.runtime.state / 'test-routing.json').write_text(json.dumps({
            'apps': ['journal'], 'fragment_sha256': hashlib.sha256(old.encode()).hexdigest()}))
        return old

    def test_pinned_old_route_can_be_backed_up_and_migrated_without_old_protocol(self):
        old = self.old_managed_route()
        target = self.root / 'recovery'
        routes.backup_configuration(self.runtime, target)
        self.assertEqual((target / 'test.caddy').read_text(), old)
        self.configure()
        self.assertEqual(self.fragment.read_text(), routes.render(['journal']))
        saved = Path(self.runtime.record.call_args.kwargs['recovery']) / 'fragment.before'
        self.assertEqual(saved.read_text(), old)

    def test_failed_shared_route_switch_restores_pinned_old_route(self):
        old = self.old_managed_route()
        before = self.main.read_text()
        self.fail_reload = True
        with self.assertRaisesRegex(OperationError, 'previous Caddy configuration restored'):
            self.configure()
        self.assertEqual(self.fragment.read_text(), old)
        self.assertEqual(self.main.read_text(), before)

    def test_unready_shared_app_blocks_all_routing_mutations(self):
        self.old_managed_route()
        before = self.fragment.read_text()
        with patch.object(routes, 'ready', side_effect=OperationError('App not ready')):
            with self.assertRaises(OperationError):
                routes.configure(self.runtime, ['albums'])
        self.assertEqual(self.calls, [])
        self.assertEqual(self.fragment.read_text(), before)

    def test_render_rejects_unknown_or_empty_apps(self):
        for apps in [[], ['production'], ['journal', 'unknown']]:
            with self.assertRaises(OperationError): routes.render(apps)

    def test_readiness_redirect_does_not_forward_authorization(self):
        self.assertIsNone(routes.RejectRedirects().redirect_request(
            Mock(), None, 302, 'redirect', {}, 'https://other.example/'))

    def managed_state(self):
        import hashlib
        text = routes.render(['journal'])
        self.fragment.write_text(text)
        self.main.write_text(self.before + 'import ' + str(self.fragment) + '\n')
        (self.runtime.state / 'test-routing.json').write_text(json.dumps({
            'apps': ['journal'], 'fragment_sha256': hashlib.sha256(text.encode()).hexdigest()}))

    def test_backup_preserves_exact_managed_root_fragment_and_state(self):
        self.managed_state()
        target = self.root / 'backup'
        hashes = routes.backup_configuration(self.runtime, target)
        self.assertEqual(set(hashes), {'Caddyfile', 'test.caddy', 'state.json'})
        self.assertEqual((target / 'Caddyfile').read_bytes(), self.main.read_bytes())
        self.assertEqual((target / 'test.caddy').read_bytes(), self.fragment.read_bytes())

    def test_backup_refuses_unmanaged_drift_and_symlinks(self):
        self.managed_state()
        text = self.fragment.read_text()
        self.fragment.write_text(text + '# unmanaged\n')
        with self.assertRaisesRegex(OperationError, 'managed recovery state'):
            routes.backup_configuration(self.runtime, self.root / 'backup')
        self.fragment.unlink()
        self.fragment.symlink_to(self.main)
        with self.assertRaisesRegex(OperationError, 'symlink'):
            routes.backup_configuration(self.runtime, self.root / 'backup')


if __name__ == '__main__':
    unittest.main()
