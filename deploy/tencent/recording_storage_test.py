import os
from pathlib import Path
import stat
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import ops_recording as recording
from ops_common import OperationError, compose_command
from release import copy_runtime


class RecordingStorageTests(unittest.TestCase):
    def setUp(self):
        # Exercise the /opt ancestor policy without Linux's publicly writable /tmp.
        temporary = tempfile.TemporaryDirectory(dir=Path.home())
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.stage = self.root / 'release'
        self.stage.mkdir()
        self.overlay = self.stage / 'compose.journal-recording.yaml'
        self.overlay.write_text('services: {}\n')
        self.base = self.root / 'recordings'
        self.base.mkdir(mode=0o711)
        self.path = self.base / 'production'
        self.path.mkdir(mode=0o700)
        self.config = {'APP_ENV': 'production', 'JOURNAL_RECORDING_ENABLED': 'false',
                       'JOURNAL_RECORDING_ROOT': str(self.path)}
        fixture = patch.object(recording, 'BASE', self.base)
        fixture.start()
        self.addCleanup(fixture.stop)

    def test_disabled_without_storage_keeps_old_deployments_unchanged(self):
        runtime = Mock()
        recording.prepare(runtime, {})
        runtime.execute.assert_not_called()
        self.assertEqual(recording.compose_overlay(self.stage, {}), [])

    def test_cross_environment_and_noncanonical_paths_are_rejected(self):
        for changes in ({'APP_ENV': 'development'}, {'JOURNAL_RECORDING_ROOT': str(self.base / 'development')},
                        {'JOURNAL_RECORDING_ROOT': str(self.path) + '/'}, {'JOURNAL_RECORDING_ROOT': '' , 'JOURNAL_RECORDING_ENABLED': 'true'},
                        {'JOURNAL_RECORDING_ENABLED': 'yes'}):
            with self.subTest(changes=changes), self.assertRaises(OperationError):
                recording.configured_root(dict(self.config, **changes), 'production')
        development = dict(self.config, APP_ENV='development', JOURNAL_RECORDING_ROOT=str(self.base / 'development'))
        self.assertEqual(recording.configured_root(development, 'development'), self.base / 'development')

    def test_preparation_is_scoped_and_retained_when_generation_disabled(self):
        runtime = Mock()
        recording.prepare(runtime, self.config)
        command = runtime.execute.call_args.args[1]
        self.assertEqual(command[:3], ['sudo', '-n', 'python3'])
        self.assertEqual(command[-1], 'production')
        self.assertNotIn(str(self.path), command)  # privileged entry accepts only the environment

    def test_first_enable_requires_a_storage_aware_rollback(self):
        recording.require_continuity({}, self.config)
        with self.assertRaisesRegex(OperationError, 'compatible rollback'):
            recording.require_continuity({}, dict(self.config, JOURNAL_RECORDING_ENABLED='true'))
        recording.require_continuity(self.config, dict(self.config, JOURNAL_RECORDING_ENABLED='true'))
        recording.require_continuity(dict(self.config, JOURNAL_RECORDING_ENABLED='true'), self.config)
        for after in ({}, dict(self.config, JOURNAL_RECORDING_ROOT=str(self.base / 'development'))):
            with self.assertRaisesRegex(OperationError, 'remain mounted'):
                recording.require_continuity(self.config, after)

    def test_compose_and_recovery_selection_retain_overlay(self):
        env = self.stage / '.env.production'
        env.write_text(''.join(k + '=' + v + '\n' for k, v in self.config.items()))
        for name in ('.environment', 'owner.deleted', 'owner-job.json', 'owner-job.wav'):
            (self.path / name).write_bytes(b'private fixture')
        with patch.object(recording, 'check_directory') as check:
            command = compose_command(self.stage, env)
        self.assertEqual(command[-2:], ['-f', str(self.overlay)])
        self.assertEqual(check.call_args_list[-1].args, (self.path, 65532, 0o700))
        recovery = self.root / 'snapshot'
        copy_runtime(self.stage, recovery)
        self.assertEqual((recovery / self.overlay.name).read_bytes(), self.overlay.read_bytes())
        self.assertEqual(len(list(recovery.iterdir())), 2)
        self.assertEqual(len(list(self.path.iterdir())), 4)
        self.assertNotIn(b'private fixture', b''.join(p.read_bytes() for p in recovery.iterdir()))

    def test_missing_overlay_or_directory_fails_before_docker(self):
        self.overlay.unlink()
        with self.assertRaisesRegex(OperationError, 'overlay'):
            recording.compose_overlay(self.stage, self.config)
        self.overlay.write_text('services: {}\n')
        with patch.object(recording, 'check_directory', side_effect=FileNotFoundError()):
            with self.assertRaisesRegex(OperationError, 'provisioned'):
                recording.compose_overlay(self.stage, self.config)

    def test_uid_permissions_and_file_type_are_checked(self):
        path = Mock()
        for mode, uid in ((stat.S_IFDIR | 0o755, 65532), (stat.S_IFDIR | 0o700, 0),
                          (stat.S_IFLNK | 0o700, 65532)):
            path.lstat.return_value = SimpleNamespace(st_mode=mode, st_uid=uid)
            with self.assertRaises(OperationError):
                recording.check_directory(path, 65532, 0o700)
        path.lstat.return_value = SimpleNamespace(st_mode=stat.S_IFDIR | 0o700, st_uid=65532)
        recording.check_directory(path, 65532, 0o700)

    def test_preparing_existing_directory_never_changes_or_erases_it(self):
        sentinel = self.path / 'owner.deleted'
        sentinel.write_bytes(b'')
        with patch.object(recording, 'check_directory') as check, patch.object(os, 'chown') as chown:
            recording.prepare_directory('production')
        chown.assert_not_called()
        check.assert_any_call(self.path, 65532, 0o700)
        self.assertTrue(sentinel.exists())
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o700)

    def test_symlink_mount_is_rejected(self):
        self.path.rmdir()
        self.path.symlink_to(self.stage, target_is_directory=True)
        with patch.object(recording, 'UID', os.getuid()):
            with self.assertRaises(OperationError):
                recording.compose_overlay(self.stage, self.config)

    def test_publicly_writable_ancestor_is_rejected_without_changes(self):
        sentinel = self.path / 'owner.deleted'
        sentinel.write_bytes(b'private fixture')
        self.root.chmod(0o777)
        try:
            with patch.object(os, 'chown') as chown, self.assertRaisesRegex(OperationError, 'ancestors'):
                recording.prepare_directory('production')
            chown.assert_not_called()
            self.assertEqual(sentinel.read_bytes(), b'private fixture')
            self.assertEqual(self.path.stat().st_mode & 0o777, 0o700)
        finally:
            self.root.chmod(0o700)


if __name__ == '__main__':
    unittest.main()
