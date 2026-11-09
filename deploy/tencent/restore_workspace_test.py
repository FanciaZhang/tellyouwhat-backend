import fcntl
import gzip
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

from ops_backup import (_restore_workspace_owner, backup_secret, crypt, file_digest, manifest_signature,
                        prune_restore_workspaces, restore_drill, restore_workspace)
from ops_common import OperationError, Runtime, atomic_json


class RestoreWorkspaceTests(unittest.TestCase):
    filename = 'mysql-20261103T000000Z-abcdef.sql.gz.enc'
    container = 'tellyouwhat-restore-' + '1' * 16

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='shared-restore-test-')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        settings = self.root / '.env.production'
        settings.write_text('MYSQL_DATABASE=restore_fixture\nBACKUP_ENCRYPTION_KEY=' + 'x' * 64 + '\n')
        settings.chmod(0o600)
        self.runtime = Runtime(self.root, self.root / 'backups')
        self.runtime.backups.mkdir(mode=0o700)
        self.original = self.runtime.backups / 'preserved-encrypted-backup'
        self.original.write_bytes(b'preserved synthetic recovery input')

    def write(self, path, content=b'synthetic temporary recovery input'):
        path.touch(mode=0o600)
        path.write_bytes(content)

    def abandoned(self, kind='download'):
        folder = Path(tempfile.mkdtemp(prefix='.restore-' + kind + '-', dir=self.runtime.state))
        owner, names = _restore_workspace_owner(self.runtime, folder, kind, self.filename if kind == 'download' else self.container,
                                               **self.selection(kind))
        atomic_json(folder / 'owner.json', {'owner': owner, 'hmac_sha256': manifest_signature(backup_secret(self.runtime), owner)})
        for name in names:
            self.write(folder / name)
        return folder

    def selection(self, kind):
        return dict(snapshot='albums-' + '1' * 32 + '.sql.gz.enc', ledger='albums-deletions-' + '2' * 32 + '.json') if kind == 'albums-sql' else {}

    def test_success_failure_and_cancellation_remove_only_owned_files(self):
        for kind in ('download', 'sql', 'albums-sql'):
            for failure in (None, RuntimeError, KeyboardInterrupt):
                with self.subTest(kind=kind, failure=failure), patch.object(self.runtime, 'execute', return_value=b''):
                    try:
                        with restore_workspace(self.runtime, kind, self.filename if kind == 'download' else self.container, **self.selection(kind)) as folder:
                            _, names = _restore_workspace_owner(self.runtime, folder, kind, self.filename if kind == 'download' else self.container, **self.selection(kind))
                            for name in names:
                                self.write(folder / name)
                            if failure:
                                raise failure('controlled cancellation')
                    except (RuntimeError, KeyboardInterrupt):
                        pass
                    self.assertFalse(folder.exists())
                    self.assertEqual(getattr(self.runtime, '_child_pass_fds', ()), ())
                    self.assertEqual(self.original.read_bytes(), b'preserved synthetic recovery input')

    def test_invalid_selections_do_not_create_workspaces(self):
        for kind, reference in (('other', self.filename), ('download', '../private'), ('sql', 'production-database')):
            with self.subTest(kind=kind), self.assertRaises(OperationError):
                with restore_workspace(self.runtime, kind, reference):
                    self.fail('invalid workspace admitted')
        self.assertEqual(list(self.runtime.state.glob('.restore-*')), [])

    def test_albums_selection_is_authenticated_and_never_reused_for_other_kinds(self):
        for selection in ({}, dict(snapshot='../private', ledger='albums-deletions-' + '2' * 32 + '.json')):
            with self.assertRaises(OperationError):
                with restore_workspace(self.runtime, 'albums-sql', self.container, **selection):
                    self.fail('invalid selection accepted')
        with self.assertRaises(OperationError):
            with restore_workspace(self.runtime, 'sql', self.container, **self.selection('albums-sql')):
                self.fail('Albums selection accepted by unrelated workspace')
        folder = self.abandoned('albums-sql')
        with (folder / 'client.cnf').open('rb') as lease:
            fcntl.flock(lease, fcntl.LOCK_SH | fcntl.LOCK_NB)
            with patch.object(self.runtime, 'execute') as execute, self.assertRaises(OperationError):
                prune_restore_workspaces(self.runtime)
            execute.assert_not_called()
        with patch.object(self.runtime, 'execute', return_value=b''):
            prune_restore_workspaces(self.runtime)
        self.assertFalse(folder.exists())

    def test_active_owner_or_payload_prevents_any_removal(self):
        for name in ('owner.json', self.filename):
            with self.subTest(name=name):
                folder = self.abandoned()
                before = {p.name: p.read_bytes() for p in folder.iterdir()}
                with (folder / name).open('rb') as lease:
                    fcntl.flock(lease, fcntl.LOCK_SH | fcntl.LOCK_NB)
                    with self.assertRaisesRegex(OperationError, 'active'):
                        prune_restore_workspaces(self.runtime)
                    self.assertEqual({p.name: p.read_bytes() for p in folder.iterdir()}, before)
                prune_restore_workspaces(self.runtime)
                self.assertFalse(folder.exists())

    def test_unknown_tampered_linked_public_and_foreign_files_are_preserved(self):
        for fault in ('unknown', 'tamper', 'symlink', 'hardlink', 'public', 'foreign'):
            with self.subTest(fault=fault):
                folder = self.abandoned()
                payload = folder / self.filename
                if fault == 'unknown':
                    self.write(folder / 'independent-user-file')
                elif fault == 'tamper':
                    (folder / 'owner.json').write_text('{}')
                elif fault == 'public':
                    payload.chmod(0o644)
                elif fault in ('symlink', 'hardlink'):
                    payload.unlink()
                    if fault == 'symlink':
                        payload.symlink_to(self.original)
                    else:
                        os.link(self.original, payload)
                else:
                    metadata = json.loads((folder / 'owner.json').read_text())
                    metadata['owner']['database'] = 'another_application'
                    metadata['hmac_sha256'] = manifest_signature(backup_secret(self.runtime), metadata['owner'])
                    atomic_json(folder / 'owner.json', metadata)
                with patch.object(self.runtime, 'execute') as execute, self.assertRaises(OperationError):
                    prune_restore_workspaces(self.runtime)
                execute.assert_not_called()
                self.assertTrue(payload.exists())
                self.assertEqual(self.original.read_bytes(), b'preserved synthetic recovery input')
                shutil.rmtree(folder)

    def test_interrupted_cleanup_keeps_owner_until_last_and_resumes(self):
        for stage in (1, 3):
            with self.subTest(stage=stage):
                folder = self.abandoned()
                original_unlink = os.unlink
                removed = []
                def interrupt(path, *args, **kwargs):
                    if kwargs.get('dir_fd') is not None:
                        if len(removed) == stage:
                            raise OSError('controlled removal interruption')
                        removed.append(str(path))
                    return original_unlink(path, *args, **kwargs)
                with patch('ops_backup.os.unlink', side_effect=interrupt), self.assertRaises(OperationError):
                    prune_restore_workspaces(self.runtime)
                self.assertTrue((folder / 'owner.json').exists())
                self.assertNotIn('owner.json', removed)
                prune_restore_workspaces(self.runtime)
                self.assertFalse(folder.exists())
                prune_restore_workspaces(self.runtime)

    def test_directory_replacement_cannot_remove_the_replacement(self):
        folder = self.abandoned()
        retired = folder.with_name('preserved-renamed-workspace')
        original_unlink = os.unlink
        switched = False
        def replace(path, *args, **kwargs):
            nonlocal switched
            if kwargs.get('dir_fd') is not None and not switched:
                switched = True
                folder.rename(retired)
                folder.mkdir(mode=0o700)
                self.write(folder / str(path), b'independent replacement file')
            return original_unlink(path, *args, **kwargs)
        with patch('ops_backup.os.unlink', side_effect=replace), self.assertRaises(OperationError):
            prune_restore_workspaces(self.runtime)
        self.assertEqual([p.read_bytes() for p in folder.iterdir()], [b'independent replacement file'])
        self.assertTrue(retired.exists())

    def test_late_unknown_file_preserves_the_owner_and_independent_input(self):
        folder = self.abandoned()
        original_unlink = os.unlink
        def add_file(path, *args, **kwargs):
            result = original_unlink(path, *args, **kwargs)
            if kwargs.get('dir_fd') is not None:
                self.write(folder / 'independent-late-input', b'preserved late input')
            return result
        with patch('ops_backup.os.unlink', side_effect=add_file), self.assertRaises(OperationError):
            prune_restore_workspaces(self.runtime)
        self.assertTrue((folder / 'owner.json').exists())
        self.assertEqual((folder / 'independent-late-input').read_bytes(), b'preserved late input')

    def docker(self, folder, fault=None):
        owner = json.loads((folder / 'owner.json').read_text())['owner']
        state = {'present': True, 'removals': 0}
        item = {'Name': '/' + self.container, 'Id': 'a' * 64, 'Config': {'Labels': {
            'cn.tellyouwhat.operation': 'restore-drill', 'cn.tellyouwhat.restore-workspace': owner['directory']}}}
        if fault == 'foreign-name': item['Name'] = '/independent-database'
        if fault == 'foreign-label': item['Config']['Labels']['cn.tellyouwhat.restore-workspace'] = 'another-workspace'
        if fault == 'foreign-operation': item['Config']['Labels']['cn.tellyouwhat.operation'] = 'production'
        def execute(label, command, **kwargs):
            if label == 'restore-workspace-list':
                self.assertIn('--no-trunc', command)
                if fault == 'inventory': raise OperationError('controlled inventory unavailable')
                return (('a' * 64 + '\n') if state['present'] else '').encode()
            if label == 'restore-workspace-inspect':
                return json.dumps([item]).encode()
            self.assertEqual(label, 'restore-workspace-cleanup')
            self.assertEqual(command, ['docker', 'rm', '--force', 'a' * 64])
            state['removals'] += 1
            if fault == 'delete-failed': raise OperationError('controlled removal failure')
            state['present'] = fault == 'still-present'
            if fault == 'lost-response': raise OperationError('controlled lost removal response')
            if fault == 'changed-payload': self.write(folder / 'snapshot.sql.gz', b'changed temporary data')
            return b''
        return state, execute

    def test_orphaned_container_requires_exact_ownership_and_positive_termination(self):
        for kind in ('sql', 'albums-sql'):
            self.check_orphaned_container(kind)

    def check_orphaned_container(self, kind):
        for fault in (None, 'foreign-name', 'foreign-label', 'foreign-operation', 'inventory', 'delete-failed', 'still-present', 'changed-payload'):
            with self.subTest(fault=fault, kind=kind):
                folder = self.abandoned(kind)
                state, execute = self.docker(folder, fault)
                with patch.object(self.runtime, 'execute', side_effect=execute):
                    if fault is None:
                        prune_restore_workspaces(self.runtime)
                        self.assertFalse(folder.exists())
                        self.assertEqual(state['removals'], 1)
                    else:
                        with self.assertRaises(OperationError): prune_restore_workspaces(self.runtime)
                        self.assertTrue((folder / 'owner.json').exists())
                        self.assertTrue((folder / 'snapshot.sql.gz').exists())
                        if fault.startswith('foreign') or fault == 'inventory': self.assertEqual(state['removals'], 0)
                if folder.exists(): shutil.rmtree(folder)

    def test_lost_container_removal_response_resumes_without_another_delete(self):
        folder = self.abandoned('sql')
        state, execute = self.docker(folder, 'lost-response')
        with patch.object(self.runtime, 'execute', side_effect=execute):
            with self.assertRaises(OperationError): prune_restore_workspaces(self.runtime)
            self.assertTrue((folder / 'owner.json').exists())
            self.assertFalse(state['present'])
            prune_restore_workspaces(self.runtime)
            self.assertFalse(folder.exists())
            self.assertEqual(state['removals'], 1)

    def wait_ready(self, process, marker):
        deadline = time.monotonic() + 8
        while not marker.exists() and process.poll() is None and time.monotonic() < deadline:
            time.sleep(.02)
        self.assertTrue(marker.exists(), 'owned child did not reach its checkpoint')
        self.assertIsNone(process.poll())

    def stop_process(self, process):
        if process.poll() is None: process.kill()
        process.communicate(timeout=10)

    def test_sigkill_and_independent_process_recovery(self):
        marker = self.root / 'ready'
        program = '''
import os,sys,time
from pathlib import Path
from ops_backup import restore_workspace
from ops_common import Runtime
root=Path(sys.argv[1]);r=Runtime(root,root/'backups')
with restore_workspace(r,'download',sys.argv[2]) as folder:
 p=folder/sys.argv[2];p.touch(mode=0o600);p.write_bytes(b'synthetic partial download')
 (root/'ready').write_text(str(folder))
 while True:time.sleep(.02)
'''
        process = subprocess.Popen([sys.executable, '-B', '-c', program, str(self.root), self.filename],
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.stop_process, process)
        self.wait_ready(process, marker)
        folder = Path(marker.read_text())
        process.kill(); process.communicate(timeout=10)
        self.assertEqual(process.returncode, -9)
        recovered = subprocess.run([sys.executable, '-B', '-c',
            "import sys;from pathlib import Path;from ops_common import Runtime;from ops_backup import prune_restore_workspaces;"
            "r=Path(sys.argv[1]);prune_restore_workspaces(Runtime(r,r/'backups'))", str(self.root)], capture_output=True, timeout=10)
        self.assertEqual(recovered.returncode, 0, recovered.stderr)
        self.assertFalse(folder.exists())
        self.assertEqual(self.original.read_bytes(), b'preserved synthetic recovery input')

    def test_descendant_retains_the_lease_after_parent_sigkill(self):
        marker, stop = self.root / 'reader-ready', self.root / 'reader-stop'
        reader = """
import os,pathlib,sys,time
folder=pathlib.Path(sys.argv[1]);ready=pathlib.Path(sys.argv[2]);stop=pathlib.Path(sys.argv[3])
ready.write_text(str(folder));deadline=time.monotonic()+5
while not stop.exists() and time.monotonic()<deadline:time.sleep(.01)
"""
        parent = """
import sys
from pathlib import Path
from ops_common import Runtime
from ops_backup import restore_workspace
root=Path(sys.argv[1]);r=Runtime(root,root/'backups')
with restore_workspace(r,'download',sys.argv[2]) as folder:
 p=folder/sys.argv[2];p.touch(mode=0o600);p.write_bytes(b'synthetic active download')
 r.execute('owned-reader',[sys.executable,'-B','-c',sys.argv[3],str(folder),str(root/'reader-ready'),str(root/'reader-stop')])
"""
        process = subprocess.Popen([sys.executable, '-B', '-c', parent, str(self.root), self.filename, reader],
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.stop_process, process)
        self.addCleanup(stop.touch)
        try:
            self.wait_ready(process, marker)
            folder = Path(marker.read_text())
            process.kill(); process.communicate(timeout=10)
            with self.assertRaisesRegex(OperationError, 'active'):
                prune_restore_workspaces(self.runtime)
            self.assertEqual((folder / self.filename).read_bytes(), b'synthetic active download')
        finally:
            stop.touch()
        deadline = time.monotonic() + 8
        while folder.exists() and time.monotonic() < deadline:
            try:
                prune_restore_workspaces(self.runtime)
            except OperationError as error:
                if 'active' not in str(error): raise
                time.sleep(.02)
        self.assertFalse(folder.exists())

    def test_restore_entry_uses_owned_private_files_and_preserves_its_source(self):
        plain = self.root / 'synthetic-input.gz'
        sql = b'CREATE TABLE apps(id INT);\nINSERT INTO apps VALUES(1);\n'
        with gzip.open(plain, 'wb') as output: output.write(sql)
        source = self.runtime.backups / self.filename
        crypt(self.runtime, plain, source)
        encrypted = source.read_bytes()
        manifest = {'filename': source.name, 'database': 'restore_fixture', 'created_at': int(time.time()),
                    'sha256': file_digest(source), 'table_rows': {'apps': 1}, 'image': 'mysql:8.4'}
        atomic_json(str(source) + '.json', {'manifest': manifest, 'hmac_sha256': manifest_signature(backup_secret(self.runtime), manifest)})
        execute, run, popen = self.runtime.execute, subprocess.run, subprocess.Popen
        for cancel in (False, True):
            with self.subTest(cancel=cancel):
                imported = []
                state = {'present': False, 'removed': 0, 'container': None, 'folder': None}
                class Importer:
                    def __init__(self): self.stdin, self.finished, self.waits = io.BytesIO(), False, 0
                    def wait(self, timeout=None):
                        self.waits += 1
                        if cancel and self.waits == 1: raise KeyboardInterrupt()
                        self.finished = True
                        return 0
                    def poll(self): return 0 if self.finished else None
                    def kill(self): self.finished = True
                importer = Importer()
                def start(command, **kwargs):
                    if command[0] != 'docker': return popen(command, **kwargs)
                    self.assertEqual(kwargs['pass_fds'], self.runtime._child_pass_fds)
                    importer.stdin.close = lambda: imported.append(importer.stdin.getvalue())
                    return importer
                def ready(command, **kwargs):
                    if command[0] != 'docker': return run(command, **kwargs)
                    self.assertEqual(kwargs['pass_fds'], self.runtime._child_pass_fds)
                    return subprocess.CompletedProcess(command, 0, b'1', b'')
                def docker(label, command, **kwargs):
                    if label == 'backup-crypto': return execute(label, command, **kwargs)
                    if label == 'restore-start':
                        folders = list(self.runtime.state.glob('.restore-sql-*'))
                        self.assertEqual(len(folders), 1)
                        self.assertIn('cn.tellyouwhat.restore-workspace=' + folders[0].name, command)
                        self.assertEqual((folders[0] / 'snapshot.sql.gz').stat().st_mode & 0o777, 0o600)
                        state.update(present=True, container=command[command.index('--name') + 1], folder=folders[0].name)
                    elif label == 'restore-tables': return b'apps\n'
                    elif label == 'restore-count': return b'1\n'
                    elif label == 'restore-workspace-inspect':
                        return json.dumps([{'Name': '/' + state['container'], 'Id': 'a' * 64, 'Config': {'Labels': {
                            'cn.tellyouwhat.operation': 'restore-drill', 'cn.tellyouwhat.restore-workspace': state['folder']}}}]).encode()
                    elif label == 'restore-workspace-cleanup':
                        self.assertEqual(command, ['docker', 'rm', '--force', 'a' * 64])
                        state.update(present=False, removed=state['removed'] + 1)
                    else:
                        self.assertEqual(label, 'restore-workspace-list')
                        return ('a' * 64 + '\n').encode() if state['present'] else b''
                    return b''
                with patch.object(self.runtime, 'execute', side_effect=docker), \
                        patch('ops_backup.subprocess.run', side_effect=ready), patch('ops_backup.subprocess.Popen', side_effect=start):
                    if cancel:
                        with self.assertRaises(KeyboardInterrupt): restore_drill(self.runtime, self.filename)
                    else:
                        self.assertEqual(restore_drill(self.runtime, self.filename)['tables'], 1)
                self.assertEqual(imported, [sql])
                io.BytesIO.close(importer.stdin)
                self.assertEqual(state['removed'], 1)
                self.assertEqual(source.read_bytes(), encrypted)
                self.assertEqual(list(self.runtime.state.glob('.restore-*')), [])


if __name__ == '__main__':
    unittest.main()
