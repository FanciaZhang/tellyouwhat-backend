import json
from pathlib import Path
import shutil
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import blue_green as bg
from ops_common import OperationError, Runtime


class SharedProxyTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        config = ('APP_ENV=production\nHEALTH_API_DOMAIN=api.health.example\n'
                  'JOURNAL_API_DOMAIN=api.journal.example\nADMIN_DOMAIN=admin.example\n'
                  'ACME_EMAIL=operator@example.com\n')
        (self.root / '.env.production').write_text(config)
        self.runtime = Runtime(self.root, self.root / 'backups')
        self.fragment = self.root / 'backend.caddy'
        self.main = self.root / 'Caddyfile'
        self.legacy = self.root / 'albums.caddy'
        self.old_slot = {'slot': 'blue', 'path': '.slots/blue/old'}
        self.new_slot = {'slot': 'green', 'path': '.slots/green/new'}
        new_root = self.root / self.new_slot['path']
        new_root.mkdir(parents=True)
        (new_root / '.env.production').write_text(config + 'PLATFORM_APPS=health,journal,birthday,albums\n'
            'BIRTHDAY_API_HOSTS=api.birthday.example\nALBUM_API_HOST=api.albums.tellyouwhat.cn\n'
            'ALBUM_DEPLOYMENT_ENVIRONMENT=production\n')
        self.original = bg.shared_proxy(self.runtime.config, self.old_slot, None)['fragment']
        self.fragment.write_text(self.original)
        self.legacy_text = 'api.albums.tellyouwhat.cn {\n reverse_proxy 127.0.0.1:18091\n}\n'
        self.legacy.write_text(self.legacy_text)
        self.root_before = ('# preserve all other sites and test/OTA imports\nimport ' + str(self.fragment) + '\n'
                            'import ' + str(self.legacy) + '\nimport /etc/caddy/unrelated.caddy\n')
        self.main.write_text(self.root_before)
        self.events = []
        self.fail_at = None
        self.extra_legacy_site = False
        for name, path in [('PROXY_FILE', self.fragment), ('PROXY_ROOT', self.main), ('LEGACY_ALBUM_PROXY', self.legacy)]:
            patcher = patch.object(bg, name, path)
            patcher.start()
            self.addCleanup(patcher.stop)
        patcher = patch.object(self.runtime, 'execute', self.execute)
        patcher.start()
        self.addCleanup(patcher.stop)

    def execute(self, label, command, **kwargs):
        self.events.append(label)
        if label == self.fail_at:
            raise OperationError('injected ' + label)
        if label == 'proxy-legacy-inspect':
            routes = [self.route('api.albums.tellyouwhat.cn', '127.0.0.1:18091')]
            if self.extra_legacy_site:
                routes.append(self.route('unrelated.example', '127.0.0.1:9000'))
            return json.dumps({'routes': routes}).encode()
        if 'install' in command:
            self.assertNotIn(Path(command[-1]), (self.main, self.fragment))
            shutil.copyfile(command[-2], command[-1])
        elif 'mv' in command:
            Path(command[-2]).replace(command[-1])
        return b''

    @staticmethod
    def route(host, upstream):
        return {'match': [{'host': [host]}], 'handle': [{'handler': 'reverse_proxy', 'upstreams': [{'dial': upstream}]}]}

    def bind(self):
        self.state = {'phase': 'READY', 'current': self.old_slot, 'candidate': self.new_slot}
        bg.bind_proxy(self.runtime, self.state, self.new_slot)

    def test_full_topology_is_bound_before_switch_and_previous_snapshot_can_restore_it(self):
        self.bind()
        stored = bg.read_state(self.runtime)
        self.assertEqual(stored['current']['proxy']['fragment'], self.original)
        self.assertEqual(stored['current']['proxy']['legacyAlbums'], self.legacy_text)
        self.assertIsNone(stored['candidate']['proxy']['legacyAlbums'])
        self.assertEqual(len(stored['candidate']['proxy']['hosts']), 5)
        with patch.object(bg, 'assert_route'):
            bg.switch_proxy(self.runtime, self.new_slot)
            self.assertNotIn('import ' + str(self.legacy), self.main.read_text())
            self.assertIn('import /etc/caddy/unrelated.caddy', self.main.read_text())
            self.assertIn('api.albums.tellyouwhat.cn {', self.fragment.read_text())
            self.assertNotIn(':18091', self.fragment.read_text())
            bg.switch_proxy(self.runtime, stored['current'])
        self.assertIn('import ' + str(self.legacy), self.main.read_text())
        self.assertEqual(self.fragment.read_text(), self.original)
        self.assertEqual(self.legacy.read_text(), self.legacy_text)
        self.assertLess(self.events.index('proxy-validate'), self.events.index('proxy-commit'))

    def test_reload_failure_restores_both_root_and_fragment_without_rewriting_legacy_file(self):
        self.bind()
        self.fail_at = 'proxy-reload'
        with self.assertRaisesRegex(OperationError, 'proxy-reload'):
            bg.switch_proxy(self.runtime, self.new_slot)
        self.assertEqual(self.main.read_text(), self.root_before)
        self.assertEqual(self.fragment.read_text(), self.original)
        self.assertEqual(self.legacy.read_text(), self.legacy_text)
        self.assertIn('proxy-restore-reload', self.events)

    def test_validate_failure_changes_no_active_configuration(self):
        self.bind()
        self.fail_at = 'proxy-validate'
        with self.assertRaises(OperationError):
            bg.switch_proxy(self.runtime, self.new_slot)
        self.assertEqual(self.main.read_text(), self.root_before)
        self.assertEqual(self.fragment.read_text(), self.original)
        self.assertNotIn('proxy-commit', self.events)

    def test_legacy_fragment_containing_another_site_cannot_be_retired(self):
        self.extra_legacy_site = True
        with self.assertRaisesRegex(OperationError, 'unrecognized routes'):
            self.bind()
        self.assertEqual(self.main.read_text(), self.root_before)
        self.assertFalse((self.runtime.state / bg.STATE).exists())

    def test_changed_legacy_fragment_blocks_recovery_before_active_writes(self):
        self.bind()
        self.legacy.write_text(self.legacy_text + '# changed outside deployment\n')
        with self.assertRaisesRegex(OperationError, 'recovery snapshot'):
            bg.switch_proxy(self.runtime, self.old_slot)
        self.assertNotIn('proxy-commit', self.events)

    def test_shared_render_selects_only_enabled_hosts_and_blocks_old_development_paths(self):
        selected = bg.shared_proxy(self.runtime.config, self.new_slot, self.legacy_text)
        self.assertEqual(len(selected['hosts']), 3)
        self.assertEqual(selected['legacyAlbums'], self.legacy_text)
        self.assertNotIn('api.albums.tellyouwhat.cn', selected['fragment'])
        self.assertIn('@internal path /internal/* /_development/* /_testflight/*', selected['fragment'])
        self.assertIn('stream_close_delay 4h', selected['fragment'])
        with self.assertRaises(OperationError):
            bg.shared_proxy(dict(self.runtime.config, ACME_EMAIL='owner@example.com\nrespond 200'), self.new_slot, None)

    def test_runtime_verification_rejects_missing_host_or_a_legacy_path_upstream(self):
        self.bind()
        routes = [self.route(host, upstream) for host, upstream in self.new_slot['proxy']['hosts'].items()]
        with patch.object(bg, 'proxy_config', return_value={'routes': routes}):
            bg.assert_route(self.new_slot)
        missing = [route for route in routes if 'api.birthday.example' not in route['match'][0]['host']]
        for invalid in [missing, routes + [self.route('api.journal.example', '127.0.0.1:18787')]]:
            with patch.object(bg, 'proxy_config', return_value={'routes': invalid}), self.assertRaisesRegex(OperationError, 'exclusively'):
                bg.assert_route(self.new_slot)

    def test_first_migration_backup_keeps_original_proxy_and_legacy_fragment(self):
        from contextlib import ExitStack
        from ops_offsite import create_recovery_bundle
        old_root = self.root / self.old_slot['path']
        old_root.mkdir(parents=True)
        shutil.copyfile(self.root / '.env.production', old_root / '.env.production')
        self.runtime.backups.mkdir()
        bg.write_state(self.runtime, {'phase': 'STABLE', 'current': self.old_slot, 'previous': None})
        with ExitStack() as stack:
            for module in ('test_routes', 'apple_delivery_artifacts', 'albums_artifacts',
                           'platform_test_artifacts', 'journal_development_artifacts'):
                stack.enter_context(patch(module + '.backup_configuration', return_value={}))
            stack.enter_context(patch('ops_backup.crypt', side_effect=lambda _runtime, source, target: shutil.copyfile(source, target)))
            result = create_recovery_bundle(self.runtime, 'synthetic.sql.gz.enc')
        with tarfile.open(self.runtime.backups / result['recovery_filename'], 'r:gz') as archive:
            selection = json.load(archive.extractfile('recovery/selection.json'))
        self.assertEqual(selection['slots']['current']['proxy'],
                         {'fragment': self.original, 'legacyAlbums': self.legacy_text})
        self.assertNotIn('proxy', bg.read_state(self.runtime)['current'])
