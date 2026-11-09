"""Opt-in snapshot round trips against disposable socket-only MySQL databases."""
import os
import copy
import datetime
import json
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

import backup
import offsite
import deletion_ledger
import asset_deletion_ledger
import collection_deletion_ledger
from ops_common import OperationError, atomic_json
from ops_backup import file_digest


@unittest.skipUnless(os.getenv('ALBUM_BACKUP_MYSQL_BIN'), 'explicit disposable MySQL backup fixture required')
class AlbumsBackupRoundTripTests(unittest.TestCase):
    def test_previous_and_current_snapshot_restore_every_recorded_table(self):
        binaries = Path(os.environ['ALBUM_BACKUP_MYSQL_BIN'])
        with tempfile.TemporaryDirectory(prefix='album-snapshot-', dir='/tmp') as directory:
            root = Path(directory); root.chmod(0o700)
            data = root / 'data'; data.mkdir(mode=0o700)
            sock = root / 'mysql.sock'
            with (root / 'mysql.log').open('wb') as log:
                subprocess.run([str(binaries / 'mysqld'), '--no-defaults', '--initialize-insecure', '--datadir=' + str(data)],
                               stdout=log, stderr=log, check=True, timeout=120)
                server = subprocess.Popen([str(binaries / 'mysqld'), '--no-defaults', '--datadir=' + str(data),
                    '--socket=' + str(sock), '--skip-networking', '--mysqlx=OFF', '--performance-schema=OFF',
                    '--innodb-buffer-pool-size=67108864', '--pid-file=' + str(root / 'mysql.pid')], stdout=log, stderr=log)
                try:
                    sql = [str(binaries / 'mysql'), '--no-defaults', '--protocol=socket', '--socket=' + str(sock), '--user=root']
                    for _ in range(300):
                        self.assertIsNone(server.poll(), 'owned fixture server exited')
                        if subprocess.run(sql + ['--execute=SELECT 1'], capture_output=True, timeout=5).returncode == 0:
                            break
                        time.sleep(0.1)
                    else:
                        self.fail('owned fixture database was not ready')
                    subprocess.run(sql + ['--execute=CREATE DATABASE albums_test'], check=True, capture_output=True)
                    sql += ['--database=albums_test']
                    subprocess.run(sql, input=b"CREATE TABLE schema_migrations(name VARCHAR(255) PRIMARY KEY);", check=True, capture_output=True)
                    migrations = Path(__file__).resolve().parents[2] / 'migrations'
                    for path in sorted(migrations.glob('*.sql')):
                        if path.name[:4] < '0074':
                            subprocess.run(sql, input=path.read_bytes(), check=True, capture_output=True, timeout=20)
                    subprocess.run(sql, input=b"INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES ('00000000-0000-0000-0000-000000000001',REPEAT('a',64));", check=True, capture_output=True)
                    settings = root / 'backup.env'
                    settings.write_text('MYSQL_DATABASE=albums_test\nMYSQL_SOCKET=' + str(sock) + '\nMYSQL_USER=root\nALBUM_BACKUP_ENCRYPTION_KEY=' + os.urandom(32).hex() + '\n')
                    settings.chmod(0o600)
                    runtime = backup.Runtime(root, root / 'vault', settings)
                    previous = backup.create(runtime, binaries)
                    self.assertNotIn('album_system_copy_locators', backup.verify(runtime, runtime.backups / previous['filename'])['table_rows'])
                    old_result = backup.drill(runtime, binaries, previous['filename'])
                    self.assertEqual(old_result['tables'], len(backup.PRE_COPY_TABLES))
                    self.assertTrue(old_result['quarantined'])
                    subprocess.run(sql, input=(migrations / '0074_album_system_copy_locators.sql').read_bytes(), check=True, capture_output=True)
                    subprocess.run(sql, input=b"""
INSERT INTO album_storage_accounts(owner_id) VALUES ('00000000-0000-0000-0000-000000000001');
INSERT INTO album_asset_versions(owner_id,asset_id,version_id,manifest_digest,manifest_json,sealed_objects,original_bytes,published_at)
VALUES ('00000000-0000-0000-0000-000000000001','synthetic','00000000-0000-0000-0000-000000000002',REPEAT('b',64),'{}','[]',1,UTC_TIMESTAMP(6));
INSERT INTO album_system_copy_locators VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',REPEAT('c',64),'daily_jpeg_v1',X'00FF416C62756D73');
UPDATE album_storage_accounts SET used_bytes=11,reserved_bytes=4 WHERE owner_id='00000000-0000-0000-0000-000000000001';
INSERT INTO album_asset_lifecycle(owner_id,asset_id,revision,state) VALUES ('00000000-0000-0000-0000-000000000001','synthetic',1,'active');
INSERT INTO album_library_changes(owner_id,asset_id,version_id,revision,state,changed_at) VALUES ('00000000-0000-0000-0000-000000000001','synthetic','00000000-0000-0000-0000-000000000002',1,'active',UTC_TIMESTAMP(6));
INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES ('00000000-0000-0000-0000-000000000010',REPEAT('d',64));
INSERT INTO album_storage_accounts(owner_id,used_bytes) VALUES ('00000000-0000-0000-0000-000000000010',7);
INSERT INTO album_asset_versions(owner_id,asset_id,version_id,manifest_digest,manifest_json,sealed_objects,original_bytes,published_at)
VALUES ('00000000-0000-0000-0000-000000000010','synthetic','00000000-0000-0000-0000-000000000011',REPEAT('e',64),'{}','[]',7,UTC_TIMESTAMP(6));
INSERT INTO album_asset_lifecycle(owner_id,asset_id,revision,state) VALUES ('00000000-0000-0000-0000-000000000010','synthetic',1,'active');
""", check=True, capture_output=True)
                    subprocess.run(sql, input=b"""
INSERT INTO album_asset_lifecycle(owner_id,asset_id,revision,state) VALUES ('00000000-0000-0000-0000-000000000010','legacy-purged',3,'purged');
INSERT INTO album_library_changes(owner_id,asset_id,version_id,revision,state,changed_at) VALUES ('00000000-0000-0000-0000-000000000010','legacy-purged','00000000-0000-0000-0000-000000000012',3,'purged','2026-10-01 00:00:00');
INSERT INTO album_object_purges(owner_id,asset_id,revision,trash_expires_at,targets_json,state,available_at,updated_at,refunded_bytes,completed_at) VALUES ('00000000-0000-0000-0000-000000000010','legacy-purged',2,'2026-10-01 00:00:00','[]','completed','2026-10-01 00:00:00','2026-10-01 00:00:00',5,'2026-10-01 00:00:00');
""", check=True, capture_output=True)
                    legacy_rows = asset_deletion_ledger.export(runtime, sql + ['--batch', '--skip-column-names'], {})
                    self.assertEqual(legacy_rows, [['00000000-0000-0000-0000-000000000010', 'legacy-purged', 3, 'trash', '2026-10-01T00:00:00.000000Z', ['00000000-0000-0000-0000-000000000012']]])
                    # Missing final-feed evidence must block the upgrade checkpoint, never become an empty fence.
                    subprocess.run(sql, input=b"UPDATE album_library_changes SET revision=4 WHERE asset_id='legacy-purged';", check=True, capture_output=True)
                    with self.assertRaises(OperationError):
                        asset_deletion_ledger.export(runtime, sql + ['--batch', '--skip-column-names'], {})
                    subprocess.run(sql, input=b"UPDATE album_library_changes SET revision=3 WHERE asset_id='legacy-purged';", check=True, capture_output=True)
                    before_corrections = backup.create(runtime, binaries)
                    legacy_checkpoint = deletion_ledger.publish(runtime, sql + ['--batch', '--skip-column-names'], {})
                    self.assertEqual(deletion_ledger.verify_checkpoint(runtime, runtime.backups / legacy_checkpoint['filename'], legacy_checkpoint['sha256'], 0)['assets'], legacy_rows)
                    before_result = backup.drill(runtime, binaries, before_corrections['filename'], runtime.backups / legacy_checkpoint['filename'], legacy_checkpoint['sha256'])
                    self.assertEqual(before_result['tables'], len(backup.PRE_CORRECTION_TABLES))
                    subprocess.run(sql, input=(migrations / '0075_album_content_corrections.sql').read_bytes(), check=True, capture_output=True)
                    subprocess.run(sql, input=b"""
INSERT INTO album_content_corrections(owner_id,version_id,revision,request_id,correction_json,updated_at)
VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',1,
 '00000000-0000-0000-0000-000000000003','{"included":["dogs"],"excluded":[]}',UTC_TIMESTAMP(6));
""", check=True, capture_output=True)
                    correction_snapshot = backup.create(runtime, binaries)
                    self.assertEqual(backup.drill(runtime, binaries, correction_snapshot['filename'])['tables'], len(backup.PRE_FAVORITE_TABLES))
                    subprocess.run(sql, input=(migrations / '0076_album_asset_favorites.sql').read_bytes(), check=True, capture_output=True)
                    favorite_snapshot = backup.create(runtime, binaries)
                    self.assertEqual(backup.drill(runtime, binaries, favorite_snapshot['filename'])['tables'], len(backup.PRE_COLLECTION_TABLES))
                    subprocess.run(sql, input=(migrations / '0077_album_collections.sql').read_bytes(), check=True, capture_output=True)
                    subprocess.run(sql,input=b"""
INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at)
VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000020',1,'00000000-0000-0000-0000-000000000021','owned group',FALSE,UTC_TIMESTAMP(6)),
('00000000-0000-0000-0000-000000000010','00000000-0000-0000-0000-000000000020',1,'00000000-0000-0000-0000-000000000021','foreign group',FALSE,UTC_TIMESTAMP(6));
INSERT INTO album_collection_members(owner_id,collection_id,asset_id,revision,request_id,present,updated_at)
VALUES ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000020','synthetic',1,'00000000-0000-0000-0000-000000000022',TRUE,UTC_TIMESTAMP(6)),
('00000000-0000-0000-0000-000000000010','00000000-0000-0000-0000-000000000020','synthetic',1,'00000000-0000-0000-0000-000000000022',TRUE,UTC_TIMESTAMP(6));
""",check=True,capture_output=True)
                    before_asset_fences = backup.create(runtime, binaries)
                    self.assertEqual(backup.drill(runtime, binaries, before_asset_fences['filename'])['tables'], len(backup.PRE_ASSET_DELETION_TABLES))
                    subprocess.run(sql, input=(migrations / '0078_album_asset_deletion_ledger.sql').read_bytes(), check=True, capture_output=True)
                    self.assertEqual(asset_deletion_ledger.export(runtime, sql + ['--batch', '--skip-column-names'], {}), legacy_rows)
                    subprocess.run(sql, input=b"INSERT INTO schema_migrations(name) VALUES('0078_album_asset_deletion_ledger.sql');", check=True, capture_output=True)
                    # Independent fence survives even when its account and media rows are absent.
                    subprocess.run(sql, input=b"""
INSERT INTO album_asset_deletion_ledger(owner_id,asset_id,final_revision,reason,purged_at,version_ids)
VALUES ('10000000-0000-4000-8000-000000000009','purged-backup-asset',3,'retention',UTC_TIMESTAMP(6),JSON_ARRAY('20000000-0000-4000-8000-000000000009'));
""", check=True, capture_output=True)
                    subprocess.run(sql, input=b"""
INSERT INTO album_photo_writes(attempt_id,owner_id,version_id,manifest_digest,preset,size_bytes,sha256,pixel_width,pixel_height,object_version,created_at,updated_at)
VALUES ('00000000-0000-0000-0000-000000000030','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',REPEAT('b',64),'grid_jpeg_v1',1,REPEAT('f',64),1,1,'legacy-photo-version',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6));
""", check=True, capture_output=True)
                    subprocess.run(sql, input=(migrations / '0080_album_photo_multipart.sql').read_bytes(), check=True, capture_output=True)
                    subprocess.run(sql, input=b"""
INSERT INTO album_photo_writes(attempt_id,owner_id,version_id,manifest_digest,preset,size_bytes,sha256,pixel_width,pixel_height,created_at,updated_at,multipart,provider_upload_id)
VALUES ('00000000-0000-0000-0000-000000000031','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',REPEAT('b',64),'daily_jpeg_v1',2,REPEAT('e',64),1,1,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),TRUE,'known-photo-session');
""", check=True, capture_output=True)
                    subprocess.run(sql, input=(migrations / '0081_album_photo_completion_fence.sql').read_bytes(), check=True, capture_output=True)
                    subprocess.run(sql, input=b"""
INSERT INTO album_photo_writes(attempt_id,owner_id,version_id,manifest_digest,preset,size_bytes,sha256,pixel_width,pixel_height,created_at,updated_at,multipart,provider_upload_id,multipart_phase,retirement_started_at,object_version)
VALUES ('00000000-0000-0000-0000-000000000032','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',REPEAT('b',64),'grid_png_v1',2,REPEAT('e',64),1,1,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),TRUE,'aborted-photo-session','aborted',UTC_TIMESTAMP(6),NULL),
('00000000-0000-0000-0000-000000000033','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',REPEAT('b',64),'daily_png_v1',2,REPEAT('e',64),1,1,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),TRUE,'completed-photo-session','completing',NULL,'completed-photo-version');
""", check=True, capture_output=True)
                    current = backup.create(runtime, binaries)
                    manifest = backup.verify(runtime, runtime.backups / current['filename'])
                    self.assertEqual(manifest['table_rows']['album_system_copy_locators'], 1)
                    self.assertEqual(manifest['table_rows']['album_content_corrections'], 1)
                    self.assertEqual(manifest['table_rows']['album_asset_deletion_ledger'], 2)
                    self.assertEqual(manifest['table_rows']['album_photo_writes'], 4)
                    result = backup.drill(runtime, binaries, current['filename'])
                    self.assertEqual(result['tables'], len(backup.TABLES))
                    self.assertTrue(result['quarantined'])
                    self.assertTrue((runtime.backups / previous['filename']).exists())
                    # Exercise real encrypted SQL through the download handoff; object transport stays local.
                    with runtime.lock():
                        ledger = deletion_ledger.publish(runtime, sql + ['--batch', '--skip-column-names'], {})
                        credentials = root / 'controlled-offsite.json'
                        atomic_json(credentials, {'Bucket': 'fixture-1250000000', 'Region': 'ap-guangzhou'})
                        objects = {}
                        def transfer(child, executable, config, operation, path, key, digest, size, **pinned):
                            if operation == 'put': objects[key] = path.read_bytes()
                            else:
                                with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'wb') as output:
                                    output.write(objects[key])
                            result = dict(Key=key, SHA256=digest, Bytes=size, Version='controlled-version')
                            result.update(pinned)
                            return result
                        with patch('offsite.initiate', return_value='controlled-session'), patch('offsite.move', side_effect=transfer):
                            published = offsite.publish(runtime, 'controlled', credentials, current['filename'],
                                                       runtime.backups / ledger['filename'], ledger['sha256'])
                            fetched = offsite.fetch(runtime, 'controlled', credentials, published['receipt'], published['receipt_sha256'])
                        self.assertFalse(fetched['recovery_ready']); self.assertFalse(fetched['latest_checkpoint_proven'])
                        restored = copy.copy(runtime); restored.backups = Path(fetched['directory'])
                        self.assertEqual(file_digest(restored.backups / current['filename']), file_digest(runtime.backups / current['filename']))
                        self.assertEqual(len(list(restored.backups.iterdir())), 3)
                        self.assertEqual(list(runtime.backups.glob('.albums-offsite-*')), [])
                        drilled = backup.drill(restored, binaries, current['filename'], fetched['deletion_ledger'], fetched['deletion_ledger_sha256'])
                        self.assertTrue(drilled['quarantined']); self.assertEqual(drilled['tables'], len(backup.TABLES))
                    probe = os.environ.get('ALBUM_BACKUP_COS_PROBE_DIR')
                    if probe:
                        from offsite_probe import run
                        remote = run(runtime, binaries, current['filename'], sql, probe)
                        self.assertTrue(remote['restored'])
                        self.assertTrue(remote['cleanup_confirmed'])
                    old_time = int(time.time()) - backup.RETENTION - 60
                    requested = datetime.datetime.fromtimestamp(old_time - 1, datetime.timezone.utc).strftime('%Y-%m-%d %H:%M:%S.%f')
                    subprocess.run(sql, input=("INSERT INTO album_deletion_ledger(account_id,request_id,authorizing_session_sha256,requested_at) VALUES ('10000000-0000-4000-8000-000000000001','20000000-0000-4000-8000-000000000001',UNHEX(REPEAT('ab',32)),'" + requested + "');").encode(), check=True, capture_output=True)
                    with runtime.lock():
                        # Record a purge after both old snapshots; their active row must not return.
                        subprocess.run(sql, input=b"""
INSERT INTO album_asset_deletion_ledger(owner_id,asset_id,final_revision,reason,purged_at,version_ids)
VALUES ('00000000-0000-0000-0000-000000000001','synthetic',3,'retention',UTC_TIMESTAMP(6),JSON_ARRAY('00000000-0000-0000-0000-000000000002'));
""", check=True, capture_output=True)
                        subprocess.run(sql, input=b"UPDATE album_collections SET name='',deleted=TRUE,revision=2,request_id='00000000-0000-0000-0000-000000000023',updated_at=UTC_TIMESTAMP(6) WHERE owner_id='00000000-0000-0000-0000-000000000010';", check=True, capture_output=True)
                        active = deletion_ledger.publish(runtime, sql + ['--batch', '--skip-column-names'], {})
                        active_path = runtime.backups / active['filename']
                        envelope = json.loads(active_path.read_text())
                        stamp = datetime.datetime.fromtimestamp(old_time-1,datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S.%fZ')
                        envelope['payload']['exported_at']=old_time
                        for entry in envelope['payload']['entries']: entry[3]=stamp
                        for entry in envelope['payload']['assets'] + envelope['payload']['collections']: entry[4]=stamp
                        envelope['hmac_sha256']=backup.manifest_signature(backup.backup_secret(runtime),envelope['payload'])
                        expired_path=runtime.backups/('albums-deletions-'+'f'*32+'.json')
                        backup.atomic_json(expired_path,envelope)
                        self.assertEqual(backup.prune(runtime), 1)
                        self.assertFalse(expired_path.exists())
                        self.assertTrue(active_path.exists())
                        # Local checkpoint cleanup must not remove the live database fence.
                        count = subprocess.run(sql + ['--batch', '--skip-column-names', '--execute=SELECT COUNT(*) FROM album_deletion_ledger'], check=True, capture_output=True, text=True)
                        self.assertEqual(count.stdout.strip(), '1')
                        original_execute=runtime.execute
                        asset_rows=deletion_ledger.verify_checkpoint(runtime,active_path,active['sha256'],0)['assets']
                        with self.assertRaisesRegex(OperationError,'quarantined'):
                            asset_deletion_ledger.restore_fences(runtime,sql,asset_rows)
                        collection_rows=deletion_ledger.verify_checkpoint(runtime,active_path,active['sha256'],0)['collections']
                        self.assertEqual(len(collection_rows),1)
                        self.assertEqual(collection_rows[0][:4],['00000000-0000-0000-0000-000000000010','00000000-0000-0000-0000-000000000020',2,'00000000-0000-0000-0000-000000000023'])
                        with self.assertRaisesRegex(OperationError,'quarantined'):
                            collection_deletion_ledger.restore_fences(runtime,sql,collection_rows)
                        expected_groups=2

                        def observe(name,args,**kwargs):
                            result=original_execute(name,args,**kwargs)
                            if name=='albums-restore-quarantine':
                                # A second producer invocation must converge without another refund.
                                with patch.object(runtime,'execute',original_execute):
                                    asset_deletion_ledger.restore_fences(runtime,args,asset_rows)
                                values=original_execute('asset-replay-proof',args+["--execute=SELECT (SELECT COUNT(*) FROM album_asset_versions WHERE owner_id='00000000-0000-0000-0000-000000000001'),(SELECT COUNT(*) FROM album_asset_versions WHERE owner_id='00000000-0000-0000-0000-000000000010'),(SELECT COUNT(*) FROM album_asset_lifecycle WHERE owner_id='00000000-0000-0000-0000-000000000001' AND asset_id='synthetic' AND revision=3 AND state='purged'),(SELECT COUNT(*) FROM album_library_changes WHERE owner_id='00000000-0000-0000-0000-000000000001' AND state='purged' AND revision=3),(SELECT CONCAT(used_bytes,',',reserved_bytes) FROM album_storage_accounts WHERE owner_id='00000000-0000-0000-0000-000000000001')"]).decode().strip()
                                self.assertEqual(values,'0\t1\t1\t1\t11,4')
                                tables=original_execute('asset-test-tables',args+['--execute=SHOW TABLES']).decode().splitlines()
                                if 'album_collections' in tables:
                                    groups=original_execute('asset-group-proof',args+["--execute=SELECT (SELECT COUNT(*) FROM album_collection_members WHERE owner_id='00000000-0000-0000-0000-000000000001'),(SELECT COUNT(*) FROM album_collection_members WHERE owner_id='00000000-0000-0000-0000-000000000010'),(SELECT COUNT(*) FROM album_collections)"]).decode().strip()
                                    self.assertEqual(groups,'0\t0\t'+str(expected_groups))
                                with patch.object(runtime,'execute',original_execute):
                                    collection_deletion_ledger.restore_fences(runtime,args,collection_rows)
                                    with self.assertRaises(OperationError):
                                        collection_deletion_ledger.restore_fences(runtime,args,[])
                                grouped=original_execute('collection-replay-proof',args+["--execute=SELECT (SELECT COUNT(*) FROM album_collections WHERE owner_id='00000000-0000-0000-0000-000000000010' AND deleted=TRUE AND name='' AND revision=2 AND request_id='00000000-0000-0000-0000-000000000023'),(SELECT COUNT(*) FROM album_collections WHERE owner_id='00000000-0000-0000-0000-000000000001' AND deleted=FALSE AND name='owned group'),(SELECT COUNT(*) FROM album_asset_versions WHERE owner_id='00000000-0000-0000-0000-000000000010' AND asset_id='synthetic'),(SELECT CONCAT(used_bytes,',',reserved_bytes) FROM album_storage_accounts WHERE owner_id='00000000-0000-0000-0000-000000000010')"]).decode().strip()
                                self.assertEqual(grouped,'1\t'+str(expected_groups-1)+'\t1\t7,0')
                            return result
                        with patch.object(runtime,'execute',side_effect=observe):
                            fenced=backup.drill(runtime,binaries,current['filename'],active_path,active['sha256'])
                            older=backup.drill(runtime,binaries,before_asset_fences['filename'],active_path,active['sha256'])
                            expected_groups=1
                            backup.drill(runtime,binaries,before_corrections['filename'],active_path,active['sha256'])
                        self.assertEqual(older['asset_deletion_intents'],3)
                        self.assertEqual(fenced['collection_deletion_intents'],1)
                        self.assertEqual(older['collection_deletion_intents'],1)
                        self.assertTrue(older['quota_reconciliation_required'])
                        # Authenticated contradictions are refused before removing metadata.
                        for fault in ['missing-receipt','unknown-version','stale-revision']:
                            bad=json.loads(active_path.read_text())
                            row=next(r for r in bad['payload']['assets'] if r[0]=='00000000-0000-0000-0000-000000000001')
                            if fault=='missing-receipt':bad['payload']['assets']=[row]
                            if fault=='unknown-version':row[5]=['00000000-0000-0000-0000-000000000099']
                            if fault=='stale-revision':row[2]=1
                            bad['hmac_sha256']=backup.manifest_signature(backup.backup_secret(runtime),bad['payload'])
                            bad_path=runtime.backups/'asset-conflict.json';backup.atomic_json(bad_path,bad)
                            with self.subTest(fault=fault),self.assertRaises(OperationError):
                                backup.drill(runtime,binaries,current['filename'],bad_path,backup.file_digest(bad_path))
                        def fail_after_removal(name,args,**kwargs):
                            if name=='albums-restore-asset-fences':
                                original_execute('asset-fault',args+["--execute=ALTER TABLE album_library_changes ADD CONSTRAINT reject_replayed_events CHECK(state<>'purged' OR owner_id<>'00000000-0000-0000-0000-000000000001' OR asset_id<>'synthetic')"])
                                with self.assertRaises(OperationError):original_execute(name,args,**kwargs)
                                values=original_execute('asset-rollback-proof',args+["--execute=SELECT (SELECT COUNT(*) FROM album_asset_versions WHERE owner_id='00000000-0000-0000-0000-000000000001'),(SELECT COUNT(*) FROM album_asset_deletion_ledger WHERE owner_id='00000000-0000-0000-0000-000000000001'),(SELECT COUNT(*) FROM album_system_copy_locators),(SELECT CONCAT(used_bytes,',',reserved_bytes) FROM album_storage_accounts WHERE owner_id='00000000-0000-0000-0000-000000000001')"]).decode().strip()
                                self.assertEqual(values,'1\t0\t1\t11,4')
                                raise OperationError('injected feed failure; rollback verified')
                            return original_execute(name,args,**kwargs)
                        with patch.object(runtime,'execute',side_effect=fail_after_removal),self.assertRaisesRegex(OperationError,'rollback verified'):
                            backup.drill(runtime,binaries,current['filename'],active_path,active['sha256'])
                        def fail_collection_insert(name,args,**kwargs):
                            if name=='albums-restore-collection-fences':
                                original_execute('collection-fault',args+["--execute=CREATE TRIGGER reject_restored_group BEFORE INSERT ON album_collections FOR EACH ROW SET NEW.revision=IF(NEW.owner_id='00000000-0000-0000-0000-000000000010' AND NEW.deleted,0,NEW.revision)"])
                                with self.assertRaises(OperationError):original_execute(name,args,**kwargs)
                                unchanged=original_execute('collection-rollback-proof',args+["--execute=SELECT (SELECT COUNT(*) FROM album_collections WHERE owner_id='00000000-0000-0000-0000-000000000010' AND name='foreign group' AND deleted=FALSE AND revision=1),(SELECT COUNT(*) FROM album_collection_members WHERE owner_id='00000000-0000-0000-0000-000000000010'),(SELECT COUNT(*) FROM album_asset_versions WHERE owner_id='00000000-0000-0000-0000-000000000010')"]).decode().strip()
                                self.assertEqual(unchanged,'1\t1\t1')
                                raise OperationError('collection insertion failure; rollback verified')
                            return original_execute(name,args,**kwargs)
                        with patch.object(runtime,'execute',side_effect=fail_collection_insert),self.assertRaisesRegex(OperationError,'rollback verified'):
                            backup.drill(runtime,binaries,current['filename'],active_path,active['sha256'])
                        self.assertTrue(fenced['deletion_ledger_applied'])
                        self.assertEqual(fenced['deletion_intents'], 1)
                        self.assertTrue(fenced['quarantined'])
                    subprocess.run(sql, input=b"RENAME TABLE album_asset_deletion_ledger TO owned_hidden_asset_ledger;", check=True, capture_output=True)
                    try:
                        with self.assertRaisesRegex(OperationError, 'schema'):
                            asset_deletion_ledger.export(runtime, sql + ['--batch', '--skip-column-names'], {})
                    finally:
                        subprocess.run(sql, input=b"RENAME TABLE owned_hidden_asset_ledger TO album_asset_deletion_ledger;", check=True, capture_output=True)
                    self.assertFalse([p for p in runtime.state.glob('albums-restore-*') if p.is_dir()])
                finally:
                    if server.poll() is None:
                        server.terminate()
                    try:
                        server.wait(timeout=30)
                    except subprocess.TimeoutExpired:
                        server.kill(); server.wait(timeout=10)


if __name__ == '__main__':
    unittest.main()
