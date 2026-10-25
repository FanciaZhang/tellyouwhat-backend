import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import albums_media as media
from albums_artifacts import FILES, verify_recovery
import albums_artifacts_test
from ops_common import OperationError


class MediaActivationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name) / 'backend'
        self.state = self.root / '.operations'
        self.state.mkdir(parents=True, mode=0o700)
        self.runtime = Mock(root=self.root, state=self.state, backups=self.root / 'backups')
        for name in ('albums_media.py', 'albums_artifacts.py'):
            self.write(self.root / 'deploy/tencent' / name, Path(media.__file__).with_name(name).read_bytes())
        self.source, self.run = 'a' * 40, 'b' * 32
        self.folder = self.root / '.albums-development/releases' / self.source
        self.folder.mkdir(parents=True)
        for path in (self.folder, self.folder.parent, self.folder.parent.parent):
            path.chmod(0o700)
        image = 'tellyouwhat-albums-test-development-albumworker:' + self.source
        app = '\n'.join(['ALBUM_COS_BUCKET=tellyouwhat-albums-1251995834', 'ALBUM_API_HOST=' + media.HOST,
                         'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums.dev', 'ALBUM_CI_VIDEO_QUEUE_ID=p' + 'c' * 32,
                         *[key + '=false' for key in media.FLAGS]]) + '\n'
        self.files = {name: b'reviewed source' for name in FILES if name != 'selection.json'}
        self.files['runtime.env'] = app.encode()
        self.files['selection.json'] = json.dumps({'source': self.source, 'files': {name: media.digest(data) for name, data in self.files.items()}}).encode()
        for name, data in self.files.items():
            self.write(self.folder / name, data)
        self.old = json.dumps({'source': self.source, 'release': str(self.folder)}).encode()
        self.write(self.state / 'albums-development.json', self.old)
        self.write(self.state / 'artifact-owners/albums-test.json', json.dumps({'current': {'source': self.source, 'paths': [str(self.folder)], 'images': [image]}}).encode())
        for name in ('operations', 'deployment'):
            self.write(self.state / (name + '.lock'), b'lock')
        self.worker = {'Id': 'worker-original', 'Image': 'sha256:' + 'd' * 64, 'RestartCount': 0, 'Mounts': [],
                       'State': {'Running': True, 'StartedAt': '2026-10-25T00:00:00Z'},
                       'Config': {'Image': image, 'Env': [*[key + '=false' for key in media.FLAGS], 'UNCHANGED=synthetic'],
                                  'Labels': {'com.docker.compose.project': media.PROJECT}},
                       'NetworkSettings': {'Networks': {media.PROJECT + '_albums-test': {}}}}
        self.starts = []
        self.fail_startup = False
        self.runtime.execute.side_effect = self.execute

    def write(self, path, data):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        path.chmod(0o600)

    def execute(self, label, command, **kwargs):
        if label == 'albums-media-inspect':
            result = self.worker if command[-1] == media.WORKER else {'Id': command[-1]}
            return json.dumps([result]).encode()
        if label == 'albums-media-start':
            self.assertEqual(command[-5:], ['up', '-d', '--no-build', '--no-deps', 'albumworker'])
            self.assertNotIn('migrate', command)
            name = Path(command[-6]).name
            self.starts.append(name)
            enabled = name == 'after.yaml'
            env = media.environment(self.worker)
            env.update({key: str(enabled).lower() for key in media.FLAGS})
            self.worker['Config']['Env'] = [key + '=' + value for key, value in env.items()]
            if self.fail_startup and enabled:
                self.worker['State']['Running'] = False
            else:
                self.worker['State']['Running'] = True
            return b''
        if label == 'albums-media-startup':
            return b'{"msg":"album workers started"}\n'
        self.fail('unexpected command ' + label)

    def activate(self, *, post_proof=True):
        folder = media.prepare(self.runtime, self.run)
        proof = {'media/' + name: media.digest((folder / name).read_bytes()) for name in media.MEDIA_FILES}
        def verify(_runtime, path):
            data = {'state.json': media.digest((self.state / 'albums-development.json').read_bytes()),
                    'current/selection.json': media.digest(self.files['selection.json']),
                    'checkpoint/albums-' + 'f' * 32 + '.sql.gz.enc': 'verified'}
            if path.name == 'after' and post_proof:
                data.update(proof)
            return {'recovery_albums_development': data}
        with patch.object(media.fcntl, 'flock', side_effect=BlockingIOError), \
             patch('ops_backup.create_backup', side_effect=[{'filename': 'before'}, {'filename': 'after'}]), \
             patch('ops_backup.verify_backup', side_effect=verify):
            return media.activate(self.runtime, self.run)

    def test_prepare_keeps_runtime_unchanged_and_rejects_stale_or_repeated_input(self):
        folder = media.prepare(self.runtime, self.run)
        self.assertEqual(self.starts, [])
        self.assertEqual((self.state / 'albums-development.json').read_bytes(), self.old)
        self.assertEqual((folder / 'after.yaml').read_bytes(), media.overlay(True))
        with self.assertRaises(FileExistsError):
            media.prepare(self.runtime, self.run)
        self.write(self.state / 'albums-development.json', self.old + b' ')
        with patch.object(media.fcntl, 'flock', side_effect=BlockingIOError):
            with self.assertRaisesRegex(OperationError, 'changed after preparation'):
                media.activate(self.runtime, self.run)
        self.assertEqual(self.starts, [])

    def test_success_changes_only_worker_and_captures_both_recovery_configurations(self):
        result = self.activate()
        self.assertEqual(self.starts, ['after.yaml'])
        self.assertEqual(result['postBackup'], 'after')
        state = json.loads((self.state / 'albums-development.json').read_bytes())
        data = media.recovery_files(self.runtime, state)
        self.assertEqual(set(data), {'media/' + name for name in media.MEDIA_FILES})
        self.assertEqual(data['media/before.yaml'], media.overlay(False))
        for name, value in self.files.items():
            self.assertEqual((self.folder / name).read_bytes(), value)

    def test_failed_start_restores_prior_worker_and_exact_state(self):
        self.fail_startup = True
        with self.assertRaises(OperationError):
            self.activate()
        self.assertEqual(self.starts, ['after.yaml', 'before.yaml'])
        self.assertEqual((self.state / 'albums-development.json').read_bytes(), self.old)
        self.assertTrue(self.worker['State']['Running'])

    def test_missing_post_backup_restores_prior_configuration(self):
        with self.assertRaisesRegex(OperationError, 'missing from verified recovery'):
            self.activate(post_proof=False)
        self.assertEqual(self.starts, ['after.yaml', 'before.yaml'])
        self.assertEqual((self.state / 'albums-development.json').read_bytes(), self.old)

    def test_lock_and_worker_isolation_guards_prevent_activation(self):
        media.prepare(self.runtime, self.run)
        with self.assertRaisesRegex(OperationError, 'deployment locks'):
            media.activate(self.runtime, self.run)
        self.assertEqual(self.starts, [])
        self.worker['Mounts'] = [{'Destination': '/album-secrets'}]
        with self.assertRaisesRegex(OperationError, 'isolation'):
            media.prepare(self.runtime, 'c' * 32)

    def test_altered_overlay_and_symlink_are_rejected(self):
        folder = media.prepare(self.runtime, self.run)
        manifest = (folder / 'manifest.json').read_bytes()
        state = {**json.loads(self.old), 'media': {'run': self.run, 'manifestSHA256': media.digest(manifest)}}
        (folder / 'after.yaml').write_bytes(media.overlay(True) + b'  albumgateway: {}\n')
        with self.assertRaisesRegex(OperationError, 'recovery selection'):
            media.recovery_files(self.runtime, state)
        (folder / 'after.yaml').unlink()
        (folder / 'after.yaml').symlink_to(folder / 'before.yaml')
        with self.assertRaisesRegex(OperationError, 'private'):
            media.recovery_files(self.runtime, state)

    def test_staged_only_recovery_code_cannot_enable_processing(self):
        media.prepare(self.runtime, self.run)
        (self.root / 'deploy/tencent/albums_artifacts.py').write_text('older scheduled backup implementation')
        with patch.object(media.fcntl, 'flock', side_effect=BlockingIOError):
            with self.assertRaisesRegex(OperationError, 'matched scheduled recovery helpers'):
                media.activate(self.runtime, self.run)
        self.assertEqual(self.starts, [])
        self.assertEqual((self.state / 'albums-development.json').read_bytes(), self.old)


