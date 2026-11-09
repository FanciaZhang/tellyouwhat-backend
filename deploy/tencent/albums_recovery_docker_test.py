"""Opt-in, synthetic Albums recovery on an existing immutable MySQL image.

On the shared host, invoke only through its registered producer. A stopped run
can be resumed with the same ALBUM_RECOVERY_WORK_ROOT to reclaim its own files.
"""
import copy
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tarfile
import time
import unittest

import albums_artifacts
import albums_recovery
from ops_backup import prune_restore_workspaces, restore_workspace
from ops_common import OperationError, Runtime, atomic_json


@unittest.skipUnless(os.getenv('ALBUM_RECOVERY_DOCKER_IMAGE'), 'explicit immutable Docker fixture required')
class AlbumsDockerRecoveryTests(unittest.TestCase):
    def test_real_container_recovery_and_owned_cleanup(self):
        image = os.environ['ALBUM_RECOVERY_DOCKER_IMAGE']
        self.assertRegex(image, r'^(?:mysql@)?sha256:[a-f0-9]{64}$')
        base = Path(os.environ['ALBUM_RECOVERY_WORK_ROOT']).resolve()
        self.assertTrue(base.is_dir()); self.assertEqual(base.stat().st_uid, os.getuid())
        folder = base / 'synthetic-albums-recovery'
        if folder.is_symlink(): raise OperationError('fixture path is a symlink; retained')
        folder.mkdir(mode=0o700, exist_ok=True)
        self.assertEqual(folder.stat().st_mode & 0o777, 0o700)
        self.assertEqual(folder.stat().st_uid, os.getuid())
        marker = folder / 'fixture-owner.json'
        owner = {'scope': 'albums-synthetic-docker-recovery-v1', 'uid': os.getuid()}
        if marker.exists(): self.assertEqual(json.loads(marker.read_text()), owner)
        else:
            self.assertEqual(list(folder.iterdir()), [])
            atomic_json(marker, owner)
        lease = folder / 'fixture.lock'; lease.touch(mode=0o600, exist_ok=True)
        with lease.open('rb') as handle:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            platform = folder / 'platform'; platform.mkdir(mode=0o700, exist_ok=True)
            env = platform / '.env.production'
            if not env.exists():
                env.touch(mode=0o600)
                env.write_text('MYSQL_DATABASE=albums_recovery_fixture\nBACKUP_ENCRYPTION_KEY=' + secrets.token_hex(32) + '\n')
            runtime = Runtime(platform, platform / 'backups')
            self.assertEqual(runtime.config['MYSQL_DATABASE'], 'albums_recovery_fixture')
            # A killed preceding invocation retains the original ownership key.
            prune_restore_workspaces(runtime)
            self.assertTrue({p.name for p in folder.iterdir()} <= {
                'fixture-owner.json', 'fixture.lock', 'platform', 'albums-development', 'albums-production', 'clients'})
            for name in ('albums-development', 'albums-production', 'clients'):
                path = folder / name
                if path.is_dir() and not path.is_symlink(): shutil.rmtree(path)
                elif path.exists() or path.is_symlink(): raise OperationError('unexpected fixture path; retained')
            evidence = []
            try:
                self.exercise(runtime, folder, image, evidence)
            finally:
                # Failure to confirm closure raises before removing any fixture data.
                prune_restore_workspaces(runtime)
                self.assertEqual(list(runtime.state.glob('.restore-*')), [])
                atomic_json(base / 'docker-recovery-result.json', {'checks': evidence,
                    'real_docker': True, 'synthetic_only': True, 'owned_workspaces_removed': True,
                    'service_deployed': False, 'latest_checkpoint_proven': False, 'recovery_ready': False})
                for child in folder.iterdir():
                    if child in (lease, marker): continue
                    if child.is_dir() and not child.is_symlink(): shutil.rmtree(child)
                    else: child.unlink()
            # Keep the lock inode until after all fixture data has been reclaimed.
        lease.unlink(); marker.unlink(); folder.rmdir()

    def exercise(self, runtime, folder, image, evidence):
        with albums_recovery._implementation() as modules:
            backup, ledger = modules['backup'], modules['deletion_ledger']
            for environment in ('development', 'production'):
                print('fixture phase: ' + environment, flush=True)
                database, user = albums_recovery.database_scope(environment)
                app = folder / ('albums-' + environment); app.mkdir(mode=0o700, exist_ok=True)
                vault = app / 'backups'; vault.mkdir(mode=0o700, exist_ok=True)
                saved = ('MYSQL_HOST=localhost\nMYSQL_PORT=3306\nMYSQL_DATABASE=' + database + '\nMYSQL_USER=' + user +
                         '\nMYSQL_PASSWORD=' + secrets.token_hex(32) + '\n')
                config = app / 'backup.env'; config.touch(mode=0o600, exist_ok=True)
                config.write_text(saved + 'ALBUM_BACKUP_ENCRYPTION_KEY=' + secrets.token_hex(32) + '\n')
                child = backup.Runtime(runtime.root, vault, config); backup.configure(child)
                snapshot, checkpoint = self.seed(runtime, child, backup, ledger, folder, image, database)
                files = {n: b'controlled configuration, not a runnable service' for n in (
                    'state.json', 'owner.json', 'database.env', *('current/' + n for n in
                    (albums_artifacts.PRODUCTION_FILES if environment == 'production' else albums_artifacts.FILES)),
                    *('secrets/' + n for n in albums_artifacts.KEYS))}
                if environment == 'production':
                    files['current/runtime.env'] = (b'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums\n'
                        b'ALBUM_API_HOST=api.albums.tellyouwhat.cn\nALBUM_COS_BUCKET=tellyouwhat-albums-production-1251995834\n')
                files['backup.env'] = saved.encode()
                for name in (snapshot['filename'], snapshot['filename'] + '.json', checkpoint['filename']):
                    files['checkpoint/' + name] = (vault / name).read_bytes()
                before = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in vault.iterdir() if p.is_file()}
                for fault in (None, 'row-hash', 'cancel', 'lost-start'):
                    print('restore phase: ' + environment + '/' + str(fault), flush=True)
                    selected = files.copy()
                    if fault == 'row-hash':
                        envelope = json.loads(selected['checkpoint/' + snapshot['filename'] + '.json'])
                        envelope['manifest']['table_sha256']['album_accounts'] = '0' * 64
                        envelope['hmac_sha256'] = backup.manifest_signature(backup.backup_secret(child), envelope['manifest'])
                        selected['checkpoint/' + snapshot['filename'] + '.json'] = json.dumps(envelope).encode()
                    packed = io.BytesIO()
                    with tarfile.open(fileobj=packed, mode='w') as archive:
                        for name, data in selected.items():
                            member = tarfile.TarInfo('recovery/albums-' + environment + '/' + name)
                            member.size = len(data); archive.addfile(member, io.BytesIO(data))
                    packed.seek(0)
                    original = runtime.execute
                    proof = []
                    def observe(label, command, **kwargs):
                        result = original(label, command, **kwargs)
                        if label == 'albums-runtime-restore-start':
                            identity = result.decode().strip(); self.assertRegex(identity, r'^[a-f0-9]{64}$')
                            item = json.loads(original('fixture-inspect', ['docker', 'inspect', identity]))[0]
                            self.assertEqual(item['HostConfig']['NetworkMode'], 'none')
                            self.assertEqual(item['HostConfig']['Memory'], 768 * 1024**2)
                            self.assertEqual(item['HostConfig']['PidsLimit'], 256)
                            self.assertTrue(item['HostConfig']['AutoRemove'])
                            self.assertTrue(all(m['Destination'] == '/run/albums-client.cnf' and not m['RW']
                                                for m in item['Mounts'] if m['Type'] == 'bind'))
                            if fault == 'lost-start': raise OperationError('controlled lost Docker response')
                        if label == 'albums-restore-count' and fault == 'cancel':
                            raise KeyboardInterrupt('controlled cancellation after SQL import')
                        if label == 'albums-restore-quarantine':
                            values = original('fixture-restored-proof', command + ["--execute=SELECT "
                                "(SELECT COUNT(*) FROM album_asset_versions WHERE owner_id='00000000-0000-0000-0000-000000000001'),"
                                "(SELECT COUNT(*) FROM album_asset_versions WHERE owner_id='00000000-0000-0000-0000-000000000010'),"
                                "(SELECT COUNT(*) FROM album_collections WHERE owner_id='00000000-0000-0000-0000-000000000010' AND deleted=TRUE AND name=''),"
                                "(SELECT COUNT(*) FROM album_accounts WHERE account_id='00000000-0000-0000-0000-000000000020' AND state='deleting'),"
                                "(SELECT COUNT(*) FROM album_accounts WHERE state='active'),"
                                "(SELECT COUNT(*) FROM album_recovery_state WHERE quarantined=TRUE)" ])
                            self.assertEqual(values.strip(), b'0\t1\t1\t1\t0\t1'); proof.append(True)
                        return result
                    runtime.execute = observe
                    try:
                        with tarfile.open(fileobj=packed) as archive:
                            args = (runtime, archive, {n: hashlib.sha256(v).hexdigest() for n, v in selected.items()})
                            kwargs = dict(environment=environment, image=image, backup_filename='synthetic-shared.enc')
                            if fault is None:
                                result = albums_recovery.verify_runtime_recovery(*args, **kwargs)
                                self.assertEqual((result['deletion_intents'], result['asset_deletion_intents'], result['collection_deletion_intents']), (1, 1, 1))
                                self.assertTrue(proof); self.assertFalse(result['recovery_ready']); self.assertFalse(result['latest_checkpoint_proven'])
                            else:
                                reason = {'row-hash': 'restored content differs from snapshot',
                                          'cancel': 'controlled cancellation', 'lost-start': 'controlled lost Docker response'}[fault]
                                with self.assertRaisesRegex(KeyboardInterrupt if fault == 'cancel' else OperationError, reason):
                                    albums_recovery.verify_runtime_recovery(*args, **kwargs)
                        self.assertEqual(list(runtime.state.glob('.restore-*')), [])
                        evidence.append({'environment': environment, 'case': fault or 'success', 'passed': True,
                                         'quarantine_proof': bool(proof), 'owned_workspace_removed': True})
                    finally:
                        runtime.execute = original
                self.assertEqual(before, {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in vault.iterdir() if p.is_file()})

    def seed(self, runtime, child, backup, ledger, folder, image, database):
        container = 'tellyouwhat-restore-' + secrets.token_hex(8)
        selection = dict(snapshot='albums-' + '1' * 32 + '.sql.gz.enc', ledger='albums-deletions-' + '2' * 32 + '.json')
        with restore_workspace(runtime, 'albums-sql', container, **selection) as workspace:
            password = secrets.token_hex(32)
            client = workspace / 'client.cnf'; client.touch(mode=0o600)
            client.write_text('[client]\nuser=root\npassword=' + password + '\nprotocol=socket\n')
            runtime.execute('fixture-mysql-start', ['docker', 'run', '--detach', '--rm', '--name', container,
                '--label', 'cn.tellyouwhat.operation=restore-drill', '--label', 'cn.tellyouwhat.restore-workspace=' + workspace.name,
                '--network', 'none', '--memory', '768m', '--pids-limit', '256', '--tmpfs', '/var/lib/mysql:rw,noexec,nosuid,size=768m',
                '--mount', 'type=bind,source=' + str(client) + ',target=/run/albums-client.cnf,readonly', '--env', 'MYSQL_ROOT_PASSWORD',
                image, '--innodb-buffer-pool-size=128M', '--performance-schema=OFF'], env={'MYSQL_ROOT_PASSWORD': password}, timeout=180)
            prefix = ['docker', 'exec', '--interactive', container, 'mysql', '--defaults-extra-file=/run/albums-client.cnf']
            for _ in range(90):
                if subprocess.run(prefix + ['--execute=SELECT 1'], capture_output=True, timeout=10,
                                  pass_fds=getattr(runtime, '_child_pass_fds', ())).returncode == 0: break
                time.sleep(1)
            else: self.fail('owned fixture MySQL not ready')
            runtime.execute('fixture-database', prefix + ['--execute=CREATE DATABASE ' + database])
            sql = prefix + ['--batch', '--skip-column-names', '--database=' + database]
            runtime.execute('fixture-schema-log', sql, input=b'CREATE TABLE schema_migrations(name VARCHAR(255) PRIMARY KEY);')
            migrations = Path(__file__).resolve().parents[2] / 'migrations'
            for path in sorted(migrations.glob('*.sql')):
                if path.name[:4] <= '0082': runtime.execute('fixture-migration', sql, input=path.read_bytes())
            runtime.execute('fixture-seed', sql, input=SEED.encode())
            # Existing backup writers use these scoped Docker clients, never a live service.
            binaries = folder / 'clients'; binaries.mkdir(mode=0o700, exist_ok=True)
            for name in ('mysql', 'mysqldump'):
                path = binaries / name
                probe = ''.join("python3 -c 'import os;os.fstat(" + str(fd) + ")'\n" for fd in getattr(runtime, '_child_pass_fds', ()))
                path.write_text('#!/bin/sh\nset -eu\n' + probe + 'if [ "${1-}" = --no-defaults ]; then shift; fi\nexec docker exec --interactive ' + container + ' ' + name +
                                ' --defaults-extra-file=/run/albums-client.cnf "$@"\n'); path.chmod(0o700)
            source = copy.copy(child); source.config = child.config.copy()
            source._child_pass_fds = getattr(runtime, '_child_pass_fds', ())
            # Wrapper supplies defaults; remove duplicate client connection arguments.
            original = backup.connection; backup.connection = lambda _: []
            try:
                snapshot = backup.create(source, binaries)
            finally:
                backup.connection = original
            runtime.execute('fixture-delete-after-snapshot', sql, input=DELETIONS.encode())
            checkpoint = ledger.publish(source, sql, {})
            return snapshot, checkpoint


