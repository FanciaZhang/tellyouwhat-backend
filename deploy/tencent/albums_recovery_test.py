"""Authenticated input and Docker protocol tests; not a Docker execution test."""
import contextlib
import copy
import datetime
import hashlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch

import albums_artifacts as artifacts
import albums_recovery as recovery
from ops_backup import manifest_signature, prune_restore_workspaces
from ops_common import OperationError, Runtime


class AlbumsRuntimeRecoveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='albums-runtime-')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        platform = self.root / 'platform'; platform.mkdir(mode=0o700)
        env = platform / '.env.production'
        env.write_text('MYSQL_DATABASE=control_fixture\nBACKUP_ENCRYPTION_KEY=' + 'c' * 64 + '\n')
        env.chmod(0o600)
        self.runtime = Runtime(platform, platform / 'backups')
        self.environment = 'development'
        self.snapshot = 'albums-' + '1' * 32 + '.sql.gz.enc'
        self.ledger = 'albums-deletions-' + '2' * 32 + '.json'
        self.created = int(time.time())
        self.opaque = b'controlled opaque encrypted snapshot'
        self.snapshot_manifest = dict(scope='albums-quarantined-v1', created_at=self.created, database='albums_test',
                                      filename=self.snapshot, sha256=hashlib.sha256(self.opaque).hexdigest())
        with recovery._implementation() as modules:
            tables = modules['backup'].TABLES
        self.snapshot_manifest.update(table_rows={t: 0 for t in tables},
                                      table_sha256={t: hashlib.sha256(b'').hexdigest() for t in tables})
        self.checkpoint = dict(scope='albums-deletion-ledger-v3', database='albums_test', exported_at=self.created,
                               entries=[], assets=[], collections=[])
        self.settings('development')
        self.load_implementation = recovery._implementation
        self.commands = []
        self.present = False
        self.failure = None
        self.restored = 0
        self.prepared = 0
        self.expected_checkpoint = None

    def settings(self, environment):
        self.environment = environment
        database, user = recovery.database_scope(environment)
        self.snapshot_manifest['database'] = self.checkpoint['database'] = database
        self.saved = dict(MYSQL_HOST='localhost', MYSQL_PORT='3306', MYSQL_DATABASE=database, MYSQL_USER=user,
                          MYSQL_PASSWORD='b' * 64)
        app = self.root / ('albums-' + environment); app.mkdir(mode=0o700, exist_ok=True)
        self.config = app / 'backup.env'
        self.config.write_text(''.join(k + '=' + v + '\n' for k, v in self.saved.items()) +
                               'ALBUM_BACKUP_ENCRYPTION_KEY=' + 'a' * 64 + '\n')
        self.config.chmod(0o600)

    def envelope(self, value, field):
        return json.dumps({field: value, 'hmac_sha256': manifest_signature('a' * 64, value)}).encode()

    def archive(self):
        files = {name: b'authenticated configuration fixture' for name in
                 ('state.json', 'owner.json', 'database.env', *('current/' + n for n in artifacts.FILES),
                  *('secrets/' + n for n in artifacts.KEYS))}
        if self.environment == 'production':
            files.pop('current/compose.albums-test.yaml')
            files['current/compose.albums-production.yaml'] = b'fixture'
            files['current/runtime.env'] = (b'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums\n'
                b'ALBUM_API_HOST=api.albums.tellyouwhat.cn\nALBUM_COS_BUCKET=tellyouwhat-albums-production-1251995834\n')
        files.update({'backup.env': ''.join(k + '=' + v + '\n' for k, v in self.saved.items()).encode(),
                      'checkpoint/' + self.snapshot: self.opaque,
                      'checkpoint/' + self.snapshot + '.json': self.envelope(self.snapshot_manifest, 'manifest'),
                      'checkpoint/' + self.ledger: self.envelope(self.checkpoint, 'payload')})
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode='w') as archive:
            for name, content in files.items():
                member = tarfile.TarInfo('recovery/albums-' + self.environment + '/' + name)
                member.size = len(content); archive.addfile(member, io.BytesIO(content))
        data.seek(0)
        return tarfile.open(fileobj=data), {n: hashlib.sha256(v).hexdigest() for n, v in files.items()}

    def execute(self, label, command, **kwargs):
        self.commands.append((label, command, kwargs))
        if label in ('albums-asset-deletion-ledger-schema', 'albums-collection-deletion-schema', 'albums-deletion-ledger-export'):
            self.assertEqual(command[:6], ['docker', 'exec', '--env', 'MYSQL_PWD',
                                           'tellyouwhat-albums-' + self.environment + '-mysql-1', 'mysql'])
            self.assertIn('--database=' + self.saved['MYSQL_DATABASE'], command)
            self.assertIn('--user=' + self.saved['MYSQL_USER'], command)
            self.assertNotIn(self.saved['MYSQL_PASSWORD'], ' '.join(command))
            self.assertEqual(kwargs['env'], {'MYSQL_PWD': self.saved['MYSQL_PASSWORD']})
            if label.endswith('-schema'):
                return b'1\t1\n'
            return ('__albums_source__\n' + self.saved['MYSQL_DATABASE'] + '\t1\n'
                    '__albums_entries__\n__albums_assets__\n__albums_collections__\n').encode()
        if label == 'albums-runtime-restore-start':
            self.container = command[command.index('--name') + 1]
            mount = command[command.index('--mount') + 1]
            self.folder = Path(mount.split('source=')[1].split(',')[0]).parent
            self.present = True
            if self.failure == 'unknown-start':
                raise OperationError('controlled lost Docker response')
            return b'a' * 64
        if label == 'restore-workspace-list':
            return (b'a' * 64 + b'\n') if self.present else b''
        if label == 'restore-workspace-inspect':
            return json.dumps([dict(Name='/' + self.container, Id='a' * 64, Config=dict(Labels={
                'cn.tellyouwhat.operation': 'restore-drill',
                'cn.tellyouwhat.restore-workspace': self.folder.name}))]).encode()
        self.assertEqual(label, 'restore-workspace-cleanup')
        if self.failure == 'cleanup':
            raise OperationError('controlled cleanup failure')
        self.present = False
        return b''

    @contextlib.contextmanager
    def implementation(self):
        with self.load_implementation() as modules:
            def prepare(child, snapshot, folder):
                self.prepared += 1
                self.assertEqual(child.config['BACKUP_ENCRYPTION_KEY'], 'a' * 64)
                self.assertEqual(self.runtime.config['BACKUP_ENCRYPTION_KEY'], 'c' * 64)
                raw = folder / 'snapshot.sql'; raw.touch(mode=0o600)
                return raw
            def restore(child, sql, dump, manifest, raw, folder, diagnostic, checkpoint):
                self.restored += 1
                self.assertEqual(checkpoint, self.expected_checkpoint or self.checkpoint)
                self.assertIn('--defaults-extra-file=/run/albums-client.cnf', sql)
                if self.failure in (RuntimeError, KeyboardInterrupt):
                    raise self.failure('controlled SQL interruption')
            with patch.object(modules['backup'], 'prepare_restore_sql', side_effect=prepare), \
                 patch.object(modules['backup'], 'restore_database', side_effect=restore):
                yield modules

    def run_restore(self, ready=0, independent_checkpoint=None, verify_checkpoint_source=False):
        archive, manifest = self.archive()
        implementation = self.implementation
        with archive, patch.object(self.runtime, 'execute', side_effect=self.execute), \
             patch.object(recovery, '_implementation', side_effect=implementation), \
             patch.object(subprocess, 'run', return_value=subprocess.CompletedProcess([], ready)), \
             patch.object(recovery.time, 'sleep'):
            return recovery.verify_runtime_recovery(self.runtime, archive, manifest, environment=self.environment,
                                                    image='mysql:8.4', backup_filename='shared-backup.enc',
                                                    independent_checkpoint=independent_checkpoint,
                                                    verify_checkpoint_source=verify_checkpoint_source)

    def independent(self, payload):
        path = self.root / 'independent-checkpoint.json'
        path.write_bytes(self.envelope(payload, 'payload')); path.chmod(0o600)
        return path, hashlib.sha256(path.read_bytes()).hexdigest()

    def test_independent_newer_checkpoint_reaches_restore_and_keeps_quarantine(self):
        for environment in ('development', 'production'):
            self.settings(environment)
            selected = copy.deepcopy(self.checkpoint); selected['exported_at'] += 1
            stamp = datetime.datetime.fromtimestamp(self.created, datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S.%fZ')
            selected['entries'] = [['10000000-0000-4000-8000-000000000001',
                                    '20000000-0000-4000-8000-000000000001', 'ab' * 32, stamp]]
            self.expected_checkpoint = selected
            pin = self.independent(selected); original = pin[0].read_bytes()
            result = self.run_restore(independent_checkpoint=pin)
            self.assertEqual(result['deletion_intents'], 1)
            self.assertTrue(result['independent_checkpoint_applied'])
            self.assertEqual(result['independent_checkpoint_sha256'], pin[1])
            self.assertFalse(result['latest_checkpoint_proven']); self.assertFalse(result['recovery_ready'])
            self.assertEqual(pin[0].read_bytes(), original)
            self.assertFalse(self.present)
            self.assertEqual(list(self.runtime.state.glob('.restore-*')), [])

    def test_selected_source_is_checked_before_and_after_each_environment_drill(self):
        for environment in ('development', 'production'):
            self.settings(environment)
            self.commands.clear()
            pin = self.independent(self.checkpoint)
            result = self.run_restore(independent_checkpoint=pin, verify_checkpoint_source=True)
            labels = [label for label, _, _ in self.commands]
            pre, post = [i for i, label in enumerate(labels) if label == 'albums-deletion-ledger-export']
            self.assertLess(pre, labels.index('albums-runtime-restore-start'))
            self.assertLess(labels.index('albums-runtime-restore-start'), post)
            self.assertLess(post, labels.index('restore-workspace-cleanup'))
            self.assertLessEqual(result['checkpoint_source_prechecked_at'], result['checkpoint_source_rechecked_at'])
            self.assertTrue(result['checkpoint_source_point_in_time_only'])
            self.assertFalse(result['latest_checkpoint_proven']); self.assertFalse(result['recovery_ready'])

    def test_bad_independent_checkpoint_is_rejected_before_decryption_or_docker(self):
        for fault in ('older', 'database', 'scope', 'future', 'digest', 'signature', 'symlink', 'public', 'missing-pin'):
            self.commands.clear()
            selected = copy.deepcopy(self.checkpoint)
            if fault == 'older': selected['exported_at'] -= 1
            if fault == 'database': selected['database'] = 'albums_production'
            if fault == 'scope': selected['scope'] = 'albums-deletion-ledger-v2'
            if fault == 'future': selected['exported_at'] += 3600
            pin = self.independent(selected)
            if fault == 'digest': pin = (pin[0], '0' * 64)
            if fault == 'signature':
                envelope = json.loads(pin[0].read_text()); envelope['hmac_sha256'] = '0' * 64
                pin[0].write_text(json.dumps(envelope)); pin = (pin[0], hashlib.sha256(pin[0].read_bytes()).hexdigest())
            if fault == 'symlink':
                link = self.root / 'independent-link.json'; link.symlink_to(pin[0]); pin = (link, pin[1])
            if fault == 'public': pin[0].chmod(0o644)
            if fault == 'missing-pin': pin = (pin[0],)
            with self.subTest(fault=fault), self.assertRaises(OperationError):
                self.run_restore(independent_checkpoint=pin)
            self.assertFalse(any(label == 'albums-runtime-restore-start' for label, _, _ in self.commands))
            self.assertEqual(self.prepared, 0)
            self.assertEqual(self.restored, 0)
            self.assertEqual(list(self.runtime.state.glob('.restore-*')), [])

    def test_both_environments_are_isolated_and_quarantined_without_cutover_authority(self):
        for environment in ('development', 'production'):
            self.settings(environment)
            result = self.run_restore()
            self.assertTrue(result['quarantined'])
            self.assertFalse(result['latest_checkpoint_proven']); self.assertFalse(result['recovery_ready'])
            start = next(c for label, c, _ in self.commands if label == 'albums-runtime-restore-start')
            self.assertEqual(start[start.index('--network') + 1], 'none')
            self.assertIn('--rm', start)
            self.assertFalse(any('password=' in a or 'a' * 64 in a or 'b' * 64 in a for a in start))
            self.assertFalse(self.present)
            self.assertEqual(list(self.runtime.state.glob('.restore-*')), [])
        self.assertEqual(self.restored, 2)

    def test_authenticated_inputs_still_require_correct_inner_scope_signature_and_order(self):
        original = self.snapshot_manifest.copy()
        for fault in ('expired', 'future', 'database', 'snapshot-hash', 'ledger-order', 'ledger-database', 'key', 'missing-key'):
            with self.subTest(fault=fault):
                self.commands.clear()
                self.snapshot_manifest = original.copy(); self.checkpoint['exported_at'] = self.created
                self.checkpoint['database'] = 'albums_test'; self.settings('development')
                if fault == 'expired': self.snapshot_manifest['created_at'] -= 15 * 86400
                if fault == 'future': self.snapshot_manifest['created_at'] += 3600
                if fault == 'database': self.snapshot_manifest['database'] = 'albums_production'
                if fault == 'snapshot-hash': self.snapshot_manifest['sha256'] = 'f' * 64
                if fault == 'ledger-order': self.checkpoint['exported_at'] -= 1
                if fault == 'ledger-database': self.checkpoint['database'] = 'albums_production'
                if fault == 'key': self.config.write_text(self.config.read_text().replace('a' * 64, 'd' * 64))
                if fault == 'missing-key': self.config.unlink()
                with self.assertRaises(OperationError): self.run_restore()
                self.assertFalse(any(label == 'albums-runtime-restore-start' for label, _, _ in self.commands))
                self.assertEqual(self.restored, 0)
                self.assertEqual(list(self.runtime.state.glob('.restore-*')), [])

    def test_failure_cancellation_and_unknown_start_are_closed_and_cleaned(self):
        for failure in (RuntimeError, KeyboardInterrupt, 'unknown-start', 'readiness'):
            self.failure = failure
            with self.subTest(failure=failure), self.assertRaises((RuntimeError, KeyboardInterrupt, OperationError)):
                self.run_restore(ready=1 if failure == 'readiness' else 0)
            self.assertFalse(self.present)
            self.assertEqual(list(self.runtime.state.glob('.restore-*')), [])

    def test_unconfirmed_cleanup_retains_owned_files_for_later_retry(self):
        self.failure = 'cleanup'
        with self.assertRaises(OperationError): self.run_restore()
        self.assertTrue(self.folder.exists()); self.assertTrue((self.folder / 'owner.json').exists())
        self.failure = None
        with patch.object(self.runtime, 'execute', side_effect=self.execute):
            prune_restore_workspaces(self.runtime)
        self.assertFalse(self.folder.exists()); self.assertFalse(self.present)

    def test_installed_validator_does_not_leak_module_or_search_path_changes(self):
        before, search = sys.modules.copy(), sys.path[:]
        with self.assertRaises(RuntimeError):
            with recovery._implementation() as modules:
                self.assertEqual(sys.modules['backup'], modules['backup'])
                raise RuntimeError('controlled validation exit')
        for name in ('backup', 'deletion_ledger', 'asset_deletion_ledger', 'collection_deletion_ledger'):
            self.assertIs(sys.modules.get(name), before.get(name))
        self.assertEqual(sys.path, search)


if __name__ == '__main__':
    unittest.main()