class MediaRecoveryTests(albums_artifacts_test.AlbumsDevelopmentRecoveryTests):
    def test_backup_requires_exact_media_pair_and_verifies_it_in_archive(self):
        import tarfile
        self.prepare()
        path = self.state / 'albums-development.json'
        state = json.loads(path.read_text())
        run = 'e' * 32
        folder = self.root / '.albums-development/media' / run
        folder.mkdir(parents=True)
        for item in (folder, folder.parent, folder.parent.parent):
            item.chmod(0o700)
        data = {'before.yaml': media.overlay(False), 'after.yaml': media.overlay(True)}
        record = {'run': run, 'source': state['source'], 'imageID': 'sha256:' + 'f' * 64,
                  'overlays': {name: media.digest(value) for name, value in data.items()}}
        data['manifest.json'] = json.dumps(record).encode()
        for name, value in data.items():
            self.private(folder / name, value)
        state['media'] = {'run': run, 'manifestSHA256': media.digest(data['manifest.json'])}
        self.private(path, json.dumps(state).encode())
        manifest = self.backup()
        for name, value in data.items():
            self.assertEqual(manifest['media/' + name], media.digest(value))
        archive_path = self.base / 'backup.tar'
        with tarfile.open(archive_path, 'w') as archive:
            archive.add(self.base / 'output', arcname='recovery/albums-development')
        with tarfile.open(archive_path) as archive:
            verify_recovery(archive, manifest)
            incomplete = dict(manifest)
            incomplete.pop('media/before.yaml')
            with self.assertRaisesRegex(OperationError, 'incomplete'):
                verify_recovery(archive, incomplete)
