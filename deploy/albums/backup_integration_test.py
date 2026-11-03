"""Opt-in snapshot round trips against disposable socket-only MySQL databases."""
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

import backup


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
                    migrations = Path(__file__).resolve().parents[2] / 'migrations'
                    for path in sorted(migrations.glob('*.sql')):
                        if path.name[:4] < '0074':
                            subprocess.run(sql, input=path.read_bytes(), check=True, capture_output=True, timeout=20)
                    subprocess.run(sql, input=b"INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES ('00000000-0000-0000-0000-000000000001',REPEAT('a',64));", check=True, capture_output=True)
                    settings = root / 'backup.env'
                    settings.write_text('MYSQL_DATABASE=albums_test\nMYSQL_SOCKET=' + str(sock) + '\nMYSQL_USER=root\nALBUM_BACKUP_ENCRYPTION_KEY=' + os.urandom(32).hex() + '\n')
                    settings.chmod(0o600)
                    runtime = backup.Runtime(root, root / 'vault', settings)
                    with patch.object(backup, 'TABLES', backup.PRE_COPY_TABLES):
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
""", check=True, capture_output=True)
                    current = backup.create(runtime, binaries)
                    manifest = backup.verify(runtime, runtime.backups / current['filename'])
                    self.assertEqual(manifest['table_rows']['album_system_copy_locators'], 1)
                    result = backup.drill(runtime, binaries, current['filename'])
                    self.assertEqual(result['tables'], len(backup.TABLES))
                    self.assertTrue(result['quarantined'])
                    self.assertTrue((runtime.backups / previous['filename']).exists())
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
