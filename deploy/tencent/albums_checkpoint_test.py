import io
import contextlib
import json
from pathlib import Path
import tarfile
import tempfile
import sys
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import albums_checkpoint as checkpoint
from ops_common import OperationError


class AlbumsDevelopmentCheckpointTests(unittest.TestCase):
    environment = 'development'
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.root = self.base / 'backend'
        self.root.mkdir()
        self.app = self.base / ('albums-' + self.environment)
        self.app.mkdir(mode=0o700)
        self.runtime = SimpleNamespace(root=self.root, state=self.root / '.operations')
        self.runtime.state.mkdir()
        self.folder = self.root / 'source'
        self.folder.mkdir()
        database, user = checkpoint.database_scope(self.environment)
        self.archive_collection_helper = True
        self.config = {'MYSQL_HOST': 'localhost', 'MYSQL_PORT': '3306', 'MYSQL_DATABASE': database,
                       'MYSQL_USER': user, 'MYSQL_PASSWORD': '1' * 64,
                       'ALBUM_BACKUP_ENCRYPTION_KEY': '2' * 64}
        self.save()
        binaries = self.app / 'backup-bin'
        binaries.mkdir(mode=0o700)
        for name in ('mysql', 'mysqldump'):
            path = binaries / name
            path.write_bytes(checkpoint.wrapper(name, self.environment))
            path.chmod(0o700)
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name in ('backup', 'deletion_ledger', 'asset_deletion_ledger', 'collection_deletion_ledger'):
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
                    checkpoint.checkpoint(self.runtime, self.folder, self.environment)
                self.config = before

    def test_client_cannot_be_substituted_with_production_or_nonexecutable_file(self):
        path = self.app / 'backup-bin/mysql'
        path.write_bytes(checkpoint.wrapper('mysql', self.environment).replace(b'-mysql-1', b'-wrong-mysql-1'))
        with self.assertRaisesRegex(OperationError, 'selected environment container'):
            checkpoint.checkpoint(self.runtime, self.folder, self.environment)
        path.write_bytes(checkpoint.wrapper('mysql', self.environment))
        path.chmod(0o600)
        with self.assertRaisesRegex(OperationError, 'selected environment container'):
            checkpoint.checkpoint(self.runtime, self.folder, self.environment)
        with self.assertRaises(OperationError):
            checkpoint.wrapper('sh')

    def test_development_snapshot_implementation_must_be_operator_owned_regular_and_not_writable(self):
        if self.environment != 'development':
            return
        source = Path(checkpoint.__file__).resolve().parent.parent / 'albums'
        destination = self.root / 'deploy/albums'
        destination.mkdir(parents=True)
        for name in ('backup', 'deletion_ledger', 'asset_deletion_ledger', 'collection_deletion_ledger'):
            (destination / (name + '.py')).write_bytes((source / (name + '.py')).read_bytes())
        for name in ('backup', 'deletion_ledger', 'asset_deletion_ledger', 'collection_deletion_ledger'):
            target = destination / (name + '.py')
            original = target.read_bytes()
            with patch.object(checkpoint, '__file__', str(self.root / 'deploy/tencent/albums_checkpoint.py')):
                target.chmod(0o666)
                with self.assertRaisesRegex(OperationError, 'implementation is unsafe'):
                    checkpoint.checkpoint(self.runtime, self.folder, self.environment)
                target.unlink(); target.symlink_to(self.folder / 'source.tar')
                with self.assertRaisesRegex(OperationError, 'implementation is unsafe'):
                    checkpoint.checkpoint(self.runtime, self.folder, self.environment)
            target.unlink(); target.write_bytes(original)

    def test_verified_checkpoint_keeps_its_decryption_key_outside_the_bundle(self):
        if self.environment == 'development':
            # Existing data-service archives predate the asset ledger module.
            with tarfile.open(self.folder / 'source.tar', 'w') as archive:
                for name in ('backup', 'deletion_ledger'):
                    item = tarfile.TarInfo('deploy/albums/' + name + '.py'); item.size = 1
                    archive.addfile(item, io.BytesIO(b'x'))
        child = SimpleNamespace(backups=self.app / 'backups', config=self.config, lock=Mock(return_value=contextlib.nullcontext()))
        backup, ledger, assets, collections = Mock(), Mock(), Mock(), Mock()
        backup.RETENTION = 14 * 86400
        backup.Runtime.return_value = child
        snapshot_name = 'albums-' + 'a' * 32 + '.sql.gz.enc'
        ledger_name = 'albums-deletions-' + 'b' * 32 + '.json'

        def create(*args):
            from deletion_ledger import verify
            self.assertIs(verify, ledger.verify)
            from asset_deletion_ledger import export
            self.assertIs(export, assets.export)
            if self.environment == 'development' or self.archive_collection_helper:
                from collection_deletion_ledger import export as export_collections
                self.assertIs(export_collections, collections.export)
            (child.backups / snapshot_name).write_bytes(b'encrypted SQL')
            (child.backups / (snapshot_name + '.json')).write_text('{"authenticated":true}')
            for name in (snapshot_name, snapshot_name + '.json'):
                (child.backups / name).chmod(0o600)
            for name, stamp in [('c' * 32, 1), ('d' * 32, int(time.time()))]:
                path = child.backups / ('albums-deletions-' + name + '.json')
                path.write_text(json.dumps({'payload': {'exported_at': stamp}}))
                path.chmod(0o600)
            return {'filename': snapshot_name}

        def publish(*args):
            (child.backups / ledger_name).write_bytes(b'authenticated deletion fence')
            (child.backups / ledger_name).chmod(0o600)
            return {'filename': ledger_name, 'sha256': 'c' * 64}

        backup.create.side_effect = create
        backup.connection.return_value = ['--host=localhost', '--port=3306', '--user=' + self.config['MYSQL_USER']]
        backup.verify.return_value = {'created_at': 123}
        ledger.publish.side_effect = publish
        previous_ledger, previous_assets, previous_collections = object(), object(), object()
        with patch.dict(sys.modules, {'deletion_ledger': previous_ledger, 'asset_deletion_ledger': previous_assets, 'collection_deletion_ledger': previous_collections}), \
                patch.object(checkpoint.importlib.util, 'module_from_spec', side_effect=[backup, ledger, assets] + ([collections] if self.environment == 'development' or self.archive_collection_helper else [])), \
                patch.object(checkpoint.importlib.util, 'spec_from_file_location', return_value=Mock(loader=Mock())):
            result = checkpoint.checkpoint(self.runtime, self.folder, self.environment)
            self.assertIs(sys.modules['deletion_ledger'], previous_ledger)
            self.assertIs(sys.modules['asset_deletion_ledger'], previous_assets)
            self.assertIs(sys.modules['collection_deletion_ledger'], previous_collections)
        self.assertEqual(len(result), 8 if self.environment == 'development' else 4)
        if self.environment == 'development':
            self.assertEqual(result['checkpoint/backup.py'], (Path(checkpoint.__file__).resolve().parent.parent / 'albums/backup.py').read_bytes())
            for name in ('deletion_ledger', 'asset_deletion_ledger', 'collection_deletion_ledger'):
                self.assertEqual(result['checkpoint/' + name + '.py'], (Path(checkpoint.__file__).resolve().parent.parent / 'albums' / (name + '.py')).read_bytes())
        else:
            self.assertNotIn('checkpoint/backup.py', result)
        self.assertNotIn(b'ALBUM_BACKUP_ENCRYPTION_KEY', result['backup.env'])
        self.assertNotIn(self.config['ALBUM_BACKUP_ENCRYPTION_KEY'].encode(), b''.join(result.values()))
        self.assertIn(self.config['ALBUM_BACKUP_ENCRYPTION_KEY'], (self.app / 'backup.env').read_text())
        self.assertIn('--database=' + self.config['MYSQL_DATABASE'], ledger.publish.call_args.args[1])
        backup.verify.assert_called_once_with(child, child.backups / snapshot_name)
        ledger.verify.assert_any_call(child, child.backups / ledger_name, 'c' * 64, 123)
        self.assertEqual(ledger.verify.call_count, 1)
        ledger.prune_expired.assert_called_once()
        self.assertIs(ledger.prune_expired.call_args.args[0], child)
        self.assertLess(abs(ledger.prune_expired.call_args.args[1] - (int(time.time()) - backup.RETENTION)), 5)
        self.assertTrue((child.backups / ('albums-deletions-' + 'c' * 32 + '.json')).exists())
        self.assertTrue((child.backups / ('albums-deletions-' + 'd' * 32 + '.json')).exists())
        backup.Runtime.assert_called_once_with(self.root, self.app / 'backups', self.app / 'backup.env')
        child.lock.assert_called_once()

    def test_failed_export_restores_both_sibling_modules(self):
        backup, ledger, assets, collections = Mock(), Mock(), Mock(), Mock()
        backup.connection.return_value = []
        backup.Runtime.return_value.lock.return_value = contextlib.nullcontext()
        ledger.publish.side_effect = OperationError('synthetic export failed')
        previous = {'deletion_ledger': object(), 'asset_deletion_ledger': object(), 'collection_deletion_ledger': object()}
        with patch.dict(sys.modules, previous), \
                patch.object(checkpoint.importlib.util, 'module_from_spec', side_effect=[backup, ledger, assets] + ([collections] if self.environment == 'development' or self.archive_collection_helper else [])), \
                patch.object(checkpoint.importlib.util, 'spec_from_file_location', return_value=Mock(loader=Mock())):
            with self.assertRaisesRegex(OperationError, 'synthetic export failed'):
                checkpoint.checkpoint(self.runtime, self.folder, self.environment)
            for name, module in previous.items():
                self.assertIs(sys.modules[name], module)
        self.assertFalse(list(self.runtime.state.glob('.albums-checkpoint-*')))

    def test_production_uses_its_frozen_three_module_source_without_upgrading_it(self):
        if self.environment != 'production':
            return
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name in ('backup', 'deletion_ledger', 'asset_deletion_ledger'):
                item = tarfile.TarInfo('deploy/albums/' + name + '.py'); item.size = 1
                archive.addfile(item, io.BytesIO(b'x'))
        self.archive_collection_helper = False
        self.test_verified_checkpoint_keeps_its_decryption_key_outside_the_bundle()

    def test_development_requires_its_current_collection_implementation(self):
        if self.environment != 'development':
            return
        destination = self.root / 'deploy/albums'
        destination.mkdir(parents=True)
        for name in ('backup', 'deletion_ledger', 'asset_deletion_ledger'):
            (destination / (name + '.py')).write_bytes(b'x')
        with patch.object(checkpoint, '__file__', str(self.root / 'deploy/tencent/albums_checkpoint.py')):
            with self.assertRaisesRegex(OperationError, 'implementation is unsafe'):
                checkpoint.checkpoint(self.runtime, self.folder, self.environment)
        self.assertFalse(list(self.runtime.state.glob('.albums-checkpoint-*')))

    def test_missing_asset_implementation_is_rejected_before_snapshot(self):
        if self.environment == 'development':
            destination = self.root / 'deploy/albums'
            destination.mkdir(parents=True)
            for name in ('backup', 'deletion_ledger'):
                (destination / (name + '.py')).write_bytes(b'x')
            with patch.object(checkpoint, '__file__', str(self.root / 'deploy/tencent/albums_checkpoint.py')):
                with self.assertRaisesRegex(OperationError, 'implementation is unsafe'):
                    checkpoint.checkpoint(self.runtime, self.folder, self.environment)
            self.assertFalse(list(self.runtime.state.glob('.albums-checkpoint-*')))
            return
        with tarfile.open(self.folder / 'source.tar', 'w') as archive:
            for name in ('backup', 'deletion_ledger'):
                item = tarfile.TarInfo('deploy/albums/' + name + '.py'); item.size = 1
                archive.addfile(item, io.BytesIO(b'x'))
        backup, ledger = Mock(), Mock()
        with patch.object(checkpoint.importlib.util, 'module_from_spec', side_effect=[backup, ledger]), \
                patch.object(checkpoint.importlib.util, 'spec_from_file_location', return_value=Mock(loader=Mock())):
            with self.assertRaisesRegex(OperationError, 'implementation is unavailable'):
                checkpoint.checkpoint(self.runtime, self.folder, self.environment)
        backup.create.assert_not_called()
        self.assertFalse(list(self.runtime.state.glob('.albums-checkpoint-*')))


class AlbumsProductionCheckpointTests(AlbumsDevelopmentCheckpointTests):
    environment = 'production'

    def test_development_database_user_and_wrapper_are_rejected(self):
        for key, value in [('MYSQL_DATABASE', 'albums_test'), ('MYSQL_USER', 'albums_development_backup_test')]:
            original = self.config[key]
            self.config[key] = value
            self.save()
            with self.assertRaisesRegex(OperationError, 'isolation'):
                checkpoint.checkpoint(self.runtime, self.folder, self.environment)
            self.config[key] = original
        self.save()
        (self.app / 'backup-bin/mysql').write_bytes(checkpoint.wrapper('mysql', 'development'))
        with self.assertRaisesRegex(OperationError, 'selected environment'):
            checkpoint.checkpoint(self.runtime, self.folder, self.environment)
        with self.assertRaisesRegex(OperationError, 'unknown'):
            checkpoint.checkpoint(self.runtime, self.folder, '../development')


if __name__ == '__main__':
    unittest.main()
