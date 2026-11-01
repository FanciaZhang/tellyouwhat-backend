import hashlib
import base64
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

    def test_sandbox_recovery_requires_both_signing_key_and_trust_roots(self):
        names = {'state.json', 'owner.json', 'database.env', 'backup.env',
                 *('current/' + name for name in (*artifacts.FILES, 'selection.json', *artifacts.albums.FILES)),
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc',
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc.json',
                 'secrets/album-app-store.p8', 'secrets/apple-app-store-roots.pem'}
        contents = {n: n.encode() for n in names}
        contents['current/albums.env'] = b'ALBUM_SUBSCRIPTIONS_ENABLED=true\n'
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode='w') as archive:
            for n, content in contents.items():
                entry = tarfile.TarInfo('recovery/platform-test/' + n)
                entry.size = len(content)
                archive.addfile(entry, io.BytesIO(content))
        data.seek(0)
        hashes = {n:hashlib.sha256(c).hexdigest() for n,c in contents.items()}
        with tarfile.open(fileobj=data) as archive:
            artifacts.verify_recovery(archive, hashes)
            for name in ('secrets/album-app-store.p8', 'secrets/apple-app-store-roots.pem'):
                with self.subTest(name=name), self.assertRaisesRegex(OperationError, 'incomplete'):
                    artifacts.verify_recovery(archive, {k:v for k,v in hashes.items() if k != name})

    def test_absent_test_stack_leaves_existing_backups_unchanged(self):
        self.assertEqual(artifacts.backup_configuration(self.runtime, self.base / 'unused'), {})
        self.runtime.execute.assert_not_called()

    def test_media_recovery_requires_both_scoped_configuration_and_reviewed_recipe(self):
        names = {'state.json', 'owner.json', 'database.env', 'backup.env',
                 *('current/' + name for name in (*artifacts.FILES, 'selection.json', *artifacts.albums.FILES, *artifacts.albums.WORKER_FILES)),
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc',
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc.json'}
        contents = {name: name.encode() for name in names}
        contents['current/runtime.env'] = b'PLATFORM_APPS=health,journal,birthday,albums\n'
        contents['current/albums.env'] = b'ALBUM_SHARING_ENABLED=false\n'
        recipe = 'current/' + artifacts.albums.WORKER_COMPOSE
        contents[recipe] = (Path(__file__).resolve().parents[2] / artifacts.albums.WORKER_COMPOSE).read_bytes()
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode='w') as archive:
            for name, content in contents.items():
                entry = tarfile.TarInfo('recovery/platform-test/' + name); entry.size = len(content)
                archive.addfile(entry, io.BytesIO(content))
        data.seek(0)
        hashes = {name: hashlib.sha256(content).hexdigest() for name, content in contents.items()}
        with tarfile.open(fileobj=data) as archive:
            artifacts.verify_recovery(archive, hashes)
            for name in ('current/album-worker.env', recipe):
                with self.subTest(name=name), self.assertRaises(OperationError):
                    artifacts.verify_recovery(archive, {k:v for k,v in hashes.items() if k != name})
            with self.assertRaisesRegex(OperationError, 'unreviewed'):
                artifacts.verify_recovery(archive, {**hashes, recipe: '0' * 64})

    def test_sharing_recovery_requires_current_and_previous_dedicated_keys(self):
        names = {'state.json', 'owner.json', 'database.env', 'backup.env',
                 *('current/' + name for name in (*artifacts.FILES, 'selection.json', *artifacts.albums.FILES)),
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc',
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc.json',
                 'secrets/album-app-store.p8', 'secrets/apple-app-store-roots.pem',
                 'secrets/share-key.base64', 'secrets/share-previous-key.base64'}
        contents = {name: name.encode() for name in names}
        contents['current/albums.env'] = ('ALBUM_SUBSCRIPTIONS_ENABLED=true\nALBUM_SHARING_ENABLED=true\n'
            'ALBUM_SHARE_SITE_ORIGIN=' + artifacts.albums.SHARE_SITE + '\n'
            'ALBUM_SHARE_KEY_FILE=/album-secrets/share-key.base64\n'
            'ALBUM_SHARE_PREVIOUS_KEY_FILE=/album-secrets/share-previous-key.base64\n').encode()
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode='w') as archive:
            for name, content in contents.items():
                entry = tarfile.TarInfo('recovery/platform-test/' + name); entry.size = len(content)
                archive.addfile(entry, io.BytesIO(content))
        data.seek(0)
        hashes = {name: hashlib.sha256(content).hexdigest() for name, content in contents.items()}
        with tarfile.open(fileobj=data) as archive:
            artifacts.verify_recovery(archive, hashes)
            for name in ('secrets/share-key.base64', 'secrets/share-previous-key.base64'):
                with self.subTest(name=name), self.assertRaisesRegex(OperationError, 'incomplete'):
                    artifacts.verify_recovery(archive, {k:v for k,v in hashes.items() if k != name})
            with self.assertRaisesRegex(OperationError, 'authenticated manifest'):
                artifacts.verify_recovery(archive, {**hashes, 'secrets/share-key.base64': '0' * 64})

    def test_read_aloud_recovery_requires_its_selected_readonly_overlay(self):
        names = {'state.json', 'owner.json', 'database.env', 'backup.env',
                 *('current/' + name for name in (*artifacts.FILES, 'selection.json')),
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc',
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc.json'}
        contents = {name: name.encode() for name in names}
        journal = artifacts.journal
        contents['current/runtime.env'] = ('JOURNAL_TTS_ENABLED=true\nJOURNAL_TTS_API_KEY_FILE=' + journal.KEY_FILE
            + '\nJOURNAL_TTS_MONTHLY_CHARACTERS=10000\nJOURNAL_TTS_PREVIEW_DAILY_CHARACTERS=1000\n').encode()
        overlay = 'current/' + journal.READ_ALOUD_OVERLAY
        contents[overlay] = (Path(__file__).resolve().parents[2] / journal.READ_ALOUD_OVERLAY).read_bytes()
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode='w') as archive:
            for name, content in contents.items():
                entry = tarfile.TarInfo('recovery/platform-test/' + name)
                entry.size = len(content)
                archive.addfile(entry, io.BytesIO(content))
        data.seek(0)
        hashes = {name: hashlib.sha256(content).hexdigest() for name, content in contents.items()}
        with tarfile.open(fileobj=data) as archive:
            artifacts.verify_recovery(archive, hashes)
            with self.assertRaisesRegex(OperationError, 'mount is incomplete'):
                artifacts.verify_recovery(archive, {k: v for k, v in hashes.items() if k != overlay})
            hashes[overlay] = '0' * 64
            with self.assertRaisesRegex(OperationError, 'unreviewed'):
                artifacts.verify_recovery(archive, hashes)

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

    def test_admin_recovery_requires_selected_overlay_and_only_its_private_keys(self):
        self.admin_recovery_recipe(False)
        self.admin_recovery_recipe(True)

    def admin_recovery_recipe(self, previous):
        names = {'state.json', 'owner.json', 'database.env', 'backup.env',
                 *('current/' + name for name in (*artifacts.FILES, 'selection.json', *artifacts.admin.FILES)),
                 'current/' + artifacts.journal.READ_ALOUD_OVERLAY,
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc',
                 'checkpoint/mysql-20261016T000000Z-abcdef.sql.gz.enc.json'}
        contents = {name: name.encode() for name in names}
        payload = base64.b64encode(b'p' * 32).decode()
        contents['current/runtime.env'] = ('JOURNAL_TTS_ENABLED=true\nJOURNAL_TTS_API_KEY_FILE=' + artifacts.journal.KEY_FILE
            + '\nJOURNAL_TTS_MONTHLY_CHARACTERS=10000\nJOURNAL_TTS_PREVIEW_DAILY_CHARACTERS=1000\nPAYLOAD_ENCRYPTION_KEY=' + payload + '\n'
            + ''.join(key + '=ep-journal-test\n' for key in sorted(artifacts.admin.MODELS))).encode()
        contents['current/admin.env'] = ('PAYLOAD_ENCRYPTION_KEY=' + payload + '\nADMIN_PREVIEW_SIGNING_KEY='
            + base64.b64encode(b's' * 32).decode().rstrip('=') + '\n'
            + ''.join(key + '=ep-journal-test\n' for key in sorted(artifacts.admin.MODELS))).encode()
        for overlay in (artifacts.journal.READ_ALOUD_OVERLAY, artifacts.admin.OVERLAY):
            contents['current/' + overlay] = (Path(__file__).resolve().parents[2] / overlay).read_bytes()
        if previous:
            contents['current/' + artifacts.admin.OVERLAY] = (Path(__file__).parent / 'testdata/previous-platform-admin-test.yaml').read_bytes()
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode='w') as archive:
            for name, content in contents.items():
                entry = tarfile.TarInfo('recovery/platform-test/' + name); entry.size = len(content)
                archive.addfile(entry, io.BytesIO(content))
        data.seek(0)
        hashes = {name: hashlib.sha256(content).hexdigest() for name, content in contents.items()}
        with tarfile.open(fileobj=data) as archive:
            artifacts.verify_recovery(archive, hashes)
            for name in artifacts.admin.FILES:
                with self.assertRaises(OperationError):
                    artifacts.verify_recovery(archive, {k: v for k, v in hashes.items() if k != 'current/' + name})
            with self.assertRaisesRegex(OperationError, 'authenticated manifest'):
                artifacts.verify_recovery(archive, {**hashes, 'current/admin.env': '0' * 64})


if __name__ == '__main__':
    unittest.main()
