import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import platform_test_artifacts as artifacts
from ops_common import OperationError


class PlatformTestRecoveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.root = self.base / 'backend'
        self.root.mkdir(mode=0o700)
        self.runtime = Mock(root=self.root, state=self.root / '.operations', config={'BACKUP_ENCRYPTION_KEY': '0' * 64})
        self.runtime.state.mkdir()
        self.app = self.base / 'platform-test'
        self.app.mkdir(mode=0o700)
        self.config = {'MYSQL_DATABASE': 'platform_test', 'MYSQL_USER': 'platform_backup_test', 'MYSQL_PORT': '3306',
                       'MYSQL_HOST': '172.29.93.2', 'MYSQL_PASSWORD': '1' * 64, 'BACKUP_ENCRYPTION_KEY': '2' * 64}
        self.save()
        self.runtime.execute.return_value = json.dumps([{'State': {'Running': True}, 'NetworkSettings': {'Networks': {
            'tellyouwhat-platform-test_platform-test': {'IPAddress': '172.29.93.2'}}}}]).encode()

    def save(self):
        p = self.app / 'backup.env'
        p.write_text(''.join(k + '=' + v + '\n' for k, v in self.config.items()))
        p.chmod(0o600)

    def test_checkpoint_uses_its_own_database_account_key_and_transport_boundary(self):
        child = artifacts.checkpoint_runtime(self.runtime)
        self.assertEqual(child.config, self.config)
        self.assertEqual(child.root, (self.app / 'backup-runtime').resolve())
        self.assertFalse(artifacts.ops_offsite.enabled(child))
        for key, value in [('MYSQL_USER', 'root'), ('MYSQL_DATABASE', 'tellyouwhat'),
                           ('MYSQL_HOST', '127.0.0.1'), ('BACKUP_ENCRYPTION_KEY', '0' * 64),
                           ('BACKUP_ENCRYPTION_KEY', '1' * 64)]:
            old = self.config[key]
            self.config[key] = value
            self.save()
            with self.subTest(key=key, value=value), self.assertRaises(OperationError):
                artifacts.checkpoint_runtime(self.runtime)
            self.config[key] = old
        self.save()
        self.runtime.execute.return_value = b'[{"State":{"Running":true},"NetworkSettings":{"Networks":{}}}]'
        with self.assertRaisesRegex(OperationError, 'dedicated MySQL'):
            artifacts.checkpoint_runtime(self.runtime)

    def test_private_root_and_accidental_recursive_transport_are_rejected(self):
        self.app.chmod(0o750)
        with self.assertRaisesRegex(OperationError, 'private'):
            artifacts.checkpoint_runtime(self.runtime)
        self.app.chmod(0o700)
        child = artifacts.checkpoint_runtime(self.runtime)
        (self.app / 'backup').mkdir()
        (self.app / 'backup/cos.env').write_text('synthetic')
        with self.assertRaisesRegex(OperationError, 'outer authenticated transport'):
            artifacts.checkpoint_runtime(self.runtime)

    def test_authenticated_recovery_requires_the_complete_pair_and_matching_bytes(self):
        names = {'state.json', 'owner.json', 'database.env', 'backup.env',
                 *('current/' + name for name in (*artifacts.FILES, 'selection.json')),
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc',
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc.json'}
        hashes = {n: hashlib.sha256(n.encode()).hexdigest() for n in names}
        def archive():
            data = io.BytesIO()
            with tarfile.open(fileobj=data, mode='w') as tar:
                for n in names:
                    item = tarfile.TarInfo('recovery/platform-test/' + n)
                    content = n.encode(); item.size = len(content)
                    tar.addfile(item, io.BytesIO(content))
            data.seek(0)
            return tarfile.open(fileobj=data)
        with archive() as tar:
            artifacts.verify_recovery(tar, hashes)
            with self.assertRaises(OperationError):
                artifacts.verify_recovery(tar, {k: v for k, v in hashes.items() if not k.endswith('.enc.json')})
            hashes['state.json'] = '0' * 64
            with self.assertRaisesRegex(OperationError, 'authenticated manifest'):
                artifacts.verify_recovery(tar, hashes)
        hashes['state.json'] = hashlib.sha256(b'state.json').hexdigest()
        names.update('current/' + n for n in artifacts.albums.FILES)
        hashes.update({n: hashlib.sha256(n.encode()).hexdigest() for n in names})
        with archive() as tar:
            artifacts.verify_recovery(tar, hashes)
            with self.assertRaisesRegex(OperationError, 'incomplete'):
                artifacts.verify_recovery(tar, {k: v for k, v in hashes.items() if k != 'current/albums.env'})

    def test_absent_test_stack_leaves_existing_backups_unchanged(self):
        self.assertEqual(artifacts.backup_configuration(self.runtime, self.base / 'unused'), {})
        self.runtime.execute.assert_not_called()

    def test_recovery_keeps_the_independent_decryption_key_out_of_the_outer_bundle(self):
        source = 'a' * 40
        folder = self.root / '.platform-test/releases' / source
        folder.mkdir(mode=0o700, parents=True)
        state = self.runtime.state / 'platform-test.json'
        state.write_text(json.dumps({'source': source, 'release': str(folder)}))
        state.chmod(0o600)
        owner = self.runtime.state / 'artifact-owners/platform-test.json'
        owner.parent.mkdir()
        owner.write_text(json.dumps({'current': {'source': source, 'paths': [str(folder)]}}))
        owner.chmod(0o600)
        for name in (*artifacts.FILES, 'selection.json'):
            (folder / name).write_bytes(b'synthetic selected source/configuration')
            (folder / name).chmod(0o600)
        (folder / 'runtime.env').write_text('PLATFORM_APPS=birthday\n')
        (self.app / 'database.env').write_text('test database credentials')
        (self.app / 'database.env').chmod(0o600)
        child = artifacts.checkpoint_runtime(self.runtime)
        name = 'mysql-20261016T000000Z-abcdef.sql.gz.enc'
        for filename in (name, name + '.json'):
            (child.backups / filename).write_bytes(b'authenticated snapshot fixture')
            (child.backups / filename).chmod(0o600)
        with patch.object(artifacts, 'selection', return_value=(folder, {}, [])), \
                patch.object(artifacts, 'create_backup', return_value={'filename': name}), \
                patch.object(artifacts, 'verify_backup', return_value={'database': 'platform_test'}):
            destination = self.base / 'bundle'
            hashes = artifacts.backup_configuration(self.runtime, destination)
            self.assertEqual(len(hashes), 11)
            payload = b''.join((destination / n).read_bytes() for n in hashes)
            self.assertNotIn(self.config['BACKUP_ENCRYPTION_KEY'].encode(), payload)
            self.assertNotIn(b'BACKUP_ENCRYPTION_KEY', (destination / 'backup.env').read_bytes())
            (folder / 'runtime.env').write_text('KEY=' + self.config['BACKUP_ENCRYPTION_KEY'])
            with self.assertRaisesRegex(OperationError, 'decryption key'):
                artifacts.backup_configuration(self.runtime, self.base / 'rejected')


if __name__ == '__main__':
    unittest.main()
