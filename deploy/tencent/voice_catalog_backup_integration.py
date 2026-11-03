"""Explicit synthetic SQL round trip; never restore over the running test schema."""

import gzip
import json
import os
from pathlib import Path
import secrets
import tempfile
import unittest

from ops_backup import create_backup, crypt, verify_backup
from ops_common import Runtime


class VoiceCatalogBackupIntegrationTests(unittest.TestCase):
    def test_catalog_history_verification_and_unknown_probe_survive_restore(self):
        database = os.environ['BACKUP_TEST_MYSQL_DATABASE']
        self.assertRegex(database, r'^journal_catalog_backup_[a-f0-9]{12}_test$')
        restored = database + '_restored'
        host = os.environ['BACKUP_TEST_MYSQL_HOST']
        self.assertRegex(host, r'^(127\.0\.0\.1|172\.29\.93\.\d{1,3})$')
        with tempfile.TemporaryDirectory(prefix='journal-catalog-backup-') as directory:
            root = Path(directory)
            config = {key: os.environ['BACKUP_TEST_' + key] for key in (
                'MYSQL_HOST', 'MYSQL_PORT', 'MYSQL_USER', 'MYSQL_PASSWORD')}
            config.update(MYSQL_DATABASE=database, BACKUP_ENCRYPTION_KEY=secrets.token_urlsafe(48))
            environment = root / '.env.production'
            environment.write_text(''.join(k + '=' + v + '\n' for k, v in config.items()))
            environment.chmod(0o600)
            runtime = Runtime(root, root / 'backups')
            sql = ['docker', 'run', '--rm', '--interactive', '--network', 'host', '--env', 'MYSQL_PWD',
                   'mysql:8.4', 'mysql', '--host=' + host, '--port=' + config['MYSQL_PORT'],
                   '--user=' + config['MYSQL_USER'], '--default-character-set=utf8mb4', '--batch', '--skip-column-names']
            credentials = {'MYSQL_PWD': config['MYSQL_PASSWORD']}

            def query(label, content, name=None):
                return runtime.execute(label, sql + (['--database=' + name] if name else []),
                                       env=credentials, input=content, timeout=60)

            query('catalog-fixture-create', ('CREATE DATABASE `' + database + '`; CREATE DATABASE `' + restored + '`;').encode())
            try:
                fixture = b'''
                    CREATE TABLE schema_migrations(name VARCHAR(128) PRIMARY KEY);
                    INSERT INTO schema_migrations VALUES ('0069_journal_voice_catalog.sql');
                    CREATE TABLE apps(app_id VARCHAR(64) PRIMARY KEY);
                    INSERT INTO apps VALUES ('journal');
                    CREATE TABLE admin_users(id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY);
                    INSERT INTO admin_users VALUES ('00000000-0000-4000-8000-000000000001');
                ''' + (Path(__file__).resolve().parents[2] / 'migrations/0069_journal_voice_catalog.sql').read_bytes()
                fixture += '''
                    INSERT INTO journal_voice_catalog_revisions VALUES
                    ('00000000-0000-4000-8000-000000000002',JSON_OBJECT('defaultVoiceID','old','voices',JSON_ARRAY(JSON_OBJECT('name','旧目录'))),'','00000000-0000-4000-8000-000000000001',1,1,'2026-10-20','2026-10-20'),
                    ('00000000-0000-4000-8000-000000000003',JSON_OBJECT('defaultVoiceID','current','voices',JSON_ARRAY(JSON_OBJECT('name','恢复后的声音','rank',7))),'00000000-0000-4000-8000-000000000002','00000000-0000-4000-8000-000000000001',1,1,'2026-10-21','2026-10-21');
                    UPDATE journal_voice_catalog_current SET revision_id='00000000-0000-4000-8000-000000000003';
                    INSERT INTO journal_voice_verifications VALUES(REPEAT('a',64),JSON_OBJECT('policySHA256',REPEAT('a',64),'audioSHA256',REPEAT('b',64),'cases',2),'2026-10-21');
                    INSERT INTO journal_voice_catalog_mutations VALUES('00000000-0000-4000-8000-000000000001','original-probe-key','read_aloud.voice.probe',UNHEX(REPEAT('cd',32)),NULL,'unknown','2026-10-21');
                '''.encode()
                private_tables = ('journal_read_aloud_accounts', 'journal_read_aloud_requests',
                                  'journal_simulator_read_aloud_consents', 'privacy_consents')
                for table in private_tables:
                    fixture += ('CREATE TABLE `' + table + '`(id INT PRIMARY KEY,payload TEXT);'
                                'INSERT INTO `' + table + "` VALUES(1,'synthetic-private-journal-body');").encode()
                query('catalog-fixture-seed', fixture, database)
                snapshot = create_backup(runtime)
                path = runtime.backups / snapshot['filename']
                manifest = verify_backup(runtime, path)
                retained = {'journal_voice_catalog_revisions': 2, 'journal_voice_catalog_current': 1,
                            'journal_voice_verifications': 1, 'journal_voice_catalog_mutations': 1}
                for table, rows in retained.items():
                    self.assertEqual(manifest['table_rows'][table], rows)
                    self.assertIn(table, manifest['included_data_tables'])
                for table in private_tables:
                    self.assertEqual(manifest['table_rows'][table], 0)
                    self.assertIn(table, manifest['excluded_data_tables'])
                plaintext = root / 'snapshot.gz'
                crypt(runtime, path, plaintext, decrypt=True)
                dump = gzip.decompress(plaintext.read_bytes())
                self.assertNotIn(b'synthetic-private-journal-body', dump)
                query('catalog-fixture-restore', dump, restored)
                actual = query('catalog-fixture-verify', b'''
                    SELECT r.document->>'$.defaultVoiceID',r.document->>'$.voices[0].name',r.document->>'$.voices[0].rank'
                    FROM journal_voice_catalog_current c JOIN journal_voice_catalog_revisions r ON r.id=c.revision_id;
                    SELECT COUNT(*) FROM journal_voice_catalog_revisions;
                    SELECT document->>'$.audioSHA256',document->>'$.cases' FROM journal_voice_verifications;
                    SELECT idempotency_key,state,HEX(request_hash),response_json IS NULL FROM journal_voice_catalog_mutations;
                    SELECT COUNT(*) FROM journal_voice_catalog_mutations m LEFT JOIN admin_users u ON m.actor_id=u.id WHERE u.id IS NULL;
                ''', restored).decode().splitlines()
                self.assertEqual(actual, ['current\t恢复后的声音\t7', '2', 'b' * 64 + '\t2',
                                         'original-probe-key\tunknown\t' + 'CD' * 32 + '\t1', '0'])
                for table in private_tables:
                    self.assertEqual(query('catalog-private-empty', ('SELECT COUNT(*) FROM `' + table + '`;').encode(), restored).strip(), b'0')
                    self.assertEqual(query('catalog-source-intact', ('SELECT COUNT(*) FROM `' + table + '`;').encode(), database).strip(), b'1')
                if destination := os.environ.get('BACKUP_TEST_EVIDENCE_DIRECTORY'):
                    report = {'passed': True, 'retainedRows': retained, 'excludedPrivateTables': private_tables,
                              'currentVoiceName': '恢复后的声音', 'unknownProbeRetained': True,
                              'foreignKeyOrphans': 0, 'sourcePrivateRowsUnchanged': True,
                              'backupSHA256': snapshot['sha256'], 'isolatedSchemas': [database, restored]}
                    (Path(destination) / 'catalog-round-trip.json').write_text(json.dumps(report, indent=2) + '\n')
            finally:
                query('catalog-fixture-cleanup', ('DROP DATABASE IF EXISTS `' + restored + '`; DROP DATABASE IF EXISTS `' + database + '`;').encode())


if __name__ == '__main__':
    unittest.main(verbosity=2)
