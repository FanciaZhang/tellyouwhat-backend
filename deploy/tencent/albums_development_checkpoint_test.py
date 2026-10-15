import io
import json
from pathlib import Path
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import albums_development_checkpoint as checkpoint
from ops_common import OperationError


class AlbumsDevelopmentCheckpointTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.root = self.base / 'backend'
        self.root.mkdir()
        self.app = self.base / 'albums-development'
        self.app.mkdir(mode=0o700)
        self.runtime = SimpleNamespace(root=self.root, state=self.root / '.operations')
        self.runtime.state.mkdir()
        self.folder = self.root / 'source'
        self.folder.mkdir()
        self.config = {'MYSQL_HOST': '127.0.0.1', 'MYSQL_PORT': '3306', 'MYSQL_DATABASE': 'albums_test',
                       'MYSQL_USER': 'albums_development_backup_test', 'MYSQL_PASSWORD': '1' * 64,
                       'ALBUM_BACKUP_ENCRYPTION_KEY': '2' * 64}
        self.save()
        binaries = self.app / 'backup-bin'
        binaries.mkdir(mode=0o700)
        for name in ('mysql', 'mysqldump'):
            path = binaries / name
            path.write_bytes(checkpoint.wrapper(name))
            path.chmod(0o700)
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name in ('backup', 'deletion_ledger'):
                item = tarfile.TarInfo('deploy/albums/' + name + '.py')
                item.size = 1
                archive.addfile(item, io.BytesIO(b'x'))

    def save(self):
        path = self.app / 'backup.env'
        path.write_text(''.join(k + '=' + v + '\n' for k, v in self.config.items()))
        path.chmod(0o600)

    def test_wrong_database_root_credentials_and_extra_socket_are_rejected(self):
        for key, value in [('MYSQL_DATABASE', 'health'), ('MYSQL_USER', 'root'),
                           ('MYSQL_PASSWORD', '2' * 64), ('MYSQL_SOCKET', '/production/mysql.sock')]:
            with self.subTest(key=key):
                before = dict(self.config)
                self.config[key] = value
                self.save()
                with self.assertRaisesRegex(OperationError, 'isolation'):
                    checkpoint.checkpoint(self.runtime, self.folder)
                self.config = before

    def test_client_cannot_be_substituted_with_production_or_nonexecutable_file(self):
        path = self.app / 'backup-bin/mysql'
        path.write_bytes(checkpoint.wrapper('mysql').replace(b'albums-development', b'production'))
        with self.assertRaisesRegex(OperationError, 'dedicated test container'):
            checkpoint.checkpoint(self.runtime, self.folder)
        path.write_bytes(checkpoint.wrapper('mysql'))
        path.chmod(0o600)
        with self.assertRaisesRegex(OperationError, 'dedicated test container'):
            checkpoint.checkpoint(self.runtime, self.folder)
        with self.assertRaises(OperationError):
            checkpoint.wrapper('sh')

    def test_verified_checkpoint_keeps_its_decryption_key_outside_the_bundle(self):
        child = SimpleNamespace(backups=self.app / 'backups', config=self.config)
        backup, ledger = Mock(), Mock()
        backup.Runtime.return_value = child
        snapshot_name = 'albums-' + 'a' * 32 + '.sql.gz.enc'
        ledger_name = 'albums-deletions-' + 'b' * 32 + '.json'

        def create(*args):
            (child.backups / snapshot_name).write_bytes(b'encrypted SQL')
            (child.backups / (snapshot_name + '.json')).write_text('{"authenticated":true}')
            for name in (snapshot_name, snapshot_name + '.json'):
                (child.backups / name).chmod(0o600)
            return {'filename': snapshot_name}

        def publish(*args):
            (child.backups / ledger_name).write_bytes(b'authenticated deletion fence')
            (child.backups / ledger_name).chmod(0o600)
            return {'filename': ledger_name, 'sha256': 'c' * 64}

        backup.create.side_effect = create
        backup.connection.return_value = ['--host=127.0.0.1', '--port=3306', '--user=albums_development_backup_test']
        backup.verify.return_value = {'created_at': 123}
        ledger.publish.side_effect = publish
        with patch.object(checkpoint.importlib.util, 'module_from_spec', side_effect=[backup, ledger]), \
                patch.object(checkpoint.importlib.util, 'spec_from_file_location', return_value=Mock(loader=Mock())):
            result = checkpoint.checkpoint(self.runtime, self.folder)
        self.assertEqual(len(result), 4)
        self.assertNotIn(b'ALBUM_BACKUP_ENCRYPTION_KEY', result['backup.env'])
        self.assertNotIn(self.config['ALBUM_BACKUP_ENCRYPTION_KEY'].encode(), b''.join(result.values()))
        self.assertIn(self.config['ALBUM_BACKUP_ENCRYPTION_KEY'], (self.app / 'backup.env').read_text())
        backup.verify.assert_called_once_with(child, child.backups / snapshot_name)
        ledger.verify.assert_called_once_with(child, child.backups / ledger_name, 'c' * 64, 123)
        backup.Runtime.assert_called_once_with(self.root, self.app / 'backups', self.app / 'backup.env')


if __name__ == '__main__':
    unittest.main()