SEED = """
INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES
('00000000-0000-0000-0000-000000000001',REPEAT('a',64)),('00000000-0000-0000-0000-000000000010',REPEAT('b',64)),('00000000-0000-0000-0000-000000000020',REPEAT('c',64));
INSERT INTO album_storage_accounts(owner_id,used_bytes) SELECT account_id,11 FROM album_accounts;
INSERT INTO album_asset_versions(owner_id,asset_id,version_id,manifest_digest,manifest_json,sealed_objects,original_bytes,published_at) VALUES
('00000000-0000-0000-0000-000000000001','synthetic','00000000-0000-0000-0000-000000000002',REPEAT('b',64),'{}','[]',1,UTC_TIMESTAMP(6)),
('00000000-0000-0000-0000-000000000010','synthetic','00000000-0000-0000-0000-000000000011',REPEAT('e',64),'{}','[]',7,UTC_TIMESTAMP(6));
INSERT INTO album_asset_lifecycle(owner_id,asset_id,revision,state) SELECT owner_id,asset_id,1,'active' FROM album_asset_versions;
INSERT INTO album_library_changes(owner_id,asset_id,version_id,revision,state,changed_at) SELECT owner_id,asset_id,version_id,1,'active',UTC_TIMESTAMP(6) FROM album_asset_versions;
INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES
('00000000-0000-0000-0000-000000000010','00000000-0000-0000-0000-000000000030',1,'00000000-0000-0000-0000-000000000031','synthetic group',FALSE,UTC_TIMESTAMP(6));
INSERT INTO album_collection_members(owner_id,collection_id,asset_id,revision,request_id,present,updated_at) VALUES
('00000000-0000-0000-0000-000000000010','00000000-0000-0000-0000-000000000030','synthetic',1,'00000000-0000-0000-0000-000000000032',TRUE,UTC_TIMESTAMP(6));
"""
DELETIONS = """
INSERT INTO album_asset_deletion_ledger(owner_id,asset_id,final_revision,reason,purged_at,version_ids) VALUES
('00000000-0000-0000-0000-000000000001','synthetic',3,'retention',UTC_TIMESTAMP(6),JSON_ARRAY('00000000-0000-0000-0000-000000000002'));
INSERT INTO album_deletion_ledger(account_id,request_id,authorizing_session_sha256,requested_at) VALUES
('00000000-0000-0000-0000-000000000020','00000000-0000-0000-0000-000000000021',UNHEX(REPEAT('ab',32)),UTC_TIMESTAMP(6));
UPDATE album_collections SET name='',deleted=TRUE,revision=2,request_id='00000000-0000-0000-0000-000000000033',updated_at=UTC_TIMESTAMP(6);
"""


if __name__ == '__main__':
    unittest.main()
