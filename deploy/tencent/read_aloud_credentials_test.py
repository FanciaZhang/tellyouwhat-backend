import os
from pathlib import Path
import stat
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from ops_common import OperationError, read_environment
from read_aloud_credentials import CONTAINER_FILE, install, materialize, validate_config


class ReadAloudCredentialTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.stage = self.root / '.incoming-new'
        self.stage.mkdir()
        self.environment = self.stage / '.env.production'
        self.environment.write_text('JOURNAL_TTS_ENABLED=true\nJOURNAL_TTS_MONTHLY_CHARACTERS=100000\nHEALTH_APPLE_BUNDLE_ID=preserved\n')
        self.source = self.stage / 'journal-tts-api-key.txt'

    def materialize(self, key='production-key-fixture'):
        with patch.dict(os.environ, JOURNAL_TTS_API_KEY=key):
            materialize(self.source)

    def test_private_materialization_rejects_bad_and_oversized_keys(self):
        for key in [' ', 'has space', 'has\nnewline', '中文', 'x' * 4097]:
            with self.assertRaises(OperationError):
                self.materialize(key)
            self.assertFalse(self.source.exists())
        self.materialize()
        self.assertEqual(stat.S_IMODE(self.source.stat().st_mode), 0o600)
        with self.assertRaises(FileExistsError):
            self.materialize('different-key')
        self.assertEqual(self.source.read_text(), 'production-key-fixture')

    def test_optional_legacy_configuration_and_missing_enabled_secret(self):
        validate_config({})
        validate_config({'JOURNAL_TTS_ENABLED': 'false'})
        with self.assertRaises(OperationError):
            install(self.stage, self.root)
        self.environment.write_text('JOURNAL_TTS_ENABLED=false\n')
        install(self.stage, self.root)

    def test_removed_secret_cannot_reuse_a_stale_staging_file(self):
        self.materialize()
        unrelated = self.stage / 'other-input.txt'
        unrelated.write_text('preserved')
        self.materialize('')
        self.assertFalse(self.source.exists())
        self.assertEqual(unrelated.read_text(), 'preserved')
        with self.assertRaises(OperationError):
            install(self.stage, self.root)

    def test_install_preserves_other_apps_and_binds_immutable_private_key(self):
        self.materialize()
        with patch('read_aloud_credentials.subprocess.run') as run:
            install(self.stage, self.root)
        calls = [c.args[0] for c in run.call_args_list]
        self.assertIn('0600', calls[1])
        self.assertIn('65532', calls[1])
        config = read_environment(self.environment)
        self.assertEqual(config['JOURNAL_TTS_API_KEY_FILE'], CONTAINER_FILE)
        self.assertEqual(Path(config['JOURNAL_TTS_API_KEY_HOST_FILE']).parent, self.root / '.journal-read-aloud')
        self.assertEqual(config['HEALTH_APPLE_BUNDLE_ID'], 'preserved')
        self.assertNotIn('production-key-fixture', self.environment.read_text())
        self.assertFalse(self.source.exists())

    def test_failure_removes_only_staged_key_and_keeps_original_configuration(self):
        self.materialize()
        before = self.environment.read_bytes()
        original = self.root / 'original-key.txt'
        original.write_text('preserved')
        with patch('read_aloud_credentials.subprocess.run', side_effect=OSError('fixture')):
            with self.assertRaises(OSError):
                install(self.stage, self.root)
        self.assertEqual(self.environment.read_bytes(), before)
        self.assertEqual(original.read_text(), 'preserved')
        self.assertFalse(self.source.exists())

    def test_mount_rejects_development_keys_unsafe_permissions_and_unbounded_quotas(self):
        config = {'JOURNAL_TTS_ENABLED': 'true', 'JOURNAL_TTS_API_KEY_FILE': CONTAINER_FILE,
                  'JOURNAL_TTS_API_KEY_HOST_FILE': str(self.root / '.journal-read-aloud' / ('a' * 32 + '.txt')),
                  'JOURNAL_TTS_MONTHLY_CHARACTERS': '100000'}
        info = SimpleNamespace(st_uid=65532, st_mode=stat.S_IFREG | 0o600, st_size=20)
        with patch('read_aloud_credentials.Path.lstat', return_value=info):
            validate_config(config)
            for key, value in [('APP_ENV', 'development'), ('JOURNAL_TTS_API_KEY_HOST_FILE', '/opt/tellyouwhat/journal-read-aloud/development/api-key.txt'),
                               ('JOURNAL_TTS_API_KEY_FILE', '/album-secrets/key'), ('JOURNAL_TTS_ENABLED', 'yes'),
                               ('JOURNAL_TTS_MONTHLY_CHARACTERS', '0'), ('JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS', '1000001')]:
                with self.subTest(key=key), self.assertRaises(OperationError):
                    validate_config({**config, key: value})
        for uid, mode in [(0, 0o600), (65532, 0o640), (65532, 0o666)]:
            with patch('read_aloud_credentials.Path.lstat', return_value=SimpleNamespace(st_uid=uid, st_mode=stat.S_IFREG | mode, st_size=20)):
                with self.assertRaises(OperationError):
                    validate_config(config)


if __name__ == '__main__':
    unittest.main()
