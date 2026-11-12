import hashlib
import base64
import copy
import io
import json
import os
import signal
import subprocess
import sys
import shutil
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import platform_test_deploy as dev
from ops_common import OperationError

READ_ALOUD_MIGRATIONS = ('0068_journal_read_aloud_quota.sql', '0069_journal_voice_catalog.sql',
                         '0070_journal_read_aloud_consent.sql', '0071_journal_simulator_read_aloud_consents.sql')


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
        imports = patch.dict(os.environ, {'PYTHONPATH': str(Path(__file__).resolve().parent) + os.pathsep + os.environ.get('PYTHONPATH', '')})
        imports.start(); self.addCleanup(imports.stop)
        scripts = self.root / 'deploy/tencent'
        scripts.mkdir(parents=True, mode=0o755)
        scripts.parent.chmod(0o755)
        for path in Path(__file__).resolve().parent.glob('*.py'):
            if not path.name.endswith('_test.py'):
                shutil.copy2(path, scripts / path.name)
        (scripts.parent / 'albums').mkdir(mode=0o755)
        for name in ('backup.py', 'deletion_ledger.py', 'asset_deletion_ledger.py', 'collection_deletion_ledger.py'):
            shutil.copy2(Path(__file__).resolve().parent.parent / 'albums' / name, scripts.parent / 'albums' / name)
        (self.root / 'migrations').mkdir(mode=0o755)
        for name in ('0077_album_collections.sql', '0078_album_asset_deletion_ledger.sql'):
            shutil.copy2(Path(__file__).resolve().parents[2] / 'migrations' / name, self.root / 'migrations' / name)
        (self.runtime.state / 'blue-green.json').write_text('{"phase":"STABLE","current":{"path":"production"}}')
        for name in ('operations', 'deployment'):
            (self.runtime.state / (name + '.lock')).touch(mode=0o600)
        self.app = {'PLATFORM_APPS': 'birthday',
                    'BIRTHDAY_APPLE_BUNDLE_ID': 'cn.tellyouwhat.birthday.dev', 'BIRTHDAY_APPLE_TEAM_ID': 'T6DD96HY2B',
                    'BIRTHDAY_API_HOSTS': dev.HOST, 'BIRTHDAY_APP_ATTEST_ENV': 'production',
                    'AI_PROJECT_MONTHLY_BUDGET_CNY': '10', 'BIRTHDAY_ALLOWED_APP_BUILDS': '4'}
        self.app['PAYLOAD_ENCRYPTION_KEY'] = base64.b64encode(b'p' * 32).decode()
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
        self.app.update(PLATFORM_APPS='health,journal,birthday', JOURNAL_ALLOWED_APP_BUILDS='20', JOURNAL_RECORDING_ENABLED='false')
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

    def enable_read_aloud(self):
        self.enable_journal()
        self.app.update(JOURNAL_TTS_ENABLED='true', JOURNAL_TTS_API_KEY_FILE=dev.journal.KEY_FILE,
                        JOURNAL_TTS_MONTHLY_CHARACTERS='10000', JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS='1000',
                        JOURNAL_READ_ALOUD_SIMULATOR_CREDENTIALS_FILE=dev.journal.SIMULATOR_FILE)
        name = dev.journal.READ_ALOUD_OVERLAY
        body = (Path(__file__).resolve().parents[2] / name).read_bytes()
        (self.folder / name).write_bytes(body)
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            item = tarfile.TarInfo(name)
            item.size = len(body)
            archive.addfile(item, io.BytesIO(body))
        self.save()

    def set_topology(self, *, mounted):
        root = Path(__file__).resolve().parents[2]
        path = root / ('compose.platform-test.yaml' if mounted else 'deploy/tencent/testdata/platform-test-before-recording.yaml')
        data = path.read_bytes()
        with tarfile.open(self.folder / 'source.tar') as archive:
            entries = [(m.name, archive.extractfile(m).read()) for m in archive.getmembers()]
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name, content in entries:
                if name == 'compose.platform-test.yaml':
                    content = data
                item = tarfile.TarInfo(name)
                item.size = len(content)
                archive.addfile(item, io.BytesIO(content))
        (self.folder / 'compose.platform-test.yaml').write_bytes(data)
        self.save()

    def save(self):
        for name, values in [('runtime.env', self.app), ('compose.env', self.compose)]:
            (self.folder / name).write_text(''.join(k + '=' + v + '\n' for k, v in values.items()))
        record = {'source': self.source, 'files': {n: hashlib.sha256((self.folder / n).read_bytes()).hexdigest() for n in dev.selection_files(self.folder)}}
        (self.folder / 'selection.json').write_text(json.dumps(record))
        for p in self.folder.iterdir():
            p.chmod(0o600)

    def recovery_sql_fixture(self):
        data = {}
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            for name in ('0077_album_collections.sql', '0078_album_asset_deletion_ledger.sql'):
                path = 'migrations/' + name
                content = (self.root / path).read_bytes()
                entry = tarfile.TarInfo(path); entry.size = len(content)
                archive.addfile(entry, io.BytesIO(content))
                data[path] = content
        self.save()
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': self.source})); state.chmod(0o600)
        return data

    def test_recovery_sql_inputs_install_and_rollback_with_their_helpers(self):
        data = self.recovery_sql_fixture()
        for name in data:
            (self.root / name).unlink()
        (self.root / 'migrations').rmdir()
        replace = Path.replace
        def interrupted(path, destination):
            if Path(destination).name == '0078_album_asset_deletion_ledger.sql':
                raise OSError('controlled SQL input install failure')
            return replace(path, destination)
        with patch.object(Path, 'replace', interrupted), self.assertRaisesRegex(OSError, 'SQL input install failure'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.fail('partial SQL input installation admitted')
        self.assertFalse((self.root / 'migrations').exists())
        for error in (RuntimeError, KeyboardInterrupt):
            with self.assertRaisesRegex(error, 'activation failed'):
                with dev.test_operations_transition(self.runtime, self.source):
                    for name, content in data.items():
                        self.assertEqual((self.root / name).read_bytes(), content)
                    raise error('activation failed')
            self.assertFalse((self.root / 'migrations').exists())
        with dev.test_operations_transition(self.runtime, self.source):
            pass
        for name, content in data.items():
            self.assertEqual((self.root / name).read_bytes(), content)
        self.runtime.execute.assert_not_called()

    def test_recovery_sql_inputs_reject_drift_and_symlinks(self):
        data = self.recovery_sql_fixture()
        installed = self.root / next(iter(data))
        installed.write_bytes(b'-- unrelated installed SQL\n')
        with self.assertRaisesRegex(OperationError, 'rollback source'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.fail('unregistered SQL replaced')
        self.assertEqual(installed.read_bytes(), b'-- unrelated installed SQL\n')
        installed.unlink()
        original = self.root.parent / 'sql-original'
        original.write_bytes(data[next(iter(data))])
        installed.symlink_to(original)
        with self.assertRaisesRegex(OperationError, 'operator owned'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.fail('symlinked recovery input accepted')
        self.assertTrue(installed.is_symlink())
        self.assertEqual(original.read_bytes(), data[next(iter(data))])

    def test_test_only_operations_update_rolls_back_and_rejects_drift(self):
        name = 'platform_test_journal.py'
        installed = self.root / 'deploy/tencent' / name
        original = b'# previous registered test validator\n'
        installed.write_bytes(original)
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            entry = tarfile.TarInfo('deploy/tencent/' + name)
            entry.size = len(original)
            archive.addfile(entry, io.BytesIO(original))
        self.save()
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': self.source}))
        state.chmod(0o600)
        with self.assertRaisesRegex(RuntimeError, 'activation failed'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.assertNotEqual(installed.read_bytes(), original)
                raise RuntimeError('activation failed')
        self.assertEqual(installed.read_bytes(), original)
        installed.write_bytes(b'# unexpected drift\n')
        with self.assertRaisesRegex(OperationError, 'rollback source'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.fail('accepted changed operations')
        self.assertEqual(installed.read_bytes(), b'# unexpected drift\n')

    def test_shared_test_ownership_update_is_pinned_and_restored_after_failure(self):
        name = 'ops_common.py'
        current = (Path(__file__).parent / name).read_bytes()
        old = current.replace(
            b"        if registered_test and (script.name.startswith('platform_test_')\n                                or script.name in ('test_routes.py', 'albums_checkpoint.py', 'albums_artifacts.py', 'albums_recovery.py')):\n",
            b"        if registered_test and (script.name.startswith('platform_test_') or script.name == 'test_routes.py'):\n")
        start = old.index(b"    candidate = source / 'deploy/tencent'\n")
        end = old.index(b"    for script in (source / 'deploy/tencent').iterdir():\n", start)
        old = old[:start] + old[end:]
        self.assertEqual(hashlib.sha256(old).hexdigest(), 'f40f082086541ecf459d4e7558dd63440cc09de4c82c68daf65603d764a5eedc')
        installed = self.root / 'deploy/tencent' / name
        installed.write_bytes(old)
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            entry = tarfile.TarInfo('deploy/tencent/' + name); entry.size = len(old)
            archive.addfile(entry, io.BytesIO(old))
        self.save()
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': self.source})); state.chmod(0o600)
        with self.assertRaisesRegex(RuntimeError, 'activation failed'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.assertEqual(installed.read_bytes(), current)
                raise RuntimeError('activation failed')
        self.assertEqual(installed.read_bytes(), old)
        installed.write_bytes(old + b'\n# unreviewed drift')
        with self.assertRaisesRegex(OperationError, 'registered-test ownership protection'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.fail('accepted unreviewed shared operations')
        installed.write_bytes(old)
        with dev.test_operations_transition(self.runtime, self.source):
            self.assertEqual(installed.read_bytes(), current)
        self.assertEqual(installed.read_bytes(), current)

    def test_development_backup_helpers_are_source_bound_and_restored_after_failure(self):
        names = ['tencent/albums_checkpoint.py', 'tencent/albums_artifacts.py', 'tencent/albums_recovery.py', 'albums/backup.py',
                 'albums/deletion_ledger.py', 'albums/asset_deletion_ledger.py', 'albums/collection_deletion_ledger.py']
        for index, name in enumerate(names):
            original = ('# registered snapshot implementation ' + str(index)).encode()
            installed = self.root / 'deploy' / name
            installed.write_bytes(original)
            with tarfile.open(self.folder / 'source.tar', 'a') as archive:
                entry = tarfile.TarInfo('deploy/' + name); entry.size = len(original)
                archive.addfile(entry, io.BytesIO(original))
        self.save()
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': self.source})); state.chmod(0o600)
        with self.assertRaisesRegex(RuntimeError, 'activation failed'):
            with dev.test_operations_transition(self.runtime, self.source):
                for name in names:
                    self.assertEqual((self.root / 'deploy' / name).read_bytes(),
                                     (Path(dev.__file__).resolve().parent.parent / name).read_bytes())
                raise RuntimeError('activation failed')
        for index, name in enumerate(names):
            self.assertEqual((self.root / 'deploy' / name).read_bytes(),
                             ('# registered snapshot implementation ' + str(index)).encode())
        (self.root / 'deploy/albums/backup.py').write_bytes(b'# unexpected drift')
        with self.assertRaisesRegex(OperationError, 'rollback source'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.fail('accepted changed snapshot code')

    def test_first_development_snapshot_install_is_removed_after_failure(self):
        installed = self.root / 'deploy/albums/backup.py'
        original = installed.read_bytes()
        for name in ('backup.py', 'deletion_ledger.py', 'asset_deletion_ledger.py', 'collection_deletion_ledger.py'):
            (installed.parent / name).unlink()
        installed.parent.rmdir()
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            entry = tarfile.TarInfo('deploy/albums/backup.py'); entry.size = len(original)
            archive.addfile(entry, io.BytesIO(original))
        self.save()
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': self.source})); state.chmod(0o600)
        with self.assertRaisesRegex(RuntimeError, 'activation failed'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.assertEqual(installed.read_bytes(), original)
                raise RuntimeError('activation failed')
        self.assertFalse(installed.parent.exists())

    def test_new_collection_checkpoint_helper_rolls_back_and_drift_is_preserved(self):
        installed = self.root / 'deploy/albums/collection_deletion_ledger.py'
        current = installed.read_bytes(); installed.unlink()
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': self.source})); state.chmod(0o600)
        with self.assertRaisesRegex(RuntimeError, 'activation failed'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.assertEqual(installed.read_bytes(), current)
                raise RuntimeError('activation failed')
        self.assertFalse(installed.exists())
        installed.write_bytes(b'# unexpected collection helper drift')
        installed.chmod(0o644)
        with self.assertRaisesRegex(OperationError, 'rollback source'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.fail('unregistered collection helper replaced')
        self.assertEqual(installed.read_bytes(), b'# unexpected collection helper drift')

    def test_new_admin_helper_is_removed_when_first_activation_fails(self):
        for name in ('platform_test_admin.py', 'albums_recovery.py'):
            installed = self.root / 'deploy/tencent' / name
            original = installed.read_bytes()
            installed.unlink()
            state = self.runtime.state / 'platform-test.json'
            state.write_text(json.dumps({'source': self.source})); state.chmod(0o600)
            with self.subTest(name=name), self.assertRaisesRegex(RuntimeError, 'activation failed'):
                with dev.test_operations_transition(self.runtime, self.source):
                    self.assertTrue(installed.is_file())
                    raise RuntimeError('activation failed')
            self.assertFalse(installed.exists())
            installed.write_bytes(original)

    def test_shared_backup_update_does_not_replace_current_production_recovery(self):
        name = 'ops_backup.py'
        current = (Path(__file__).parent / name).read_bytes()
        added = b''.join(b'    "' + table + b'",\n' for table in (
            b'journal_voice_catalog_revisions', b'journal_voice_catalog_current',
            b'journal_voice_verifications', b'journal_voice_catalog_mutations'))
        original = current.replace(added, b'')
        installed = self.root / 'deploy/tencent' / name
        installed.write_bytes(original)
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            entry = tarfile.TarInfo('deploy/tencent/' + name)
            entry.size = len(original)
            archive.addfile(entry, io.BytesIO(original))
        self.save()
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': self.source})); state.chmod(0o600)
        with self.assertRaisesRegex(OperationError, 'outside the reviewed'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.fail('test updates must not replace the production recovery implementation')
        self.assertEqual(installed.read_bytes(), original)
        installed.write_bytes(original + b'# unreviewed shared operation\n')
        with self.assertRaisesRegex(OperationError, 'outside the reviewed'):
            with dev.test_operations_transition(self.runtime, self.source):
                self.fail('accepted an unrelated shared backup change')

    def shared_backup_fixture(self):
        originals, targets, reviewed = {}, {}, {}
        source = Path(dev.__file__).resolve().parent.parent
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            for name in dev.SHARED_BACKUP_UPGRADE:
                original = ('# registered recovery member ' + name + '\n').encode()
                current = (source / name).read_bytes()
                (self.root / 'deploy' / name).write_bytes(original)
                entry = tarfile.TarInfo('deploy/' + name); entry.size = len(original)
                archive.addfile(entry, io.BytesIO(original))
                originals[name], targets[name] = original, current
                reviewed[name] = (hashlib.sha256(original).hexdigest(), hashlib.sha256(current).hexdigest())
        self.save()
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': self.source})); state.chmod(0o600)
        return originals, targets, reviewed

    def test_reviewed_shared_backup_targets_match_current_source(self):
        for name, (_, digest) in dev.SHARED_BACKUP_UPGRADE.items():
            self.assertEqual(hashlib.sha256((Path(dev.__file__).resolve().parent.parent / name).read_bytes()).hexdigest(), digest)

    def test_shared_backup_upgrade_rolls_back_every_member_and_reclaims_failed_write(self):
        originals, targets, reviewed = self.shared_backup_fixture()
        replace = Path.replace; failed = False
        def interrupted(path, destination):
            nonlocal failed
            if Path(destination).name == 'ops_cos.py' and not failed:
                failed = True
                raise OSError('controlled shared member install failure')
            return replace(path, destination)
        with patch.object(dev, 'SHARED_BACKUP_UPGRADE', reviewed):
            with patch.object(Path, 'replace', interrupted), self.assertRaisesRegex(OSError, 'controlled shared member'):
                with dev.test_operations_transition(self.runtime, self.source): self.fail('partial installation admitted')
            for name, data in originals.items(): self.assertEqual((self.root / 'deploy' / name).read_bytes(), data)
            self.assertEqual(list((self.root / 'deploy/tencent').glob('.*.transition')), [])
            for error in (RuntimeError, KeyboardInterrupt):
                with self.assertRaisesRegex(error, 'candidate rejected'):
                    with dev.test_operations_transition(self.runtime, self.source):
                        for name, data in targets.items(): self.assertEqual((self.root / 'deploy' / name).read_bytes(), data)
                        raise error('candidate rejected')
                for name, data in originals.items(): self.assertEqual((self.root / 'deploy' / name).read_bytes(), data)
            with dev.test_operations_transition(self.runtime, self.source): pass
            for name, data in targets.items(): self.assertEqual((self.root / 'deploy' / name).read_bytes(), data)

    def test_shared_backup_upgrade_preserves_replaced_pending_input(self):
        originals, _, reviewed = self.shared_backup_fixture()
        replace = Path.replace
        saved = self.root / 'pending-created-by-producer'
        def substituted(path, destination):
            if Path(destination).name == 'ops_backup.py':
                path.rename(saved)
                path.write_bytes(b'# independent pending code')
                path.chmod(0o600)
                raise OSError('controlled pending replacement')
            return replace(path, destination)
        with patch.object(dev, 'SHARED_BACKUP_UPGRADE', reviewed), patch.object(Path, 'replace', substituted), self.assertRaises((OperationError, FileExistsError)):
            with dev.test_operations_transition(self.runtime, self.source): self.fail('replaced pending code accepted')
        pending = self.root / 'deploy/tencent/.ops_backup.py.transition'
        self.assertEqual(pending.read_bytes(), b'# independent pending code')
        self.assertTrue(saved.is_file())
        for name, data in originals.items(): self.assertEqual((self.root / 'deploy' / name).read_bytes(), data)

    def test_shared_backup_upgrade_rejects_drift_and_unregistered_baseline(self):
        originals, _, reviewed = self.shared_backup_fixture()
        for name, original in originals.items():
            path = self.root / 'deploy' / name; path.write_bytes(original + b'# unreviewed drift')
            with self.subTest(name=name), patch.object(dev, 'SHARED_BACKUP_UPGRADE', reviewed), self.assertRaises(OperationError):
                with dev.test_operations_transition(self.runtime, self.source): self.fail('unreviewed member installed')
            self.assertEqual(path.read_bytes(), original + b'# unreviewed drift')
            path.write_bytes(original)
        wrong = dict(reviewed); name = next(iter(wrong)); wrong[name] = ('0' * 64, wrong[name][1])
        (self.root / 'deploy' / name).write_bytes((Path(dev.__file__).resolve().parent.parent / name).read_bytes())
        with patch.object(dev, 'SHARED_BACKUP_UPGRADE', wrong), self.assertRaisesRegex(OperationError, 'baseline'):
            with dev.test_operations_transition(self.runtime, self.source): self.fail('unregistered rollback accepted')
        for other, data in originals.items():
            if other != name: self.assertEqual((self.root / 'deploy' / other).read_bytes(), data)

    def test_shared_upgrade_uses_the_target_bytes_that_passed_digest_validation(self):
        _, targets, reviewed = self.shared_backup_fixture()
        name = next(iter(targets)); source = Path(dev.__file__).resolve().parent.parent / name
        read = Path.read_bytes; seen = 0
        def changed_after_check(path):
            nonlocal seen
            if path == source:
                seen += 1
                if seen > 1: return b'# target changed after digest validation'
            return read(path)
        with patch.object(dev, 'SHARED_BACKUP_UPGRADE', reviewed), patch.object(Path, 'read_bytes', changed_after_check):
            with dev.test_operations_transition(self.runtime, self.source):
                for member, data in targets.items(): self.assertEqual((self.root / 'deploy' / member).read_bytes(), data)

    def test_shared_upgrade_preserves_installed_drift_after_selection(self):
        _, _, reviewed = self.shared_backup_fixture()
        name = next(iter(reviewed)); installed = self.root / 'deploy' / name
        selection = dev.selection
        def changed_after_check(*args):
            result = selection(*args)
            installed.write_bytes(b'# independent installed code changed after selection')
            return result
        with patch.object(dev, 'SHARED_BACKUP_UPGRADE', reviewed), patch.object(dev, 'selection', side_effect=changed_after_check), self.assertRaisesRegex(OperationError, 'baseline'):
            with dev.test_operations_transition(self.runtime, self.source): self.fail('post-selection drift replaced')
        self.assertEqual(installed.read_bytes(), b'# independent installed code changed after selection')

    def test_killed_shared_upgrade_resumes_exact_members_and_can_restore_registered_baseline(self):
        originals, targets, reviewed = self.shared_backup_fixture()
        code = r"""
import json,os,signal,sys
from pathlib import Path
from types import SimpleNamespace
import platform_test_deploy as dev
root,revision,pairs,stop=sys.argv[1:]
dev.SHARED_BACKUP_UPGRADE=json.loads(pairs)
r=SimpleNamespace(root=Path(root),state=Path(root)/'.operations')
original=Path.replace;count=0
def replace(path,destination):
    global count
    result=original(path,destination)
    if 'tencent/' + Path(destination).name in dev.SHARED_BACKUP_UPGRADE:
        count+=1
        if count==int(stop):os.kill(os.getpid(),signal.SIGKILL)
    return result
Path.replace=replace
with dev.test_operations_transition(r,revision):pass
"""
        for count in range(1, len(reviewed)):
            for name, data in originals.items(): (self.root / 'deploy' / name).write_bytes(data)
            result = subprocess.run([sys.executable, '-c', code, str(self.root), self.source, json.dumps(reviewed), str(count)], capture_output=True, timeout=15)
            self.assertEqual(result.returncode, -signal.SIGKILL, result.stderr)
            self.assertEqual(sum((self.root / 'deploy' / name).read_bytes() == data for name, data in targets.items()), count)
            with patch.object(dev, 'SHARED_BACKUP_UPGRADE', reviewed), self.assertRaisesRegex(RuntimeError, 'candidate rejected'):
                with dev.test_operations_transition(self.runtime, self.source):
                    for name, data in targets.items(): self.assertEqual((self.root / 'deploy' / name).read_bytes(), data)
                    raise RuntimeError('candidate rejected')
            for name, data in originals.items(): self.assertEqual((self.root / 'deploy' / name).read_bytes(), data)
            self.assertEqual(list((self.root / 'deploy/tencent').glob('.*.transition')), [])

    @unittest.skipUnless(os.environ.get('PLATFORM_TEST_REGISTERED_SOURCE_TAR'), 'explicit authenticated running-source fixture required')
    def test_shared_backup_upgrade_matches_registered_running_source_bytes(self):
        originals = {}
        source = Path(dev.__file__).resolve().parent.parent
        collection = self.root / 'deploy/albums/collection_deletion_ledger.py'; collection.unlink()
        with tarfile.open(os.environ['PLATFORM_TEST_REGISTERED_SOURCE_TAR']) as registered, tarfile.open(self.folder / 'source.tar', 'a') as selected:
            for name in dict.fromkeys((*dev.SHARED_BACKUP_UPGRADE, 'tencent/operations.py')):
                member = registered.getmember('deploy/' + name)
                self.assertTrue(member.isfile()); self.assertLessEqual(member.size, 131072)
                data = registered.extractfile(member).read()
                if name in dev.SHARED_BACKUP_UPGRADE:
                    digest, _ = dev.SHARED_BACKUP_UPGRADE[name]
                    self.assertIn(hashlib.sha256(data).hexdigest(), (digest, *dev.SHARED_BACKUP_INTERMEDIATE.get(name, ())))
                originals[name] = data
                (self.root / 'deploy' / name).write_bytes(data)
                entry = tarfile.TarInfo('deploy/' + name); entry.size = len(data)
                selected.addfile(entry, io.BytesIO(data))
        self.save()
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': self.source})); state.chmod(0o600)
        sentinel = self.root / 'preserved-business-input'; sentinel.write_bytes(b'preserved synthetic business data')
        for resumed in (False, True):
            if resumed:
                name = next(iter(originals)); (self.root / 'deploy' / name).write_bytes((source / name).read_bytes())
            with self.assertRaisesRegex(RuntimeError, 'candidate rejected'):
                with dev.test_operations_transition(self.runtime, self.source):
                    for name in originals: self.assertEqual((self.root / 'deploy' / name).read_bytes(), (source / name).read_bytes())
                    self.assertEqual(collection.read_bytes(), (source / 'albums/collection_deletion_ledger.py').read_bytes())
                    with patch.object(dev.recording, 'available', return_value=True):
                        dev.require_installed_operations(self.runtime, self.folder)
                    raise RuntimeError('candidate rejected')
            for name, data in originals.items(): self.assertEqual((self.root / 'deploy' / name).read_bytes(), data)
            self.assertFalse(collection.exists())
        with dev.test_operations_transition(self.runtime, self.source): pass
        for name in originals: self.assertEqual((self.root / 'deploy' / name).read_bytes(), (source / name).read_bytes())
        self.assertTrue(collection.is_file())
        self.assertEqual(sentinel.read_bytes(), b'preserved synthetic business data')
        self.assertEqual(list((self.root / 'deploy/tencent').glob('.*.transition')), [])

    def test_required_operations_include_cos_and_collection_recovery_dependencies(self):
        for name in ('tencent/ops_cos.py', 'albums/collection_deletion_ledger.py'):
            path = self.root / 'deploy' / name; original = path.read_bytes()
            path.write_bytes(original + b'\n# unreviewed dependency')
            with self.subTest(name=name), patch.object(dev.recording, 'available', return_value=True), self.assertRaises(OperationError):
                dev.require_installed_operations(self.runtime, self.folder)
            path.write_bytes(original)

    def test_storekit_configuration_is_sandbox_only_and_app_specific(self):
        app = {'PLATFORM_APPS': 'health,journal,birthday,albums',
               'HEALTH_APP_STORE_ENV': 'Sandbox',
               'HEALTH_APP_STORE_ISSUER_ID': '1f4f7f07-26b1-4226-9518-5bd19f8440b8',
               'HEALTH_APP_STORE_KEY_ID': 'YTX5GYG2Z5',
               'HEALTH_APP_STORE_PRIVATE_KEY_PATH': '/album-secrets/album-app-store.p8',
               'HEALTH_APP_STORE_ROOT_PEM_PATH': '/config/apple-app-store-roots.pem',
               'HEALTH_APP_STORE_APP_APPLE_ID': '6824089036'}
        dev.validate_storekit(app, 'HEALTH')
        historical = {**app, 'HEALTH_APP_STORE_KEY_ID': '4TVBWNCPNR'}
        dev.validate_storekit(historical, 'HEALTH', '0a6a8acd32414df5d55186c4a4f30e42f87f0b5e')
        with self.assertRaises(OperationError):
            dev.validate_storekit(historical, 'HEALTH')
        for key, value in [('HEALTH_APP_STORE_ENV', 'Both'), ('HEALTH_APP_STORE_APP_APPLE_ID', '6804425507'),
                           ('HEALTH_APP_STORE_PRIVATE_KEY_PATH', '/production/key.p8'), ('PLATFORM_APPS', 'health,birthday')]:
            with self.subTest(key=key), self.assertRaises(OperationError):
                dev.validate_storekit({**app, key:value}, 'HEALTH')

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
        if dev.admin.selected(self.folder):
            self.compose['PLATFORM_TEST_ADMIN_ENV_FILE'] = str(self.folder / 'admin.env')
        self.app.pop('BIRTHDAY_DEV_ACTIVATION_SECRET', None)
        self.app['BIRTHDAY_ALLOWED_APP_BUILDS'] = '4'
        self.save()
        self.runtime.backups = self.root / 'backups'
        def execute(label, command, **kwargs):
            if label in ('platform-test-update-running-admin', 'platform-test-update-check-admin', 'platform-test-rollback-check-admin'):
                folder = self.folder if label.startswith('platform-test-update-check') else self.old_folder
                return json.dumps([self.admin_container(folder)]).encode()
            if label.endswith('-worker-status'):
                return b'{"httpEnabled":true,"backgroundEnabled":true}'
            if label in ('platform-test-update-running-gateway', 'platform-test-update-check-gateway',
                         'platform-test-rollback-check-gateway', 'platform-test-update-check-worker',
                         'platform-test-update-running-worker', 'platform-test-rollback-check-worker'):
                source = self.source if label.startswith('platform-test-update-check') else self.old_source
                service = 'worker' if label.endswith('-worker') else 'gateway'
                container = {'State': {'Running': True}, 'RestartCount': 0,
                             'Config': {'Image': dev.PROJECT + '-' + service + ':' + source}}
                folder = self.folder if source == self.source else self.old_folder
                if service == 'gateway' and dev.recording.available(folder):
                    container['Config'].update(User='65532:65532', Env=['APP_ENV=development', 'JOURNAL_RECORDING_ROOT=' + dev.recording.ROOT])
                    container['Mounts'] = [{'Type': 'bind', 'Source': dev.recording.ROOT,
                                            'Destination': dev.recording.ROOT, 'RW': True}]
                if dev.journal.read_aloud_selected(folder):
                    env = dict(e.split('=', 1) for e in container['Config'].get('Env', []))
                    if service == 'gateway':
                        env['APP_ENV'] = 'development'
                        env.update({k: v for k, v in dev.read_environment(folder / 'runtime.env').items() if k in dev.journal.READ_ALOUD_SETTINGS})
                        container['Config']['User'] = '65532:65532'
                        container.setdefault('Mounts', []).append({'Type': 'bind', 'Source': str(dev.journal.PRIVATE_BASE / 'development'),
                                                                  'Destination': '/journal-read-aloud', 'RW': False})
                    else:
                        env.update({k: 'false' if k == 'JOURNAL_TTS_ENABLED' else '' for k in dev.journal.READ_ALOUD_SETTINGS})
                    container['Config']['Env'] = [k + '=' + v for k, v in env.items()]
                return json.dumps([container]).encode()
            if label == 'platform-test-update-schema':
                return b'1\t0001_example.sql\n'
            if label == 'platform-test-migration-names':
                return b'0001_example.sql\n'
            return b''
        self.runtime.execute.side_effect = execute

    def update(self, *, ready_error=None, backup_error=None, albums_backup=False):
        with patch.object(dev.fcntl, 'flock', side_effect=BlockingIOError), patch.object(dev, 'assert_route'), \
                patch.object(dev, 'ready', side_effect=ready_error), \
                patch('ops_backup.create_backup', return_value={'filename': 'verified-checkpoint'}, side_effect=backup_error), \
                patch('ops_backup.verify_backup', return_value={'recovery_platform_test': {'state.json': 'verified'},
                      'recovery_albums_development': {'state.json': 'verified'} if albums_backup else {}}):
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
        self.assertLess(labels.index('platform-test-update-stop-added'), labels.index('platform-test-update-rollback'))
        self.assertIn('platform-test-rollback-check-gateway', labels)

    def test_worker_cleanup_failure_still_restores_old_gateway_and_reports_failure(self):
        self.prepare_update()
        self.enable_health()
        execute = self.runtime.execute.side_effect
        def fail_worker_stop(label, command, **kwargs):
            if label == 'platform-test-update-stop-added':
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

    def enable_admin(self, previous=False):
        values = {'PAYLOAD_ENCRYPTION_KEY': self.app['PAYLOAD_ENCRYPTION_KEY'],
                  'ADMIN_PREVIEW_SIGNING_KEY': base64.b64encode(b's' * 32).decode().rstrip('='),
                  **{key: self.app[key] for key in dev.admin.MODELS}}
        (self.folder / 'admin.env').write_text(''.join(k + '=' + v + '\n' for k, v in values.items()))
        path = Path(__file__).parent / 'testdata/previous-platform-admin-test.yaml' if previous else Path(__file__).resolve().parents[2] / dev.admin.OVERLAY
        body = path.read_bytes()
        (self.folder / dev.admin.OVERLAY).write_bytes(body)
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            entry = tarfile.TarInfo(dev.admin.OVERLAY); entry.size = len(body)
            archive.addfile(entry, io.BytesIO(body))
        self.compose['PLATFORM_TEST_ADMIN_ENV_FILE'] = str(self.folder / 'admin.env')
        self.save()

    def admin_container(self, folder):
        compose = dev.read_environment(folder / 'compose.env')
        env = {**dev.admin.settings(folder), **dev.read_environment(folder / 'admin.env'),
               'DATABASE_DSN': 'platform_test:' + compose['PLATFORM_TEST_MYSQL_PASSWORD'] + '@tcp(mysql:3306)/platform_test?charset=utf8mb4&collation=utf8mb4_0900_ai_ci&parseTime=true',
               'REDIS_URL': 'redis://:' + compose['PLATFORM_TEST_REDIS_PASSWORD'] + '@redis:6379/1'}
        return {'State': {'Running': True}, 'RestartCount': 0,
                'Config': {'Image': dev.PROJECT + '-admin:' + folder.name, 'User': '65532:65532',
                           'Env': [k + '=' + v for k, v in env.items()]},
                'Mounts': [{'Source': str(dev.journal.PRIVATE_BASE / 'development/api-key.txt'),
                            'Destination': dev.admin.KEY_FILE, 'Type': 'bind', 'RW': False}],
                'HostConfig': {'ReadonlyRootfs': True, 'PortBindings': {'8082/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '18792'}]}},
                'NetworkSettings': {'Networks': {dev.PROJECT + '_platform-test': {}}}}

    def test_unified_admin_is_not_checked_as_an_albums_worker(self):
        self.enable_read_aloud(); self.enable_admin(); self.prepare_update()
        with patch.object(dev.albums, 'selected', return_value=True), patch.object(dev.albums, 'verify_gateway'):
            dev.verify_running(self.runtime, self.folder, self.source, 'platform-test-update-check')
            execute = self.runtime.execute.side_effect
            def with_worker_secret(label, command, **kwargs):
                body = execute(label, command, **kwargs)
                if label == 'platform-test-update-check-worker':
                    container = json.loads(body)[0]
                    container['Config'].setdefault('Env', []).append('ALBUM_APP_STORE_CONNECT_ENABLED=false')
                    return json.dumps([container]).encode()
                return body
            self.runtime.execute.side_effect = with_worker_secret
            with self.assertRaisesRegex(OperationError, 'AI worker received Albums'):
                dev.verify_running(self.runtime, self.folder, self.source, 'platform-test-update-check')

    def test_admin_update_uses_selected_private_inputs_and_keeps_existing_keys(self):
        self.enable_journal(); self.enable_read_aloud(); self.prepare_update(); self.enable_admin()
        result = self.update()
        self.assertEqual(dev.running_services(self.folder), ('gateway', 'worker', 'admin'))
        self.assertIn('admin.env', result['files'])
        self.assertIn(str(self.folder / dev.admin.OVERLAY), dev.compose(self.folder, 'config', '--quiet'))
        self.assertEqual(result['previous']['source'], self.old_source)
        next_folder = self.folder.parent / ('c' * 40)
        shutil.copytree(self.folder, next_folder)
        old = dev.read_environment(next_folder / 'admin.env')
        old['ADMIN_PREVIEW_SIGNING_KEY'] = base64.b64encode(b'n' * 32).decode().rstrip('=')
        (next_folder / 'admin.env').write_text(''.join(k + '=' + v + '\n' for k, v in old.items()))
        with self.assertRaisesRegex(OperationError, 'preserve its active private'):
            dev.admin.require_transition(self.folder, next_folder)

    def test_journal_admin_migrates_to_unified_recipe_with_unchanged_private_keys(self):
        self.enable_journal(); self.enable_read_aloud(); self.enable_admin(previous=True)
        self.prepare_update(); self.enable_admin()
        self.assertEqual((self.old_folder/'admin.env').read_bytes(), (self.folder/'admin.env').read_bytes())
        result = self.update()
        self.assertEqual(result['previous']['source'], self.old_source)
        self.assertEqual(dev.admin.settings(self.old_folder)['ADMIN_APPS'], 'journal')
        self.assertEqual(dev.admin.settings(self.folder)['ADMIN_APPS'], 'health,journal,birthday,albums')
        with self.assertRaisesRegex(OperationError, 'narrow'):
            dev.admin.require_transition(self.folder, self.old_folder)

    def test_unified_admin_activation_failure_restores_the_matched_journal_admin(self):
        self.enable_journal(); self.enable_read_aloud(); self.enable_admin(previous=True)
        self.prepare_update(); self.enable_admin()
        with self.assertRaises(OperationError):
            self.update(ready_error=[OperationError('new admin unavailable'), None])
        self.assertEqual(json.loads((self.runtime.state/'platform-test.json').read_text()), self.previous)
        rollback = next(c.args[1] for c in self.runtime.execute.call_args_list if c.args[0]=='platform-test-update-rollback')
        self.assertEqual(rollback[-3:], ['gateway','worker','admin'])
        self.assertEqual(dev.admin.settings(self.old_folder)['ADMIN_RP_ID'], 'api-test.journal.tellyouwhat.cn')

    def test_first_admin_failure_stops_added_service_and_restores_prior_gateway(self):
        self.enable_journal(); self.enable_read_aloud(); self.prepare_update(); self.enable_admin()
        with self.assertRaises(OperationError):
            self.update(ready_error=[OperationError('admin did not become ready'), None])
        calls = self.runtime.execute.call_args_list
        stop = next(c.args[1] for c in calls if c.args[0] == 'platform-test-update-stop-added')
        rollback = next(c.args[1] for c in calls if c.args[0] == 'platform-test-update-rollback')
        self.assertEqual(stop[-1], 'admin')
        self.assertEqual(rollback[-2:], ['gateway', 'worker'])
        self.assertEqual(json.loads((self.runtime.state / 'platform-test.json').read_text()), self.previous)

    def test_admin_rejects_unrelated_secrets_and_unreviewed_overlay(self):
        self.enable_journal(); self.enable_read_aloud(); self.enable_admin()
        dev.selection(self.runtime, self.source)
        with (self.folder / 'admin.env').open('a') as output: output.write('HEALTH_ARK_API_KEY=unexpected\n')
        self.save()
        with self.assertRaisesRegex(OperationError, 'unrelated service credentials'):
            dev.selection(self.runtime, self.source)
        self.enable_admin()
        (self.folder / dev.admin.OVERLAY).write_text('services: {admin: {privileged: true}}'); self.save()
        with self.assertRaisesRegex(OperationError, 'reviewed source'):
            dev.selection(self.runtime, self.source)

    def test_admin_requires_the_existing_journal_models_before_deployment(self):
        self.enable_journal(); self.enable_read_aloud(); self.enable_admin()
        values = dev.read_environment(self.folder / 'admin.env')
        for key in dev.admin.MODELS:
            missing = {k: v for k, v in values.items() if k != key}
            with self.assertRaises(OperationError): dev.admin.validate_config(missing, self.app)
            with self.assertRaisesRegex(OperationError, 'model identifiers'):
                dev.admin.validate_config({**values, key: 'ep-unselected-project'}, self.app)

    def test_admin_process_rejects_public_ports_production_credentials_and_simulator_mount(self):
        self.enable_journal(); self.enable_read_aloud(); self.enable_admin()
        original = self.admin_container(self.folder)
        dev.admin.verify(original, self.folder)
        mutations = [lambda c: c['HostConfig']['PortBindings']['8082/tcp'][0].update(HostIp='0.0.0.0'),
                     lambda c: c['Mounts'][0].update(RW=True),
                     lambda c: c['Mounts'][0].update(Source='/production/key.txt'),
                     lambda c: c['Mounts'].append({'Destination': '/journal-read-aloud'}),
                     lambda c: c['Config']['Env'].append('HEALTH_ARK_API_KEY=production'),
                     lambda c: c['Config']['Env'].append('ALBUM_TOS_SECRET_KEY=production'),
                     lambda c: c['Config']['Env'].append('JOURNAL_READ_ALOUD_SIMULATOR_CREDENTIALS_FILE=/private.json'),
                     lambda c: c['Config']['Env'].append('ADMIN_ORIGIN=https://admin.tellyouwhat.cn'),
                     lambda c: c['Config']['Env'].append('ADMIN_WRITES_ENABLED=false'),
                     lambda c: c['Config']['Env'].append('DATABASE_DSN=production'),
                     lambda c: c['NetworkSettings']['Networks'].update(production={})]
        for mutate in mutations:
            candidate = copy.deepcopy(original); mutate(candidate)
            with self.assertRaises(OperationError): dev.admin.verify(candidate, self.folder)

    def test_read_aloud_update_selects_private_mount_and_preserves_rollback(self):
        self.enable_journal()
        self.prepare_update()
        self.enable_read_aloud()
        result = self.update()
        self.assertEqual(result['previous']['source'], self.old_source)
        self.assertIn(dev.journal.READ_ALOUD_OVERLAY, result['files'])
        commands = self.runtime.execute.call_args_list
        labels = [c.args[0] for c in commands]
        self.assertLess(labels.index('journal-read-aloud-private-prepare'), labels.index('platform-test-update-start'))
        prepare = next(c.args[1] for c in commands if c.args[0] == 'journal-read-aloud-private-prepare')
        self.assertEqual(prepare[:3], ['sudo', '-n', 'python3'])
        self.assertEqual(prepare[-1], str(self.folder / 'runtime.env'))
        self.assertIn(str(self.folder / dev.journal.READ_ALOUD_OVERLAY), dev.compose(self.folder, 'config', '--quiet'))
        self.assertNotIn(str(self.old_folder / dev.journal.READ_ALOUD_OVERLAY), dev.compose(self.old_folder, 'config', '--quiet'))

    def test_read_aloud_configuration_rejects_arbitrary_files_and_excessive_allowances(self):
        self.enable_read_aloud()
        for key, value in [('JOURNAL_TTS_API_KEY_FILE', '/album-secrets/private-key'),
                           ('JOURNAL_READ_ALOUD_SIMULATOR_CREDENTIALS_FILE', '/tmp/identities.json'),
                           ('JOURNAL_TTS_ENABLED', 'yes'), ('JOURNAL_TTS_MONTHLY_CHARACTERS', '100001'),
                           ('JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS', '10001')]:
            before = dict(self.app)
            self.app[key] = value
            self.save()
            with self.subTest(key=key), self.assertRaises(OperationError):
                dev.selection(self.runtime, self.source)
            self.app = before
        self.save()
        overlay = self.folder / dev.journal.READ_ALOUD_OVERLAY
        overlay.write_bytes(overlay.read_bytes().replace(b'read_only: true', b'read_only: false'))
        self.save()
        with self.assertRaisesRegex(OperationError, 'reviewed source'):
            dev.selection(self.runtime, self.source)

    def test_read_aloud_mount_never_reaches_worker_or_an_unselected_gateway(self):
        self.enable_read_aloud()
        mount = {'Type': 'bind', 'Source': str(dev.journal.PRIVATE_BASE / 'development'),
                 'Destination': '/journal-read-aloud', 'RW': False}
        env = ['APP_ENV=development'] + [k + '=' + v for k, v in self.app.items() if k in dev.journal.READ_ALOUD_SETTINGS]
        container = {'Config': {'User': '65532:65532', 'Env': env}, 'Mounts': [mount]}
        dev.journal.verify_read_aloud(container, self.folder, 'gateway')
        with self.assertRaises(OperationError):
            dev.journal.verify_read_aloud(container, self.folder, 'worker')
        mount['RW'] = True
        with self.assertRaises(OperationError):
            dev.journal.verify_read_aloud(container, self.folder, 'gateway')
        container['Mounts'] = []
        with self.assertRaises(OperationError):
            dev.journal.verify_read_aloud(container, self.folder, 'gateway')
        for k in dev.journal.READ_ALOUD_SETTINGS:
            self.app.pop(k)
        self.save()
        with self.assertRaises(OperationError):
            dev.journal.verify_read_aloud(container, self.folder, 'gateway')

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

    def prepare_migration(self, names=('0065_journal_illustration_jobs.sql',)):
        self.prepare_update()
        for name in names:
            self.add_illustration_migration(name=name)
        execute = self.runtime.execute.side_effect
        self.migrated = False
        def migrate(label, command, **kwargs):
            if label == 'platform-test-update-migrate':
                self.migrated = True
            if label == 'platform-test-migration-names':
                return ('0001_example.sql\n' + (''.join(name + '\n' for name in names) if self.migrated else '')).encode()
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

    def test_reviewed_illustration_consent_upgrade_is_bound_to_exact_sql(self):
        from platform_test_schema import plan
        self.prepare_update()
        self.add_illustration_migration(name='0067_journal_illustration_consent_scope.sql')
        before, after = plan(self.old_folder, self.folder)
        self.assertEqual(before, ['0001_example.sql'])
        self.assertEqual(after, ['0001_example.sql', '0067_journal_illustration_consent_scope.sql'])
        self.write_archive()
        self.add_illustration_migration(b'ALTER TABLE privacy_consents DROP CHECK privacy_consents_scope_chk;',
                                       name='0067_journal_illustration_consent_scope.sql')
        with self.assertRaisesRegex(OperationError, 'unreviewed database schemas'):
            plan(self.old_folder, self.folder)

    def test_voice_recording_upgrade_requires_exact_reviewed_sql(self):
        from platform_test_schema import plan
        self.prepare_update()
        self.add_illustration_migration(name='0085_prompt_voice_debug_samples.sql')
        shutil.copy2(self.folder / 'source.tar', self.old_folder / 'source.tar')
        name = '0086_prompt_recording_inputs.sql'
        self.add_illustration_migration(name=name)
        before, after = plan(self.old_folder, self.folder)
        self.assertEqual(after, before + [name])
        self.write_archive()
        self.add_illustration_migration(name='0085_prompt_voice_debug_samples.sql')
        self.add_illustration_migration(b'DELETE FROM prompt_eval_samples;', name=name)
        with self.assertRaisesRegex(OperationError, 'unreviewed database schemas'):
            plan(self.old_folder, self.folder)

    def test_album_attestation_registry_upgrade_requires_exact_reviewed_sql(self):
        from platform_test_schema import plan
        self.prepare_update()
        name = '0079_platform_albums_app_attest.sql'
        self.add_illustration_migration(name=name)
        self.assertEqual(plan(self.old_folder, self.folder)[1], ['0001_example.sql', name])
        self.write_archive()
        self.add_illustration_migration(b"DELETE FROM apps WHERE app_id='health';", name=name)
        with self.assertRaisesRegex(OperationError, 'unreviewed database schemas'):
            plan(self.old_folder, self.folder)

    def test_main_consent_history_insertion_is_exact_and_does_not_admit_other_backfills(self):
        from platform_test_schema import plan
        self.prepare_update()
        last = '0078_album_asset_deletion_ledger.sql'
        name = '0066_privacy_consent_scopes.sql'
        self.add_illustration_migration(name=last)
        shutil.copyfile(self.folder / 'source.tar', self.old_folder / 'source.tar')
        self.add_illustration_migration(name=name)
        before, after = plan(self.old_folder, self.folder)
        self.assertEqual(before, ['0001_example.sql', last])
        self.assertEqual(after, ['0001_example.sql', name, last])
        for backfill, contents in ((name, b'ALTER TABLE privacy_consents DROP CHECK privacy_consents_scope_chk;'),
                                   ('0072_health_guardian_consent_scope.sql', None)):
            with self.subTest(backfill=backfill):
                shutil.copyfile(self.old_folder / 'source.tar', self.folder / 'source.tar')
                self.add_illustration_migration(contents, name=backfill)
                with self.assertRaisesRegex(OperationError, 'unreviewed database schemas'):
                    plan(self.old_folder, self.folder)

    def test_guardian_consent_migration_requires_exact_reviewed_sql(self):
        from platform_test_schema import plan
        self.prepare_update()
        name = '0072_health_guardian_consent_scope.sql'
        self.add_illustration_migration(name=name)
        self.assertEqual(plan(self.old_folder, self.folder)[1], ['0001_example.sql', name])
        self.write_archive()
        self.add_illustration_migration(b'ALTER TABLE privacy_consents DROP CHECK privacy_consents_scope_chk;', name=name)
        with self.assertRaisesRegex(OperationError, 'unreviewed database schemas'):
            plan(self.old_folder, self.folder)

    def test_read_aloud_migrations_require_each_exact_reviewed_sql(self):
        from platform_test_schema import plan
        self.prepare_update()
        for name in READ_ALOUD_MIGRATIONS:
            with self.subTest(name=name):
                self.write_archive()
                self.add_illustration_migration(name=name)
                self.assertEqual(plan(self.old_folder, self.folder)[1], ['0001_example.sql', name])
                self.write_archive()
                self.add_illustration_migration(b'CREATE TABLE unreviewed (id INT);', name=name)
                with self.assertRaisesRegex(OperationError, 'unreviewed database schemas'):
                    plan(self.old_folder, self.folder)

    def test_album_metadata_migrations_require_each_exact_reviewed_sql(self):
        from platform_test_schema import plan
        from platform_test_albums import SCHEMA_MIGRATIONS
        self.prepare_update()
        for path in SCHEMA_MIGRATIONS[2:]:
            name = Path(path).name
            with self.subTest(name=name):
                self.write_archive()
                self.add_illustration_migration(name=name)
                self.assertEqual(plan(self.old_folder, self.folder)[1], ['0001_example.sql', name])
                self.write_archive()
                self.add_illustration_migration(b'CREATE TABLE unreviewed (id INT);', name=name)
                with self.assertRaisesRegex(OperationError, 'unreviewed database schemas'):
                    plan(self.old_folder, self.folder)

    def test_read_aloud_migrations_finish_before_candidate_activation(self):
        self.enable_journal()
        self.prepare_migration(READ_ALOUD_MIGRATIONS)
        result = self.update()
        self.assertEqual(result['migrations'], 5)
        self.assertEqual(result['latest_migration'], READ_ALOUD_MIGRATIONS[-1])
        self.assertEqual(result['previous']['migrations'], 5)
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        self.assertLess(labels.index('platform-test-update-migrate'), labels.index('platform-test-update-start'))

    def test_read_aloud_failed_candidate_retains_schema_and_recovers_previous_selection(self):
        self.enable_journal()
        self.prepare_migration(READ_ALOUD_MIGRATIONS)
        with self.assertRaisesRegex(OperationError, 'not ready'):
            self.update(ready_error=[OperationError('not ready'), None])
        retained = json.loads((self.runtime.state / 'platform-test.json').read_text())
        self.assertEqual(retained['source'], self.old_source)
        self.assertEqual(retained['migrations'], 5)
        self.assertEqual(retained['schema_source'], self.source)
        rollback = next(c.args[1] for c in self.runtime.execute.call_args_list if c.args[0] == 'platform-test-update-rollback')
        self.assertIn(str(self.old_folder / 'compose.env'), rollback)
        self.assertFalse(any('down' in c.args[1] or '--volumes' in c.args[1] for c in self.runtime.execute.call_args_list))

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

    def test_reviewed_storage_transition_prepares_directory_and_builds_maintenance(self):
        self.set_topology(mounted=False)
        self.prepare_update()
        self.enable_journal()
        self.set_topology(mounted=True)
        result = self.update()
        self.assertEqual(result['apps'], ['health', 'journal', 'birthday'])
        self.assertEqual(dev.image_services(self.folder), ('gateway', 'worker', 'migrate', 'maintenance'))
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        self.assertLess(labels.index('recording-storage-prepare'), labels.index('platform-test-update-start'))
        builds = [c.args[1] for c in self.runtime.execute.call_args_list if c.args[0].startswith('platform-test-build-')]
        self.assertEqual({c[c.index('--target') + 1] for c in builds}, {'gateway', 'worker', 'migrate', 'maintenance'})
        self.assertFalse(any('down' in c.args[1] or '--volumes' in c.args[1] for c in self.runtime.execute.call_args_list))

    def test_recordings_enable_only_after_mounted_journal_baseline(self):
        self.set_topology(mounted=False)
        self.prepare_update()
        self.enable_journal()
        self.set_topology(mounted=True)
        self.app['JOURNAL_RECORDING_ENABLED'] = 'true'
        self.save()
        with self.assertRaisesRegex(OperationError, 'disabled before enabling'):
            self.update()
        self.assertFalse(any(c.args[0] == 'recording-storage-prepare' for c in self.runtime.execute.call_args_list))

    def test_mounted_journal_can_enable_recording_generation(self):
        self.enable_journal()
        self.set_topology(mounted=True)
        self.prepare_update()
        self.app['JOURNAL_RECORDING_ENABLED'] = 'true'
        self.save()
        self.update()
        self.assertEqual(dev.read_environment(self.folder / 'runtime.env')['JOURNAL_RECORDING_ENABLED'], 'true')

    def test_disabling_recordings_keeps_storage_and_maintenance(self):
        self.enable_journal()
        self.app['JOURNAL_RECORDING_ENABLED'] = 'true'
        self.set_topology(mounted=True)
        self.prepare_update()
        self.app['JOURNAL_RECORDING_ENABLED'] = 'false'
        self.save()
        self.update()
        self.assertTrue(dev.recording.available(self.folder))
        self.assertIn('maintenance', dev.image_services(self.folder))

    def test_unreviewed_topology_and_removing_storage_are_rejected(self):
        self.enable_journal()
        self.set_topology(mounted=True)
        self.prepare_update()
        self.set_topology(mounted=False)
        with self.assertRaisesRegex(OperationError, 'topology'):
            self.update()
        self.assertFalse(any('build' in c.args[1] for c in self.runtime.execute.call_args_list))

    def test_new_maintenance_topology_requires_test_media_cleanup_credentials(self):
        self.set_topology(mounted=True)
        with self.assertRaisesRegex(OperationError, 'Health media configuration'):
            dev.selection(self.runtime, self.source)

    def enable_albums(self):
        self.enable_journal()
        self.set_topology(mounted=True)
        overlay = Path(__file__).resolve().parents[2] / dev.albums.OVERLAY
        (self.folder / dev.albums.OVERLAY).write_bytes(overlay.read_bytes())
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            item = tarfile.TarInfo(dev.albums.OVERLAY)
            data = overlay.read_bytes(); item.size = len(data)
            archive.addfile(item, io.BytesIO(data))
        values = {**dev.albums.REGISTRY, 'ALBUM_COS_BUCKET': 'tellyouwhat-albums-1251995834',
                  'ALBUM_COS_REGION': 'ap-guangzhou', 'ALBUM_APPLE_SIGN_IN_KEY_ID': '9H4R3FJ9D8',
                  'ALBUM_APPLE_SIGN_IN_KEY_FILE': '/album-secrets/apple-sign-in.p8',
                  'ALBUM_CREDENTIAL_KEY_FILE': '/album-secrets/credential-key.base64',
                  'ALBUM_LOGIN_LIMIT_KEY_FILE': '/album-secrets/login-limit-key.base64'}
        (self.folder / 'albums.env').write_text(''.join(k + '=' + v + '\n' for k, v in values.items()))
        self.app.update(dev.albums.REGISTRY)
        self.app['PLATFORM_APPS'] = 'health,journal,birthday,albums'
        self.compose.update(PLATFORM_ALBUM_SOURCE='c' * 40, PLATFORM_ALBUM_ENV_FILE=str(self.folder / 'albums.env'),
                            PLATFORM_ALBUM_MYSQL_PASSWORD='4' * 64, PLATFORM_ALBUM_REDIS_PASSWORD='5' * 64)
        self.save()
        approved = self.root / '.albums-development/releases' / ('c' * 40)
        approved.mkdir(mode=0o700, parents=True)
        for p in (approved.parent, approved.parent.parent):
            p.chmod(0o700)
        for name in dev.albums.ALBUM_FILES:
            (approved / name).write_bytes(b'synthetic selected source')
            (approved / name).chmod(0o600)
        with tarfile.open(approved / 'source.tar', 'w') as archive:
            item = tarfile.TarInfo('migrations/0001_example.sql'); item.size = 1
            archive.addfile(item, io.BytesIO(b'x'))
        (approved / 'runtime.env').write_bytes((self.folder / 'albums.env').read_bytes())
        (approved / 'compose.env').write_text('ALBUM_TEST_SECRET_DIRECTORY=/opt/tellyouwhat/albums-development/secrets\n'
            'ALBUM_TEST_MYSQL_USER=albums_development_test\nALBUM_TEST_NETWORK_CIDR=172.29.92.0/24\n'
            'ALBUM_TEST_MYSQL_PASSWORD=' + '4' * 64 + '\nALBUM_TEST_REDIS_PASSWORD=' + '5' * 64 + '\n')
        (approved / 'selection.json').write_text(json.dumps({'source': 'c' * 40, 'files': {
            n: hashlib.sha256((approved / n).read_bytes()).hexdigest() for n in dev.albums.ALBUM_FILES if n != 'selection.json'}}))
        for path, value in [(self.runtime.state / 'albums-development.json', {'source': 'c' * 40, 'release': str(approved)}),
                            (self.runtime.state / 'artifact-owners/albums-test.json', {'current': {'source': 'c' * 40, 'paths': [str(approved)]}})]:
            path.parent.mkdir(exist_ok=True)
            path.write_text(json.dumps(value)); path.chmod(0o600)
        execute = self.runtime.execute.side_effect or self.execute
        def with_albums(label, command, **kwargs):
            if label == 'platform-test-album-schema':
                return b'0001_example.sql\n'
            if label.startswith('platform-test-albums-'):
                return json.dumps([{'State': {'Running': True},
                    'NetworkSettings': {'Networks': {dev.albums.NETWORK: {}}},
                    'Config': {'Labels': {'com.docker.compose.project': dev.albums.PROJECT},
                               'Image': 'tellyouwhat-albums-test-development-albumworker:' + 'c' * 40,
                               'Env': ['MYSQL_DATABASE=albums_test', 'MYSQL_USER=albums_development_test',
                                       'MYSQL_PASSWORD=' + '4' * 64, 'REDIS_PASSWORD=' + '5' * 64]}}]).encode()
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = with_albums

    def test_albums_admission_requires_registered_resources_and_complete_backup_before_build(self):
        self.set_topology(mounted=False)
        self.prepare_update()
        self.enable_albums()
        dev.selection(self.runtime, self.source)
        dev.albums.require_resources(self.runtime, self.folder)
        with patch.object(dev.albums, 'verify_gateway'):
            with self.assertRaisesRegex(OperationError, 'full authenticated development backup'):
                self.update()
        self.assertFalse(any(c.args[0].startswith('platform-test-build') for c in self.runtime.execute.call_args_list))
        self.compose['PLATFORM_ALBUM_MYSQL_PASSWORD'] = '6' * 64
        self.save()
        with self.assertRaisesRegex(OperationError, 'existing store'):
            self.update()
        self.compose['PLATFORM_ALBUM_MYSQL_PASSWORD'] = '4' * 64
        self.save()
        with (self.folder / 'albums.env').open('a') as f:
            f.write('ALBUM_SHARING_ENABLED=true\n')
        self.save()
        with self.assertRaisesRegex(OperationError, 'test recipient site, Sandbox and dedicated mounted keys'):
            self.update()

    def test_albums_credentials_cannot_escape_gateway_or_use_production_identity(self):
        self.enable_albums()
        for target, key, bad in [(self.app, 'ALBUM_COS_SECRET_KEY', 'private'),
                                 (self.compose, 'PLATFORM_ALBUM_MYSQL_PASSWORD', '1' * 64),
                                 (self.app, 'ALBUM_APPLE_BUNDLE_ID', 'cn.tellyouwhat.albums')]:
            original = target.get(key)
            target[key] = bad
            self.save()
            with self.subTest(key=key), self.assertRaises(OperationError):
                dev.selection(self.runtime, self.source)
            if original is None:
                target.pop(key)
            else:
                target[key] = original
        self.save()
        with (self.folder / 'albums.env').open('a') as f:
            f.write('DATABASE_DSN=wrong-database\n')
        self.save()
        with self.assertRaises(OperationError):
            dev.selection(self.runtime, self.source)

    def test_all_four_hosts_are_checked_using_their_actual_readiness_contract(self):
        self.enable_albums()
        seen = []
        def respond(request, **kwargs):
            host = request.get_header('Host'); seen.append(host)
            result = io.BytesIO(b'{"code":"ok"}' if host == dev.albums.HOST else b'{"status":"ready"}')
            result.status = 200
            return result
        opener = Mock(); opener.open.side_effect = respond
        with patch.object(dev.urllib.request, 'build_opener', return_value=opener):
            dev.ready(self.folder)
        self.assertEqual(seen, [dev.HOST, dev.HEALTH_HOST, dev.journal.HOST, dev.albums.HOST])
        self.assertIn(str(self.folder / dev.albums.OVERLAY), dev.compose(self.folder, 'config', '--quiet'))

    def test_albums_admission_reuses_data_services_and_registers_shared_gateway(self):
        self.enable_journal()
        self.set_topology(mounted=True)
        self.prepare_update()
        self.enable_albums()
        with patch.object(dev.albums, 'verify_gateway') as verify:
            result = self.update(albums_backup=True)
        verify.assert_called_once()
        self.assertEqual(result['apps'], ['health', 'journal', 'birthday', 'albums'])
        self.assertEqual(result['previous']['source'], self.old_source)
        commands = [c.args[1] for c in self.runtime.execute.call_args_list]
        starts = [c for c in commands if 'up' in c]
        self.assertEqual(len(starts), 1)
        self.assertEqual(starts[0][-2:], ['gateway', 'worker'])
        self.assertIn(str(self.folder / dev.albums.OVERLAY), starts[0])
        self.assertFalse(any('down' in c or '--volumes' in c or 'albumworker' in c or 'albumgateway' in c for c in commands))

    def test_failed_albums_admission_restores_previous_gateway_without_touching_album_data(self):
        self.enable_journal()
        self.set_topology(mounted=True)
        self.prepare_update()
        self.enable_albums()
        with self.assertRaisesRegex(OperationError, 'not ready'):
            self.update(albums_backup=True, ready_error=[OperationError('not ready'), None])
        self.assertEqual(json.loads((self.runtime.state / 'platform-test.json').read_text()), self.previous)
        rollback = next(c.args[1] for c in self.runtime.execute.call_args_list if c.args[0] == 'platform-test-update-rollback')
        self.assertIn(str(self.old_folder / 'compose.env'), rollback)
        self.assertNotIn(str(self.folder / dev.albums.OVERLAY), rollback)

    def prepare_album_product_migration(self):
        name = '0073_album_development_subscription_products.sql'
        self.enable_journal()
        self.set_topology(mounted=True)
        self.prepare_migration((name,))
        self.enable_albums()
        self.album_migrated = False
        execute = self.runtime.execute.side_effect
        def migrate(label, command, **kwargs):
            if label == 'platform-test-update-album-migrate':
                self.album_migrated = True
            if label == 'platform-test-album-schema':
                return ('0001_example.sql\n' + (name + '\n' if self.album_migrated else '')).encode()
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = migrate

    def test_album_product_migration_is_backed_up_scoped_and_retained_on_rollback(self):
        self.prepare_album_product_migration()
        with patch.object(dev.albums, 'verify_gateway'):
            with self.assertRaisesRegex(OperationError, 'not ready'):
                self.update(albums_backup=True, ready_error=[OperationError('not ready'), None])
        state = json.loads((self.runtime.state / 'platform-test.json').read_text())
        self.assertEqual(state['source'], self.old_source)
        self.assertEqual(state['album_migrations'], 2)
        self.assertEqual(state['album_latest_migration'], '0073_album_development_subscription_products.sql')
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        self.assertLess(labels.index('platform-test-update-migrate'), labels.index('platform-test-update-album-migrate'))
        self.assertLess(labels.index('platform-test-update-album-migrate'), labels.index('platform-test-update-start'))
        job = next(c.args[1] for c in self.runtime.execute.call_args_list if c.args[0] == 'platform-test-update-album-migrate')
        self.assertEqual(job[-4:], ['run', '--rm', '--no-deps', 'album-migrate'])
        execute = self.runtime.execute.side_effect
        def retry(label, command, **kwargs):
            if label == 'platform-test-update-schema':
                return b'2\t0073_album_development_subscription_products.sql\n'
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = retry
        self.runtime.execute.reset_mock()
        with patch.object(dev.albums, 'verify_gateway'):
            self.update(albums_backup=True)
        self.assertFalse(any(c.args[0] in ('platform-test-update-migrate', 'platform-test-update-album-migrate')
                             for c in self.runtime.execute.call_args_list))

    def enable_album_media(self):
        self.prepare_update()
        self.enable_albums()
        with (self.folder / 'albums.env').open('a') as output:
            values = {'ALBUM_COS_ARCHIVE_SECRET_ID': 'test-archive', 'ALBUM_COS_ARCHIVE_SECRET_KEY': 'test-archive-key',
                      'ALBUM_COS_UPLOAD_SECRET_ID': 'test-upload', 'ALBUM_COS_UPLOAD_SECRET_KEY': 'test-upload-key',
                      'ALBUM_MAX_ORIGINAL_BYTES': '1000000000', 'ALBUM_MAX_DERIVED_BYTES': '100000000',
                      'ALBUM_CI_VIDEO_QUEUE_ID': dev.albums.WORKER_VALUES['ALBUM_CI_VIDEO_QUEUE_ID']}
            output.write(''.join(k + '=' + v + '\n' for k,v in values.items()))
        # Existing admission credentials are pinned by the original resource selection.
        original = self.root / '.albums-development/releases' / ('c' * 40)
        (original / 'runtime.env').write_bytes((self.folder / 'albums.env').read_bytes())
        record = json.loads((original / 'selection.json').read_text())
        record['files']['runtime.env'] = hashlib.sha256((original / 'runtime.env').read_bytes()).hexdigest()
        (original / 'selection.json').write_text(json.dumps(record))
        self.save()
        for name in dev.selection_files(self.folder):
            shutil.copy2(self.folder / name, self.old_folder / name)
        old_compose = {**self.compose, 'PLATFORM_TEST_IMAGE_TAG': self.old_source,
            'PLATFORM_TEST_ENV_FILE': str(self.old_folder / 'runtime.env'),
            'PLATFORM_ALBUM_ENV_FILE': str(self.old_folder / 'albums.env')}
        (self.old_folder / 'compose.env').write_text(''.join(k + '=' + v + '\n' for k,v in old_compose.items()))
        (self.old_folder / 'selection.json').write_text(json.dumps({'source': self.old_source, 'files': {
            name: hashlib.sha256((self.old_folder / name).read_bytes()).hexdigest() for name in dev.selection_files(self.old_folder)}}))
        recipe = (Path(__file__).resolve().parents[2] / dev.albums.WORKER_COMPOSE).read_bytes()
        (self.folder / dev.albums.WORKER_COMPOSE).write_bytes(recipe)
        with tarfile.open(self.folder / 'source.tar', 'a') as archive:
            item = tarfile.TarInfo(dev.albums.WORKER_COMPOSE); item.size = len(recipe)
            archive.addfile(item, io.BytesIO(recipe))
        settings = dev.albums.worker_values(dev.read_environment(self.folder / 'albums.env'))
        (self.folder / 'album-worker.env').write_text(''.join(k + '=' + v + '\n' for k,v in settings.items()))
        self.compose['PLATFORM_ALBUM_WORKER_ENV_FILE'] = str(self.folder / 'album-worker.env')
        self.save()
        execute = self.runtime.execute.side_effect
        self.runtime.execute.side_effect = lambda label, command, **kw: (b'[{"State":{"Running":false,"ExitCode":0}}]'
            if label == 'platform-test-album-worker-stopped' else execute(label, command, **kw))

    def prepare_photo_writer_migration(self):
        self.enable_album_media()
        # The running worker already uses the shared, scoped media recipe.
        for name in (dev.albums.WORKER_COMPOSE, 'album-worker.env'):
            shutil.copyfile(self.folder / name, self.old_folder / name)
        with tarfile.open(self.old_folder / 'source.tar', 'a') as archive:
            data = (self.old_folder / dev.albums.WORKER_COMPOSE).read_bytes()
            entry = tarfile.TarInfo(dev.albums.WORKER_COMPOSE); entry.size = len(data)
            archive.addfile(entry, io.BytesIO(data))
        old_compose = dev.read_environment(self.old_folder / 'compose.env')
        old_compose['PLATFORM_ALBUM_WORKER_ENV_FILE'] = str(self.old_folder / 'album-worker.env')
        (self.old_folder / 'compose.env').write_text(''.join(k + '=' + v + '\n' for k,v in old_compose.items()))
        old_files = {name: hashlib.sha256((self.old_folder / name).read_bytes()).hexdigest()
                     for name in dev.selection_files(self.old_folder)}
        (self.old_folder / 'selection.json').write_text(json.dumps({'source': self.old_source, 'files': old_files}))
        for p in self.old_folder.iterdir(): p.chmod(0o600)
        self.previous['files'] = old_files
        dev.atomic_json(self.runtime.state / 'platform-test.json', self.previous)
        names = [Path(name).name for name in dev.albums.SCHEMA_MIGRATIONS]
        for name in names: self.add_illustration_migration(name=name)
        self.photo_shared_migrated = self.photo_album_migrated = False
        self.photo_failure = None
        execute = self.runtime.execute.side_effect
        def migration(label, command, **kwargs):
            if label == self.photo_failure:
                self.photo_failure = None
                raise OperationError('controlled photo transition failure')
            if label.startswith('platform-test-photo-transition-'):
                service = label.removeprefix('platform-test-photo-transition-')
                project = dev.albums.PROJECT if service == 'albumworker' else dev.PROJECT
                self.assertEqual(command, ['docker', 'inspect', project + '-' + service + '-1'])
                return json.dumps([{'Config': {'Image': dev.PROJECT + '-' + service + ':' + self.old_source,
                    'Labels': {'com.docker.compose.project': project}}}]).encode()
            if label == 'platform-test-album-worker-stop':
                state = json.loads((self.runtime.state / 'platform-test.json').read_text())
                if not self.photo_shared_migrated:
                    self.assertEqual(state['album_worker_transition']['source'], self.source)
            if label == 'platform-test-update-migrate': self.photo_shared_migrated = True
            if label == 'platform-test-update-album-migrate': self.photo_album_migrated = True
            if label == 'platform-test-update-schema':
                return ((str(1 + len(names)) + '\t' + names[-1] if self.photo_shared_migrated
                         else '1\t0001_example.sql') + '\n').encode()
            if label in ('platform-test-migration-names', 'platform-test-album-schema'):
                done = self.photo_shared_migrated if label.endswith('names') else self.photo_album_migrated
                return ('0001_example.sql\n' + (''.join(name + '\n' for name in names) if done else '')).encode()
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = migration

    def photo_update(self):
        with patch.object(dev.albums, 'verify_gateway'), patch.object(dev.albums, 'verify_worker'):
            return self.update(albums_backup=True)

    def test_photo_migrations_wait_for_old_writer_exit_before_either_database(self):
        self.prepare_photo_writer_migration()
        result = self.photo_update()
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        stopped = labels.index('platform-test-album-worker-stopped')
        self.assertLess(labels.index('platform-test-build-albumworker'), stopped)
        for name in ('migrate', 'album-migrate'):
            self.assertLess(stopped, labels.index('platform-test-update-' + name))
        self.assertNotIn('album_worker_transition', result)
        self.assertNotIn('album_worker_transition', result['previous'])

    def test_photo_migration_failure_restores_old_worker_and_keeps_forward_schema(self):
        self.prepare_photo_writer_migration()
        self.photo_failure = 'platform-test-update-album-migrate'
        with self.assertRaisesRegex(OperationError, 'controlled photo transition failure'):
            self.photo_update()
        state = json.loads((self.runtime.state / 'platform-test.json').read_text())
        self.assertEqual(state['source'], self.old_source)
        self.assertEqual(state['latest_migration'], Path(dev.albums.SCHEMA_MIGRATIONS[-1]).name)
        self.assertNotIn('album_worker_transition', state)
        commands = [c.args[1] for c in self.runtime.execute.call_args_list]
        self.assertFalse(any('down' in c or '--volumes' in c or 'mysql' in c and 'up' in c for c in commands))

    def test_photo_drain_failure_does_not_start_any_migration(self):
        self.prepare_photo_writer_migration()
        self.photo_failure = 'platform-test-album-worker-stop'
        with self.assertRaisesRegex(OperationError, 'controlled photo transition failure'):
            self.photo_update()
        self.assertFalse(self.photo_shared_migrated or self.photo_album_migrated)
        self.assertNotIn('album_worker_transition', json.loads((self.runtime.state / 'platform-test.json').read_text()))

    def test_photo_failed_recovery_retains_checkpoint_and_resumes_forward_schema(self):
        self.prepare_photo_writer_migration()
        self.photo_failure = 'platform-test-update-album-migrate'
        with patch.object(dev, 'rollback_update', side_effect=OperationError('recovery unavailable')):
            with self.assertRaisesRegex(OperationError, 'recovery unavailable'):
                self.photo_update()
        path = self.runtime.state / 'platform-test.json'
        state = json.loads(path.read_text())
        self.assertIn('album_worker_transition', state)
        self.assertEqual(state['latest_migration'], Path(dev.albums.SCHEMA_MIGRATIONS[-1]).name)
        self.runtime.execute.reset_mock()
        self.photo_update()
        self.assertNotIn('album_worker_transition', json.loads(path.read_text()))
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        self.assertLess(labels.index('platform-test-update-rollback'), labels.index('platform-test-update-album-migrate'))
        self.assertNotIn('platform-test-update-migrate', labels)

    @unittest.skipUnless(hasattr(os, 'fork'), 'POSIX process kill required')
    def test_photo_writer_sigkill_is_recovered_before_new_backup_or_migration(self):
        self.prepare_photo_writer_migration()
        execute = self.runtime.execute.side_effect
        pid = os.fork()
        if pid == 0:
            def killed(label, command, **kwargs):
                if label == 'platform-test-update-migrate': os.kill(os.getpid(), signal.SIGKILL)
                return execute(label, command, **kwargs)
            self.runtime.execute.side_effect = killed
            try: self.photo_update()
            finally: os._exit(93)
        _, status = os.waitpid(pid, 0)
        self.assertTrue(os.WIFSIGNALED(status))
        self.assertEqual(os.WTERMSIG(status), signal.SIGKILL)
        self.assertIn('album_worker_transition', json.loads((self.runtime.state / 'platform-test.json').read_text()))
        self.runtime.execute.reset_mock()
        self.photo_update()
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        self.assertLess(labels.index('platform-test-update-rollback'), labels.index('platform-test-update-schema'))
        self.assertLess(labels.index('platform-test-update-rollback'), labels.index('platform-test-update-migrate'))

    def test_photo_recovery_rejects_changed_target_without_stopping_processes(self):
        self.prepare_photo_writer_migration()
        state = {**self.previous, 'album_worker_transition': {'source': 'f' * 40, 'files': {}}}
        dev.atomic_json(self.runtime.state / 'platform-test.json', state)
        with self.assertRaisesRegex(OperationError, 'does not match the interrupted'):
            self.photo_update()
        self.assertFalse(any('stop' in c.args[1] or 'up' in c.args[1] for c in self.runtime.execute.call_args_list))
        self.assertEqual(json.loads((self.runtime.state / 'platform-test.json').read_text()), state)

    def test_photo_recovery_rejects_foreign_worker_and_retains_checkpoint(self):
        self.prepare_photo_writer_migration()
        record = json.loads((self.folder / 'selection.json').read_text())
        state = {**self.previous, 'album_worker_transition': {'source': self.source, 'files': record['files']}}
        dev.atomic_json(self.runtime.state / 'platform-test.json', state)
        execute = self.runtime.execute.side_effect
        self.runtime.execute.side_effect = lambda label, command, **kw: (b'[{"Config":{"Image":"foreign"}}]'
            if label == 'platform-test-photo-transition-albumworker' else execute(label, command, **kw))
        with self.assertRaisesRegex(OperationError, 'unrelated process'):
            self.photo_update()
        self.assertFalse(any('stop' in c.args[1] or 'up' in c.args[1] for c in self.runtime.execute.call_args_list))
        self.assertEqual(json.loads((self.runtime.state / 'platform-test.json').read_text()), state)

    def test_media_activation_requires_backup_and_registers_matched_image(self):
        self.enable_album_media()
        self.assertIn('albumworker', dev.image_services(self.folder))
        with patch.object(dev.albums, 'verify_gateway'):
            with self.assertRaisesRegex(OperationError, 'full authenticated development backup'):
                self.update()
        self.assertFalse(any(c.args[0] == 'platform-test-album-worker-stop' for c in self.runtime.execute.call_args_list))
        with patch.object(dev.albums, 'verify_gateway'), patch.object(dev.albums, 'verify_worker'):
            result = self.update(albums_backup=True)
        self.assertEqual(result['source'], self.source)
        self.assertIn('album-worker.env', result['files'])
        labels = [c.args[0] for c in self.runtime.execute.call_args_list]
        self.assertLess(labels.index('platform-test-build-albumworker'), labels.index('platform-test-album-worker-stop'))
        self.assertLess(labels.index('platform-test-album-worker-start'), labels.index('platform-test-update-start'))

    def test_media_startup_failure_restores_only_existing_worker_and_shared_api(self):
        self.enable_album_media()
        with patch.object(dev.albums, 'verify_gateway'), patch.object(dev.albums, 'verify_worker', side_effect=OperationError('media startup failed')):
            with self.assertRaisesRegex(OperationError, 'media startup failed'):
                self.update(albums_backup=True)
        commands = [c.args[1] for c in self.runtime.execute.call_args_list if c.args[0] == 'platform-test-album-worker-start']
        self.assertEqual(len(commands), 2)
        self.assertIn(str(self.folder / dev.albums.WORKER_COMPOSE), commands[0])
        self.assertTrue(any('compose.albums-test.yaml' in value for value in commands[1]))
        self.assertEqual(commands[1][-1], 'albumworker')
        self.assertNotIn('albumgateway', commands[1])
        self.assertEqual(json.loads((self.runtime.state / 'platform-test.json').read_text())['source'], self.old_source)

    def test_album_and_platform_receipts_recover_interruption_before_state_write(self):
        self.prepare_album_product_migration()
        self.migrated = self.album_migrated = True
        execute = self.runtime.execute.side_effect
        def committed(label, command, **kwargs):
            if label == 'platform-test-update-schema':
                return b'2\t0073_album_development_subscription_products.sql\n'
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = committed
        with patch.object(dev.albums, 'verify_gateway'):
            result = self.update(albums_backup=True)
        self.assertEqual(result['migrations'], 2)
        self.assertEqual(result['album_migrations'], 2)
        self.assertFalse(any(c.args[0] in ('platform-test-update-migrate', 'platform-test-update-album-migrate')
                             for c in self.runtime.execute.call_args_list))

    def test_previous_album_overlay_remains_readable_but_cannot_run_new_job(self):
        self.enable_albums()
        body = (Path(__file__).parent / 'testdata/previous-platform-albums-test.yaml').read_bytes()
        self.assertEqual(hashlib.sha256(body).hexdigest(), dev.albums.ROLLBACK_OVERLAY_SHA256)
        (self.folder / dev.albums.OVERLAY).write_bytes(body)
        with tarfile.open(self.folder / 'source.tar') as archive:
            entries = [(m.name, archive.extractfile(m).read()) for m in archive.getmembers()]
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name, data in entries:
                if name == dev.albums.OVERLAY: data = body
                entry = tarfile.TarInfo(name); entry.size = len(data)
                archive.addfile(entry, io.BytesIO(data))
        self.save()
        dev.selection(self.runtime, self.source)

    def test_album_product_migration_never_runs_without_both_recovery_selections(self):
        self.prepare_album_product_migration()
        with self.assertRaisesRegex(OperationError, 'full authenticated development backup'):
            self.update()
        self.assertFalse(any(c.args[0].startswith('platform-test-build') or 'migrate' in c.args[0]
                             for c in self.runtime.execute.call_args_list))

    def test_album_migration_failure_keeps_original_api_and_does_not_restore_data(self):
        self.prepare_album_product_migration()
        execute = self.runtime.execute.side_effect
        def fail(label, command, **kwargs):
            if label == 'platform-test-update-album-migrate':
                raise OperationError('scoped migration failed')
            return execute(label, command, **kwargs)
        self.runtime.execute.side_effect = fail
        with self.assertRaisesRegex(OperationError, 'scoped migration failed'):
            self.update(albums_backup=True)
        state = json.loads((self.runtime.state / 'platform-test.json').read_text())
        self.assertEqual(state['source'], self.old_source)
        self.assertEqual(state['migrations'], 2)
        self.assertFalse(any('up' in c.args[1] or 'down' in c.args[1] or '--volumes' in c.args[1]
                             for c in self.runtime.execute.call_args_list))

    def test_admission_requires_matched_scheduled_backup_and_maintenance_before_any_mutation(self):
        self.enable_journal()
        scripts = self.root / 'deploy/tencent'
        for name in ('operations.py', 'platform_test_artifacts.py', 'platform_test_maintenance.py', 'platform_test_albums.py'):
            path = scripts / name
            expected = path.read_bytes()
            path.write_bytes(b'old operations version')
            error = 'reviewed recovery-helper transition' if name == 'operations.py' else 'matched scheduled operations'
            with self.subTest(name=name), self.assertRaisesRegex(OperationError, error):
                self.activate()
            path.write_bytes(expected)
        self.runtime.execute.assert_not_called()
        path = scripts / 'platform_test_maintenance.py'
        path.unlink()
        with self.assertRaisesRegex(OperationError, 'matched scheduled operations'):
            self.activate()
        path.symlink_to(Path(__file__).resolve().parent / path.name)
        with self.assertRaisesRegex(OperationError, 'matched scheduled operations'):
            self.activate()




class SourceBoundBuildCacheTests(unittest.TestCase):
    def test_cache_cleanup_is_source_scoped_and_runs_after_failed_build(self):
        runtime=Mock()
        runtime.execute.side_effect=lambda label,*args,**kw: (_ for _ in ()).throw(OperationError('compile failure')) if label=='platform-test-build-gateway' else b''
        with tempfile.TemporaryDirectory() as directory:
            folder=Path(directory);(folder/'source.tar').write_bytes(b'fixture archive')
            with patch.object(dev,'image_services',return_value=('gateway',)):
                with self.assertRaisesRegex(OperationError,'compile failure'):
                    dev.build_images(runtime,folder,{'files':{'source.tar':'fixture-hash'}},'a'*40)
        build=next(c.args[1] for c in runtime.execute.call_args_list if c.args[0]=='platform-test-build-gateway')
        self.assertIn('--force-rm',build)
        self.assertIn('PLATFORM_TEST_BUILD_SOURCE='+'a'*40,build)
        self.assertEqual(runtime.execute.call_args_list[-1].args[1],['docker','image','prune','--force','--filter','label=cn.tellyouwhat.platform-test-build-source='+'a'*40])
        dockerfile=(Path(__file__).resolve().parents[2]/'deploy/tencent/platform-test.Dockerfile').read_text()
        build_stage,runtime_stage=dockerfile.split('FROM scratch',1)
        self.assertIn('LABEL cn.tellyouwhat.platform-test-build-source=',build_stage)
        self.assertNotIn('platform-test-build-source',runtime_stage)


if __name__ == '__main__':
    unittest.main()
