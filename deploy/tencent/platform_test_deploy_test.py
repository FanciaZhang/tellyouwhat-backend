import hashlib
import io
import json
import shutil
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import platform_test_deploy as dev
from ops_common import OperationError


class PlatformTestBootstrapTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name) / 'backend'
        self.root.mkdir(mode=0o700)
        self.source = 'a' * 40
        self.folder = self.root / '.platform-test/releases' / self.source
        self.folder.mkdir(parents=True, mode=0o700)
        for parent in (self.folder.parent, self.folder.parent.parent):
            parent.chmod(0o700)
        self.runtime = Mock(root=self.root, state=self.root / '.operations', config={})
        self.runtime.state.mkdir()
        (self.runtime.state / 'blue-green.json').write_text('{"phase":"STABLE","current":{"path":"production"}}')
        for name in ('operations', 'deployment'):
            (self.runtime.state / (name + '.lock')).touch(mode=0o600)
        self.app = {'PLATFORM_APPS': 'birthday',
                    'BIRTHDAY_APPLE_BUNDLE_ID': 'cn.tellyouwhat.birthday.dev', 'BIRTHDAY_APPLE_TEAM_ID': 'T6DD96HY2B',
                    'BIRTHDAY_API_HOSTS': dev.HOST, 'BIRTHDAY_APP_ATTEST_ENV': 'production',
                    'AI_PROJECT_MONTHLY_BUDGET_CNY': '10', 'BIRTHDAY_ALLOWED_APP_BUILDS': '4'}
        self.compose = {'PLATFORM_TEST_ENV_FILE': str(self.folder / 'runtime.env'), 'PLATFORM_TEST_IMAGE_TAG': self.source,
                        'PLATFORM_TEST_API_PORT': '18790', 'PLATFORM_TEST_NETWORK_CIDR': '172.29.93.0/24',
                        'PLATFORM_TEST_MYSQL_PASSWORD': '1' * 64, 'PLATFORM_TEST_MYSQL_ROOT_PASSWORD': '2' * 64,
                        'PLATFORM_TEST_REDIS_PASSWORD': '3' * 64}
        self.write_archive()
        (self.folder / 'compose.platform-test.yaml').write_bytes(b'name: fixture\n')
        self.save()

    def write_archive(self, explicit_apps=True, migration=b'x'):
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name in ('deploy/tencent/platform-test.Dockerfile', 'go.mod', 'go.sum', 'cmd/gateway/main.go',
                         'cmd/migrate/main.go', 'compose.platform-test.yaml', 'migrations/0001_example.sql',
                         'internal/config/platform.go'):
                data = b'name: fixture\n' if name == 'compose.platform-test.yaml' else b'x'
                if name == 'internal/config/platform.go' and explicit_apps:
                    data = b'value("PLATFORM_APPS", "health,journal")'
                if name.startswith('migrations/'):
                    data = migration
                item = tarfile.TarInfo(name)
                item.size = len(data)
                archive.addfile(item, io.BytesIO(data))

    def enable_health(self):
        self.app.update(PLATFORM_APPS='health,birthday',
                        HEALTH_APPLE_BUNDLE_ID='cn.tellyouwhat.healthapp.dev', HEALTH_APPLE_TEAM_ID='T6DD96HY2B',
                        HEALTH_API_HOSTS=dev.HEALTH_HOST, HEALTH_APP_ATTEST_ENV='production', HEALTH_ALLOWED_APP_BUILDS='4',
                        HEALTH_SCHEMA_MANIFEST_PATH='/config/schema-manifest.json',
                        HEALTH_ARK_BASE_URL='https://ark.cn-beijing.volces.com', HEALTH_ARK_API_KEY='health-test-key',
                        HEALTH_ARK_MAX_INPUT_CNY_PER_MILLION='12', HEALTH_ARK_MAX_OUTPUT_CNY_PER_MILLION='30',
                        TOS_ENDPOINT='https://tos-cn-beijing.volces.com', TOS_REGION='cn-beijing', TOS_BUCKET='health-ai-test',
                        TOS_ACCESS_KEY='test-access', TOS_SECRET_KEY='test-secret')
        self.app.update({k: 'ep-health-test' for k in dev.HEALTH_ENDPOINTS})
        self.runtime.config = {k: 'production-' + k for k in ('HEALTH_ARK_API_KEY', 'TOS_ACCESS_KEY', 'TOS_SECRET_KEY', *dev.HEALTH_ENDPOINTS)}
        self.save()

    def enable_journal(self):
        self.enable_health()
        self.app.update(dev.journal.FIXED)
        self.app.update(PLATFORM_APPS='health,journal,birthday', JOURNAL_ALLOWED_APP_BUILDS='20')
        for key in dev.journal.RESOURCE_KEYS:
            self.app[key] = 'ep-journal-test' if key.endswith('_MODEL_ID') else 'journal-test-key'
        self.app['JOURNAL_IMAGE_MAX_COST_NANOS'] = '100000000'
        approved = self.root.parent / 'journal-development/runtime.env'
        approved.parent.mkdir(mode=0o700, exist_ok=True)
        approved.write_text(''.join(source + '=' + self.app[key] + '\n' for key, source in dev.journal.RESOURCE_KEYS.items()))
        approved.chmod(0o600)
        self.save()

    def add_illustration_migration(self, contents=None, name='0065_journal_illustration_jobs.sql'):
        if contents is None:
            contents = (Path(__file__).resolve().parents[2] / 'migrations' / name).read_bytes()
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            item = tarfile.TarInfo('migrations/' + name)
            item.size = len(contents)
            archive.addfile(item, io.BytesIO(contents))
        self.save()

    def save(self):
        for name, values in [('runtime.env', self.app), ('compose.env', self.compose)]:
            (self.folder / name).write_text(''.join(k + '=' + v + '\n' for k, v in values.items()))
        record = {'source': self.source, 'files': {n: hashlib.sha256((self.folder / n).read_bytes()).hexdigest() for n in dev.FILES}}
        (self.folder / 'selection.json').write_text(json.dumps(record))
        for p in self.folder.iterdir():
            p.chmod(0o600)

    def test_selected_source_rejects_mixed_identity_credentials_and_configuration(self):
        self.assertEqual(dev.selection(self.runtime, self.source)[2], ['0001_example.sql'])
        for target, key, value in [(self.app, 'BIRTHDAY_APPLE_BUNDLE_ID', 'cn.tellyouwhat.birthday'),
                                   (self.app, 'BIRTHDAY_APP_ATTEST_ENV', 'development'),
                                   (self.app, 'AI_PROJECT_MONTHLY_BUDGET_CNY', '400'),
                                   (self.app, 'TOS_SECRET_KEY', 'synthetic'),
                                   (self.app, 'DATABASE_DSN', 'production'),
                                   (self.compose, 'PLATFORM_TEST_NETWORK_CIDR', '172.29.92.0/24')]:
            old = target.get(key)
            target[key] = value
            self.save()
            with self.subTest(key=key), self.assertRaises(OperationError):
                dev.selection(self.runtime, self.source)
            if old is None:
                target.pop(key)
            else:
                target[key] = old
        self.save()
        (self.folder / 'source.tar').write_bytes(b'tampered')
        with self.assertRaisesRegex(OperationError, 'changed'):
            dev.selection(self.runtime, self.source)

    def test_private_boundary_and_source_compose_are_checked(self):
        (self.folder / 'runtime.env').chmod(0o640)
        with self.assertRaises(OperationError):
            dev.selection(self.runtime, self.source)
        (self.folder / 'compose.platform-test.yaml').write_bytes(b'other: source\n')
        self.save()
        with self.assertRaisesRegex(OperationError, 'Compose differs'):
            dev.selection(self.runtime, self.source)

    def test_automatic_enrollment_still_requires_approved_builds(self):
        self.save()
        dev.selection(self.runtime, self.source)
        self.app['BIRTHDAY_ALLOWED_APP_BUILDS'] = ''
        self.save()
        with self.assertRaises(OperationError):
            dev.selection(self.runtime, self.source)

    def test_retained_recovery_uses_its_source_contract_but_new_source_rejects_old_flags(self):
        self.app.pop('PLATFORM_APPS')
        self.app.update(BIRTHDAY_AI_ENABLED='true', BIRTHDAY_AI_ONLY='true')
        self.save()
        with self.assertRaisesRegex(OperationError, 'selected explicitly'):
            dev.selection(self.runtime, self.source)
        self.write_archive(explicit_apps=False)
        self.save()
        dev.selection(self.runtime, self.source)
        self.app['PLATFORM_APPS'] = 'birthday'
        self.save()
        with self.assertRaisesRegex(OperationError, 'source contract'):
            dev.selection(self.runtime, self.source)

    def test_health_requires_its_own_complete_test_resources(self):
        self.enable_health()
        dev.selection(self.runtime, self.source)
        self.assertEqual(dev.running_services(self.folder), ('gateway', 'worker'))
        for key, invalid in [('HEALTH_APPLE_BUNDLE_ID', 'cn.tellyouwhat.healthapp'),
                             ('HEALTH_API_HOSTS', 'api.health.tellyouwhat.cn'),
                             ('TOS_BUCKET', 'health-ai'), ('TOS_SECRET_KEY', ''),
                             ('HEALTH_ARK_ENDPOINT_MEAL_PHOTO_CAPTURE', 'doubao-direct-model'),
                             ('HEALTH_ALLOWED_APP_BUILDS', ''), ('HEALTH_APP_STORE_PRIVATE_KEY_PATH', '/secrets/formal.p8'),
                             ('HEALTH_ARK_MAX_INPUT_CNY_PER_MILLION', ''),
                             ('HEALTH_ARK_MAX_INPUT_CNY_PER_MILLION', '0.8'),
                             ('HEALTH_ARK_MAX_OUTPUT_CNY_PER_MILLION', '8'),
                             ('PLATFORM_APPS', 'health,journal,birthday')]:
            old = self.app.get(key)
            self.app[key] = invalid
            self.save()
            with self.subTest(key=key), self.assertRaises(OperationError):
                dev.selection(self.runtime, self.source)
            if old is None:
                self.app.pop(key)
            else:
                self.app[key] = old

    def execute(self, label, command, **kwargs):
        if label == 'platform-test-schema':
            return b'1\t0001_example.sql\n1\n0\n'
        if label.endswith('-worker-status'):
            return b'{"httpEnabled":true,"backgroundEnabled":true}'
        if label in ('platform-test-running-gateway', 'platform-test-running-worker'):
            service = 'worker' if label.endswith('-worker') else 'gateway'
            return json.dumps([{'State': {'Running': True, 'OOMKilled': False}, 'RestartCount': 0,
                                'Config': {'Image': dev.PROJECT + '-' + service + ':' + self.source}}]).encode()
        return b''

    def activate(self):
        with patch.object(dev.fcntl, 'flock', side_effect=BlockingIOError), patch.object(dev, 'assert_route'), patch.object(dev, 'ready'):
            return dev.activate(self.runtime, self.source)

    def test_fresh_stack_starts_only_birthday_and_preserves_existing_services(self):
        self.runtime.execute.side_effect = self.execute
        result = self.activate()
        self.assertEqual(result['apps'], ['birthday'])
        self.assertFalse(result['real_app_attest_verified'])
        commands = [c.args[1] for c in self.runtime.execute.call_args_list]
        self.assertFalse(any('down' in c or '--volumes' in c or 'birthday-private-development' in c for c in commands))
        starts = [c for c in commands if 'up' in c]
        self.assertEqual(starts[-1][-1], 'gateway')
        with self.assertRaisesRegex(OperationError, 'already exists'):
            self.activate()

    def test_failed_start_keeps_database_and_stops_only_new_gateway(self):
        self.runtime.execute.side_effect = self.execute
        with patch.object(dev.fcntl, 'flock', side_effect=BlockingIOError), patch.object(dev, 'assert_route'), \
                patch.object(dev, 'ready', side_effect=OperationError('not ready')):
            with self.assertRaisesRegex(OperationError, 'not ready'):
                dev.activate(self.runtime, self.source)
        self.assertFalse((self.runtime.state / 'platform-test.json').exists())
        self.assertEqual(self.runtime.execute.call_args.args[1][-4:], ['stop', '-t', '30', 'gateway'])

    def test_unlocked_execution_and_preexisting_data_are_rejected_before_build(self):
        with self.assertRaisesRegex(OperationError, 'both owner locks'):
            dev.activate(self.runtime, self.source)
        self.runtime.execute.side_effect = lambda label, *a, **k: b'volume' if label == 'platform-test-volumes' else b''
        with self.assertRaisesRegex(OperationError, 'already exists'):
            self.activate()
        self.assertFalse(any(c.args[0].startswith('platform-test-build') for c in self.runtime.execute.call_args_list))

    def prepare_update(self):
        self.old_source, self.old_folder = self.source, self.folder
        self.previous = {'source': self.source, 'release': str(self.folder), 'migrations': 1, 'latest_migration': '0001_example.sql'}
        (self.runtime.state / 'platform-test.json').write_text(json.dumps(self.previous))
        (self.runtime.state / 'platform-test.json').chmod(0o600)
        owner = self.runtime.state / 'artifact-owners/platform-test.json'
        owner.parent.mkdir()
        owner.write_text(json.dumps({'current': {'source': self.source, 'paths': [str(self.folder)]}}))
        owner.chmod(0o600)
        self.source = 'b' * 40
        self.folder = self.old_folder.parent / self.source
        shutil.copytree(self.old_folder, self.folder)
        self.compose.update(PLATFORM_TEST_ENV_FILE=str(self.folder / 'runtime.env'), PLATFORM_TEST_IMAGE_TAG=self.source)
        self.app.pop('BIRTHDAY_DEV_ACTIVATION_SECRET', None)
        self.app['BIRTHDAY_ALLOWED_APP_BUILDS'] = '4'
        self.save()
        self.runtime.backups = self.root / 'backups'
        def execute(label, command, **kwargs):
            if label.endswith('-worker-status'):
                return b'{"httpEnabled":true,"backgroundEnabled":true}'
            if label in ('platform-test-update-running-gateway', 'platform-test-update-check-gateway',
                         'platform-test-rollback-check-gateway', 'platform-test-update-check-worker'):
                source = self.source if label.startswith('platform-test-update-check') else self.old_source
                service = 'worker' if label.endswith('-worker') else 'gateway'
                return json.dumps([{'State': {'Running': True}, 'RestartCount': 0,
                                    'Config': {'Image': dev.PROJECT + '-' + service + ':' + source}}]).encode()
            if label == 'platform-test-update-schema':
                return b'1\t0001_example.sql\n'
            if label == 'platform-test-migration-names':
                return b'0001_example.sql\n'
            return b''
        self.runtime.execute.side_effect = execute

    def update(self, *, ready_error=None, backup_error=None):
        with patch.object(dev.fcntl, 'flock', side_effect=BlockingIOError), patch.object(dev, 'assert_route'), \
                patch.object(dev, 'ready', side_effect=ready_error), \
                patch('ops_backup.create_backup', return_value={'filename': 'verified-checkpoint'}, side_effect=backup_error), \
                patch('ops_backup.verify_backup', return_value={'recovery_platform_test': {'state.json': 'verified'}}):
            return dev.activate(self.runtime, self.source, self.old_source)

    def test_enrollment_update_backs_up_and_preserves_database_and_matched_rollback(self):
        self.prepare_update()
        result = self.update()
        self.assertEqual(result['previous'], self.previous)
        self.assertEqual(result['pre_update_backup'], 'verified-checkpoint')
        commands = [c.args[1] for c in self.runtime.execute.call_args_list]
        starts = [c for c in commands if 'up' in c]
        self.assertEqual(len(starts), 1)
        self.assertEqual(starts[0][-4:], ['-d', '--no-build', '--no-deps', 'gateway'])
        self.assertFalse(any('down' in c or '--volumes' in c or 'run' in c for c in commands))

    def test_registered_legacy_source_can_upgrade_to_explicit_apps_without_client_adapters(self):
        self.app.pop('PLATFORM_APPS')
        self.app.update(BIRTHDAY_AI_ENABLED='true', BIRTHDAY_AI_ONLY='true')
        self.write_archive(explicit_apps=False)
        self.save()
        self.prepare_update()
        self.app.pop('BIRTHDAY_AI_ENABLED')
        self.app.pop('BIRTHDAY_AI_ONLY')
        self.app['PLATFORM_APPS'] = 'birthday'
        self.write_archive()
        self.save()
        result = self.update()
        self.assertEqual(result['apps'], ['birthday'])
        self.assertEqual(result['previous'], self.previous)

    def test_update_rejects_changed_credentials_and_backup_failure_before_build(self):
        self.prepare_update()
        self.app['BIRTHDAY_ARK_API_KEY'] = 'changed-provider'
        self.save()
        with self.assertRaisesRegex(OperationError, 'unrelated configuration'):
            self.update()
        self.app.pop('BIRTHDAY_ARK_API_KEY')
        self.save()
        with self.assertRaisesRegex(OperationError, 'backup failed'):
            self.update(backup_error=OperationError('backup failed'))
        self.assertFalse(any(c.args[0].startswith('platform-test-build') for c in self.runtime.execute.call_args_list))

    def test_failed_update_restores_old_configuration_without_replacing_state(self):
        self.prepare_update()
        with self.assertRaisesRegex(OperationError, 'not ready'):
            self.update(ready_error=[OperationError('not ready'), None])
        self.assertEqual(json.loads((self.runtime.state / 'platform-test.json').read_text()), self.previous)
        rollback = next(c.args[1] for c in self.runtime.execute.call_args_list if c.args[0] == 'platform-test-update-rollback')
        self.assertIn(str(self.old_folder / 'compose.env'), rollback)
        self.assertEqual(rollback[-4:], ['-d', '--no-build', '--no-deps', 'gateway'])

    def test_health_admission_starts_and_registers_worker_without_changing_existing_database(self):
        self.prepare_update()
        self.enable_health()
        result = self.update()
        self.assertEqual(result['apps'], ['health', 'birthday'])
        self.assertEqual(result['previous'], self.previous)
        commands = [c.args[1] for c in self.runtime.execute.call_args_list]
        start = next(c for c in commands if 'up' in c)
        self.assertEqual(start[-2:], ['gateway', 'worker'])
        self.assertFalse(any('down' in c or '--volumes' in c or 'run' in c for c in commands))
        builds = [c for c in commands if 'build' in c]
        self.assertEqual({c[c.index('--target') + 1] for c in builds}, {'gateway', 'worker', 'migrate'})

    def test_health_bootstrap_also_checks_worker_and_rejects_shared_credentials(self):
        self.enable_health()
        self.runtime.execute.side_effect = self.execute
        self.app['TOS_ACCESS_KEY'] = self.runtime.config['TOS_ACCESS_KEY']
        self.save()
        with self.assertRaisesRegex(OperationError, 'differ from verified production'):
            self.activate()
        self.app['TOS_ACCESS_KEY'] = 'test-access'
        self.save()
        result = self.activate()
        self.assertEqual(result['apps'], ['health', 'birthday'])
        self.assertIn('platform-test-running-worker-status', [c.args[0] for c in self.runtime.execute.call_args_list])

    def test_worker_must_report_active_lifecycle_after_startup(self):
        self.enable_health()
        calls = []
        def execute(label, command, **kwargs):
            calls.append(label)
            if label.endswith('-worker-status'):
                return b'{"httpEnabled":false,"backgroundEnabled":false}'
            return self.execute(label, command, **kwargs)
        self.runtime.execute.side_effect = execute
        with patch.object(dev.time, 'sleep'), self.assertRaisesRegex(OperationError, 'not accepting work'):
            dev.verify_running(self.runtime, self.folder, self.source, 'platform-test-running')
        self.assertEqual(calls.count('platform-test-running-worker-status'), 30)

    def test_health_admission_rejects_production_resources_before_backup_or_build(self):
        self.prepare_update()
        self.enable_health()
        for key in ('HEALTH_ARK_API_KEY', 'TOS_ACCESS_KEY', 'TOS_SECRET_KEY', *dev.HEALTH_ENDPOINTS):
            old = self.app[key]
            self.app[key] = self.runtime.config[key]
            if key in dev.HEALTH_ENDPOINTS:
                self.runtime.config[key] = self.app[key] = 'ep-production'
            self.save()
            with self.subTest(key=key), self.assertRaisesRegex(OperationError, 'differ from verified production'):
                self.update()
            self.app[key] = old
        self.assertFalse(any(c.args[0].startswith('platform-test-build') for c in self.runtime.execute.call_args_list))

    def test_health_start_failure_stops_new_worker_and_restores_old_gateway(self):
        self.prepare_update()
        self.enable_health()
        with self.assertRaisesRegex(OperationError, 'not ready'):
            self.update(ready_error=[OperationError('not ready'), None])
        self.assertEqual(json.loads((self.runtime.state / 'platform-test.json').read_text()), self.previous)
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        self.assertLess(labels.index('platform-test-update-stop-worker'), labels.index('platform-test-update-rollback'))
        self.assertIn('platform-test-rollback-check-gateway', labels)

    def test_worker_cleanup_failure_still_restores_old_gateway_and_reports_failure(self):
        self.prepare_update()
        self.enable_health()
        execute = self.runtime.execute.side_effect
        def fail_worker_stop(label, command, **kwargs):
            if label == 'platform-test-update-stop-worker':
                raise OperationError('worker stop failed')
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = fail_worker_stop
        with self.assertRaisesRegex(OperationError, 'worker stop failed'):
            self.update(ready_error=[OperationError('not ready'), None])
        self.assertEqual(json.loads((self.runtime.state / 'platform-test.json').read_text()), self.previous)
        self.assertIn('platform-test-rollback-check-gateway', [c.args[0] for c in self.runtime.execute.call_args_list])

    def test_configuration_update_rejects_schema_changes(self):
        self.prepare_update()
        self.write_archive(migration=b'ALTER TABLE existing_users DROP COLUMN data;')
        self.save()
        with self.assertRaisesRegex(OperationError, 'database schemas'):
            self.update()

    def test_journal_admission_uses_one_gateway_and_existing_test_resources(self):
        self.prepare_update()
        self.enable_journal()
        result = self.update()
        self.assertEqual(result['apps'], ['health', 'journal', 'birthday'])
        commands = [c.args[1] for c in self.runtime.execute.call_args_list]
        starts = [c for c in commands if 'up' in c]
        self.assertEqual(len(starts), 1)
        self.assertEqual(starts[0][-2:], ['gateway', 'worker'])
        self.assertFalse(any('journal-private-development' in c or 'down' in c for c in commands))

    def test_journal_rejects_formal_identity_and_unmounted_or_unscoped_credentials(self):
        self.enable_journal()
        for key, bad in [('JOURNAL_APPLE_BUNDLE_ID', 'cn.tellyouwhat.journalapp'),
                         ('JOURNAL_API_HOSTS', 'api.journal.tellyouwhat.cn'),
                         ('JOURNAL_APP_ATTEST_ENV', 'development'),
                         ('JOURNAL_RECORDING_ENABLED', 'true'),
                         ('JOURNAL_APP_STORE_PRIVATE_KEY_PATH', '/secrets/formal.p8'),
                         ('JOURNAL_IMAGE_MAX_COST_NANOS', '2000000000')]:
            before = dict(self.app)
            self.app[key] = bad
            self.save()
            with self.subTest(key=key), self.assertRaises(OperationError):
                dev.selection(self.runtime, self.source)
            self.app = before
        self.save()
        for key in ('JOURNAL_ARK_API_KEY', 'JOURNAL_SPEECH_API_KEY', 'JOURNAL_IMAGE_API_KEY', 'JOURNAL_ARK_PRO_MODEL_ID'):
            self.runtime.config[key] = self.app[key]
            with self.subTest(key=key), self.assertRaisesRegex(OperationError, 'differ from production'):
                self.activate()
            self.runtime.config.pop(key)
        self.app['JOURNAL_SPEECH_API_KEY'] = 'unapproved'
        self.save()
        with self.assertRaisesRegex(OperationError, 'restricted development resources'):
            self.activate()
        self.assertFalse(any('build' in c.args[1] for c in self.runtime.execute.call_args_list))

    def test_readiness_checks_every_selected_app_host(self):
        self.enable_journal()
        seen = []
        def respond(request, **kwargs):
            seen.append(request.get_header('Host'))
            result = io.BytesIO(b'{"status":"ready"}')
            result.status = 200
            return result
        opener = Mock()
        opener.open.side_effect = respond
        with patch.object(dev.urllib.request, 'build_opener', return_value=opener):
            dev.ready(self.folder)
        self.assertEqual(seen, [dev.HOST, dev.HEALTH_HOST, dev.journal.HOST])

    def prepare_migration(self):
        self.prepare_update()
        self.add_illustration_migration()
        execute = self.runtime.execute.side_effect
        self.migrated = False
        def migrate(label, command, **kwargs):
            if label == 'platform-test-update-migrate':
                self.migrated = True
            if label == 'platform-test-migration-names':
                return b'0001_example.sql\n' + (b'0065_journal_illustration_jobs.sql\n' if self.migrated else b'')
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = migrate

    def test_reviewed_additive_migration_updates_database_metadata_before_activation(self):
        self.prepare_migration()
        result = self.update()
        self.assertEqual(result['migrations'], 2)
        self.assertEqual(result['schema_source'], self.source)
        self.assertEqual(result['previous']['source'], self.old_source)
        self.assertEqual(result['previous']['migrations'], 2)
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        self.assertLess(labels.index('platform-test-update-migrate'), labels.index('platform-test-update-start'))
        self.assertFalse(any('down' in c.args[1] or '--volumes' in c.args[1] for c in self.runtime.execute.call_args_list))

    def test_failed_candidate_keeps_additive_schema_and_restores_old_api(self):
        self.prepare_migration()
        with self.assertRaisesRegex(OperationError, 'not ready'):
            self.update(ready_error=[OperationError('not ready'), None])
        retained = json.loads((self.runtime.state / 'platform-test.json').read_text())
        self.assertEqual(retained['source'], self.old_source)
        self.assertEqual(retained['migrations'], 2)
        self.assertEqual(retained['schema_source'], self.source)
        self.assertIn('platform-test-update-rollback', [c.args[0] for c in self.runtime.execute.call_args_list])

    def test_failed_migration_does_not_switch_or_restore_data(self):
        self.prepare_migration()
        execute = self.runtime.execute.side_effect
        def fail(label, command, **kwargs):
            if label == 'platform-test-update-migrate':
                raise OperationError('migration failed')
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = fail
        with self.assertRaisesRegex(OperationError, 'migration failed'):
            self.update()
        self.assertEqual(json.loads((self.runtime.state / 'platform-test.json').read_text()), self.previous)
        self.assertFalse(any('up' in c.args[1] or 'stop' in c.args[1] for c in self.runtime.execute.call_args_list))

    def test_unknown_or_changed_additive_migration_is_rejected(self):
        self.prepare_update()
        self.add_illustration_migration(b'CREATE TABLE unreviewed (id INT);')
        with self.assertRaisesRegex(OperationError, 'unreviewed database schemas'):
            self.update()
        self.assertFalse(any('build' in c.args[1] for c in self.runtime.execute.call_args_list))

    def test_candidate_retry_after_rollback_does_not_repeat_migration(self):
        self.prepare_migration()
        with self.assertRaises(OperationError):
            self.update(ready_error=[OperationError('not ready'), None])
        execute = self.runtime.execute.side_effect
        def already_migrated(label, command, **kwargs):
            if label == 'platform-test-update-schema':
                return b'2\t0065_journal_illustration_jobs.sql\n'
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = already_migrated
        self.runtime.execute.reset_mock()
        result = self.update()
        self.assertEqual(result['migrations'], 2)
        self.assertNotIn('platform-test-update-migrate', [c.args[0] for c in self.runtime.execute.call_args_list])

    def test_different_applied_names_are_rejected_even_if_count_and_latest_match(self):
        self.prepare_update()
        execute = self.runtime.execute.side_effect
        def altered(label, command, **kwargs):
            if label == 'platform-test-migration-names':
                return b'0001_unexpected.sql\n'
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = altered
        with self.assertRaisesRegex(OperationError, 'either reviewed release'):
            self.update()
        self.assertFalse(any('build' in c.args[1] for c in self.runtime.execute.call_args_list))


if __name__ == '__main__':
    unittest.main()
