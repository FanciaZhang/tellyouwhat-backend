import gzip
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import ops_backup
from ops_backup import crypt, manifest_signature, prune_backups, verify_backup
from ops_common import OperationError, Runtime, atomic_json


class BackupRetentionResumeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.now = int(time.time())
        self.serial = 0
        self.runtime = self.runtime_at(self.root / 'default')

    def runtime_at(self, root):
        root.mkdir()
        settings = root / '.env.production'
        settings.write_text('MYSQL_DATABASE=retention_fixture\nBACKUP_ENCRYPTION_KEY=' + 'x' * 64 + '\n')
        settings.chmod(0o600)
        runtime = Runtime(root, root / 'backups')
        runtime.backups.mkdir(mode=0o700)
        return runtime

    def fixture(self, runtime=None, *, created=None, recovery=True):
        runtime = runtime or self.runtime
        self.serial += 1
        source = runtime.backups / ('mysql-20261102T000000Z-%06x.sql.gz.enc' % self.serial)
        source.write_bytes(b'controlled ciphertext ' + str(self.serial).encode())
        source.chmod(0o600)
        manifest = {'filename': source.name, 'database': 'retention_fixture', 'scope': 'control-plane',
                    'created_at': self.now - 15 * 86400 if created is None else created,
                    'table_rows': {'schema_migrations': 1, 'apps': 1}, 'included_data_tables': ['schema_migrations', 'apps'],
                    'excluded_data_tables': [], 'sha256': ops_backup.file_digest(source)}
        members = [source]
        if recovery:
            bundle = source.parent / ('recovery-' + source.name.replace('.sql.gz.enc', '.tar.gz.enc'))
            bundle.write_bytes(b'controlled encrypted recovery selection')
            bundle.chmod(0o600)
            manifest.update(recovery_filename=bundle.name, recovery_sha256=ops_backup.file_digest(bundle))
            members.append(bundle)
        self.sign(runtime, source, manifest)
        return source, members + [Path(str(source) + '.json')], manifest

    def sign(self, runtime, source, manifest):
        atomic_json(str(source) + '.json', {'manifest': manifest, 'hmac_sha256': manifest_signature('x' * 64, manifest)})

    def child(self, runtime, program, *args):
        return subprocess.Popen([sys.executable, '-c', program, str(runtime.root), *map(str, args)],
                                cwd=Path(__file__).parent, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

    def stop_child(self, process):
        if process.poll() is None:
            process.kill()
        return process.communicate(timeout=10)

    def wait_ready(self, process, marker):
        deadline = time.monotonic() + 8
        while not marker.exists() and process.poll() is None and time.monotonic() < deadline:
            time.sleep(.02)
        self.assertIsNone(process.poll(), self.stop_child(process) if process.poll() is not None else 'still running')
        self.assertTrue(marker.exists(), 'owned child did not reach the fault-injection point')

    def test_expired_partial_payload_sets_resume_but_never_verify_as_restorable(self):
        for missing in ('snapshot', 'recovery', 'both', 'no-recovery'):
            with self.subTest(missing=missing):
                runtime = self.runtime_at(self.root / missing)
                source, members, _ = self.fixture(runtime, recovery=missing != 'no-recovery')
                if missing in ('snapshot', 'both', 'no-recovery'):
                    source.unlink()
                if missing in ('recovery', 'both'):
                    members[1].unlink()
                with self.assertRaises(OperationError):
                    verify_backup(runtime, source)
                self.assertEqual(prune_backups(runtime), 1)
                self.assertEqual(list(runtime.backups.iterdir()), [])
                self.assertEqual(prune_backups(runtime), 0)

    def test_killed_pruner_resumes_with_authenticated_inventory(self):
        program = '''
import os,sys,time
from pathlib import Path
from unittest.mock import patch
from ops_common import Runtime
from ops_backup import prune_backups
root=Path(sys.argv[1]);runtime=Runtime(root,root/'backups')
original=Path.unlink;removed=0
def interrupt(path,*args,**kwargs):
 global removed
 result=original(path,*args,**kwargs)
 if path.parent==runtime.backups:
  removed+=1
  if removed==int(sys.argv[2]):
   (root/'ready').write_text('payload removed')
   while True:time.sleep(1)
 return result
with runtime.lock(),patch.object(Path,'unlink',interrupt):prune_backups(runtime)
'''
        for stage in (1, 2):
            with self.subTest(stage=stage):
                runtime = self.runtime_at(self.root / ('kill-' + str(stage)))
                source, members, _ = self.fixture(runtime)
                process = self.child(runtime, program, stage)
                self.addCleanup(self.stop_child, process)
                self.wait_ready(process, runtime.root / 'ready')
                process.kill()
                process.communicate(timeout=10)
                self.assertEqual(process.returncode, -9)
                self.assertTrue(members[-1].exists(), 'authenticated inventory was removed before all payloads')
                result = subprocess.run([sys.executable, '-c',
                    "import sys,json;from pathlib import Path;from ops_common import Runtime;from ops_backup import prune_backups;"
                    "r=Path(sys.argv[1]);print(json.dumps({'removed':prune_backups(Runtime(r,r/'backups'))}))", str(runtime.root)],
                    cwd=Path(__file__).parent, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout)['removed'], 1)
                self.assertTrue(all(not p.exists() for p in members))

    def test_recent_or_unauthenticated_orphans_are_preserved(self):
        for missing in ('snapshot', 'recovery', 'manifest', 'snapshot-and-manifest'):
            with self.subTest(missing=missing):
                runtime = self.runtime_at(self.root / missing)
                _, members, _ = self.fixture(runtime, created=self.now if missing in ('snapshot', 'recovery') else None)
                remove = {'snapshot': [0], 'recovery': [1], 'manifest': [2], 'snapshot-and-manifest': [0, 2]}[missing]
                for index in remove:
                    members[index].unlink()
                before = {p.name: p.read_bytes() for p in members if p.exists()}
                with self.assertRaises(OperationError):
                    prune_backups(runtime)
                self.assertEqual({p.name: p.read_bytes() for p in runtime.backups.iterdir()}, before)

    def test_authenticated_age_not_file_mtime_controls_the_exact_boundary(self):
        source, members, _ = self.fixture(created=self.now - ops_backup.BACKUP_RETENTION_SECONDS + 1)
        for path in members:
            os.utime(path, (1, 1))
        with patch('ops_backup.time.time', return_value=self.now):
            self.assertEqual(prune_backups(self.runtime), 0)
        with patch('ops_backup.time.time', return_value=self.now + 1):
            self.assertEqual(prune_backups(self.runtime), 1)
        self.assertFalse(source.exists())

    def test_invalid_metadata_or_corrupt_payload_cannot_authorize_partial_deletion(self):
        for kind in ('bad-hmac', 'database', 'scope', 'recovery-path', 'hash', 'rows', 'ciphertext', 'recovery', 'oversized'):
            with self.subTest(kind=kind):
                runtime = self.runtime_at(self.root / kind)
                source, members, manifest = self.fixture(runtime)
                if kind == 'database': manifest['database'] = 'another_app'
                if kind == 'scope': manifest['recovery_scope'] = 'unknown'
                if kind == 'recovery-path': manifest['recovery_filename'] = '../outside'
                if kind == 'hash': manifest['sha256'] = 'not-a-digest'
                if kind == 'rows': manifest['table_rows'] = {'apps': -1}
                self.sign(runtime, source, manifest)
                if kind == 'bad-hmac':
                    value = json.loads(members[-1].read_text());value['hmac_sha256'] = '0' * 64;atomic_json(members[-1], value)
                if kind in ('ciphertext', 'recovery'):
                    members[0 if kind == 'ciphertext' else 1].write_bytes(b'changed content')
                if kind == 'oversized':
                    value = json.loads(members[-1].read_text());value['padding'] = 'x' * (1024 * 1024);atomic_json(members[-1], value)
                before = [p.read_bytes() for p in members]
                with self.assertRaises(OperationError):
                    prune_backups(runtime)
                self.assertEqual([p.read_bytes() for p in members], before)

    def test_invalid_family_does_not_skip_other_expired_family(self):
        _, invalid, _ = self.fixture()
        invalid[0].write_bytes(b'corrupt')
        _, expired, _ = self.fixture()
        source, recent, _ = self.fixture(created=self.now)
        with self.assertRaisesRegex(OperationError, 'retention verification failed'):
            prune_backups(self.runtime)
        self.assertTrue(all(p.exists() for p in invalid + recent))
        self.assertTrue(all(not p.exists() for p in expired))
        verify_backup(self.runtime, source)

    def test_links_and_nonregular_files_are_rejected_without_following_them(self):
        for kind in ('symlink', 'hardlink', 'fifo'):
            for index in range(3):
                with self.subTest(kind=kind, member=index):
                    runtime = self.runtime_at(self.root / (kind + str(index)))
                    _, members, _ = self.fixture(runtime)
                    protected = runtime.root / 'protected';protected.write_bytes(members[index].read_bytes())
                    members[index].unlink()
                    if kind == 'symlink': members[index].symlink_to(protected)
                    if kind == 'hardlink': os.link(protected, members[index])
                    if kind == 'fifo': os.mkfifo(members[index])
                    before = protected.read_bytes()
                    with self.assertRaises(OperationError): prune_backups(runtime)
                    self.assertEqual(protected.read_bytes(), before)
                    self.assertTrue(all(p.exists() for p in members))

    def test_replaced_recovery_is_preserved_and_inventory_remains_for_review(self):
        source, members, _ = self.fixture()
        original = Path.unlink
        def replace_before_unlink(path, *args, **kwargs):
            if path == source:
                replacement = self.runtime.root / 'new-selection';replacement.write_bytes(b'uncommitted new selection')
                os.replace(replacement, members[1])
            return original(path, *args, **kwargs)
        with patch.object(Path, 'unlink', replace_before_unlink), self.assertRaises(OperationError):
            prune_backups(self.runtime)
        self.assertEqual(members[1].read_bytes(), b'uncommitted new selection')
        self.assertTrue(members[-1].exists())
        with self.assertRaises(OperationError): prune_backups(self.runtime)

    def test_unregistered_recovery_cannot_remove_a_valid_inventory(self):
        source, members, _ = self.fixture(recovery=False)
        unexpected = source.parent / ('recovery-' + source.name.replace('.sql.gz.enc', '.tar.gz.enc'))
        unexpected.write_bytes(b'unregistered recovery selection')
        verify_backup(self.runtime, source)
        with self.assertRaises(OperationError): prune_backups(self.runtime)
        self.assertTrue(all(p.exists() for p in members))
        self.assertEqual(unexpected.read_bytes(), b'unregistered recovery selection')

    def test_late_payload_is_preserved_before_inventory_commit(self):
        source, members, _ = self.fixture()
        original = Path.unlink
        def recreate_after_unlink(path, *args, **kwargs):
            result = original(path, *args, **kwargs)
            if path == source:
                source.write_bytes(b'late uncommitted input')
            return result
        with patch.object(Path, 'unlink', recreate_after_unlink), self.assertRaises(OperationError):
            prune_backups(self.runtime)
        self.assertEqual(source.read_bytes(), b'late uncommitted input')
        self.assertTrue(members[-1].exists())

    def test_uploading_sdk_process_holds_all_inputs_until_success_failure_or_cancellation(self):
        program = '''
import hashlib,io,sys,time
from pathlib import Path
from types import SimpleNamespace
from ops_cos import transfer
root=Path(sys.argv[1]);paths=sorted((root/'backups').iterdir());stored={}
class Client:
 def put_object(self,**kw):
  stored[kw['Key']]=kw['Body'].read()
  if len(stored)==1:
   (root/'ready').write_text('upload in progress')
   while not (root/'release').exists():time.sleep(.02)
   if sys.argv[2]=='failure':raise RuntimeError('controlled transfer failure')
 def get_object(self,**kw):
  stream=io.BytesIO(stored[kw['Key']]);return {'Body':SimpleNamespace(get_raw_stream=lambda:stream)}
transfer(Client(),'fixture','backups/','upload',paths,
 expected={p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in paths})
'''
        for mode in ('success', 'failure', 'cancel'):
            with self.subTest(mode=mode):
                runtime = self.runtime_at(self.root / mode)
                _, members, _ = self.fixture(runtime)
                before = [p.read_bytes() for p in members]
                process = self.child(runtime, program, mode)
                self.addCleanup(self.stop_child, process)
                self.wait_ready(process, runtime.root / 'ready')
                with self.assertRaises(OperationError): prune_backups(runtime)
                self.assertEqual([p.read_bytes() for p in members], before)
                if mode == 'cancel': process.kill()
                else: (runtime.root / 'release').touch()
                _, error = process.communicate(timeout=10)
                self.assertEqual(process.returncode, {'success': 0, 'failure': 1, 'cancel': -9}[mode], error)
                self.assertEqual(prune_backups(runtime), 1)
                self.assertTrue(all(not p.exists() for p in members))

    def test_real_encrypted_snapshot_stays_readable_then_expires_with_its_recovery(self):
        source, members, manifest = self.fixture(created=self.now)
        plaintext = self.runtime.root / 'snapshot.sql.gz'
        plaintext.write_bytes(gzip.compress(b'CREATE TABLE apps(id INT); INSERT INTO apps VALUES(1);', mtime=0))
        crypt(self.runtime, plaintext, source)
        crypt(self.runtime, plaintext, members[1])
        manifest.update(sha256=ops_backup.file_digest(source), recovery_sha256=ops_backup.file_digest(members[1]))
        self.sign(self.runtime, source, manifest)
        verify_backup(self.runtime, source)
        self.assertEqual(prune_backups(self.runtime), 0)
        decoded = self.runtime.root / 'decoded.gz';crypt(self.runtime, source, decoded, decrypt=True)
        self.assertEqual(decoded.read_bytes(), plaintext.read_bytes())
        with patch('ops_backup.time.time', return_value=self.now + ops_backup.BACKUP_RETENTION_SECONDS):
            self.assertEqual(prune_backups(self.runtime), 1)
        self.assertTrue(all(not p.exists() for p in members))


if __name__ == '__main__':
    unittest.main()
