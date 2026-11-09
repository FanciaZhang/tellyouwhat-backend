import base64
import datetime as dt
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import patch
import uuid

import platform_test_journal as journal
from ops_common import OperationError


@unittest.skipUnless(os.geteuid() == 0 and os.environ.get('JOURNAL_PRIVATE_PREPARE_TEST_ROOT'),
                     'requires an explicitly isolated private Linux root test directory')
class JournalReadAloudPrivatePreparationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(dir=os.environ['JOURNAL_PRIVATE_PREPARE_TEST_ROOT'])
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.backend = self.base / 'backend'
        self.backend.mkdir(mode=0o700)
        self.private = self.base / 'read-aloud'
        self.folder = self.backend / '.platform-test/releases' / ('a' * 40)
        self.folder.mkdir(parents=True, mode=0o700)
        self.environment = self.folder / 'runtime.env'
        self.settings = {**journal.FIXED, 'JOURNAL_RECORDING_ENABLED': 'false', 'JOURNAL_ALLOWED_APP_BUILDS': '26'}
        self.settings.update({k: 'ep-journal-test' if k.endswith('_MODEL_ID') else 'journal-speech-test-key'
                              for k in journal.RESOURCE_KEYS})
        self.settings.update(JOURNAL_IMAGE_MAX_COST_NANOS='100000000', JOURNAL_TTS_ENABLED='true',
                             JOURNAL_TTS_API_KEY_FILE=journal.KEY_FILE, JOURNAL_TTS_MONTHLY_CHARACTERS='10000',
                             JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS='1000',
                             JOURNAL_READ_ALOUD_SIMULATOR_CREDENTIALS_FILE=journal.SIMULATOR_FILE)
        self.approved = self.base / 'journal-development/runtime.env'
        self.approved.parent.mkdir(mode=0o700)
        self.approved.write_text('JOURNAL_VOICE_ASR_API_KEY=journal-speech-test-key\n')
        self.approved.chmod(0o600)
        self.save()
        for name, value in [('BACKEND_ROOT', self.backend), ('PRIVATE_BASE', self.private)]:
            patched = patch.object(journal, name, value)
            patched.start()
            self.addCleanup(patched.stop)

    def save(self):
        self.environment.write_text(''.join(k + '=' + v + '\n' for k, v in self.settings.items()))
        self.environment.chmod(0o600)

    def prepare(self):
        journal.prepare_private_read_aloud(self.environment)

    def test_creates_private_key_and_empty_registry_without_granting_access(self):
        self.prepare()
        root = self.private / 'development'
        self.assertEqual(stat.S_IMODE(root.stat().st_mode), 0o700)
        self.assertEqual(root.stat().st_uid, journal.SERVICE_UID)
        for name in ['api-key.txt', 'simulators.json']:
            info = (root / name).stat()
            self.assertEqual(info.st_uid, journal.SERVICE_UID)
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
        self.assertEqual((root / 'api-key.txt').read_text().strip(), 'journal-speech-test-key')
        self.assertEqual(json.loads((root / 'simulators.json').read_text()), {'version': 1, 'credentials': []})

    def test_repeated_prepare_and_disabled_rollback_preserve_atomic_revocations(self):
        self.prepare()
        root = self.private / 'development'
        now = dt.datetime.now(dt.timezone.utc)
        credential = {'keyID': 'simulator_' + str(uuid.uuid4()), 'subjectID': str(uuid.uuid4()),
                      'publicKey': base64.b64encode(bytes(range(32))).decode(), 'revoked': True,
                      'issuedAt': now.isoformat(), 'expiresAt': (now + dt.timedelta(hours=1)).isoformat(),
                      'journalCharacters': 10000, 'previewCharacters': 1000}
        body = json.dumps({'version': 1, 'credentials': [credential]}).encode()
        candidate = root / 'simulators.next'
        candidate.write_bytes(body)
        candidate.chmod(0o600)
        os.chown(candidate, journal.SERVICE_UID, journal.SERVICE_UID)
        candidate.replace(root / 'simulators.json')
        self.prepare()
        self.settings['JOURNAL_TTS_ENABLED'] = 'false'
        self.save()
        self.prepare()
        self.assertEqual((root / 'simulators.json').read_bytes(), body)

    def test_rotation_public_permissions_and_symlinks_are_rejected_without_overwrite(self):
        self.prepare()
        key = self.private / 'development/api-key.txt'
        original = key.read_bytes()
        key.chmod(0o644)
        with self.assertRaises(OperationError):
            self.prepare()
        self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o644)
        key.chmod(0o600)
        self.settings['JOURNAL_SPEECH_API_KEY'] = 'different-approved-test-key'
        self.approved.write_text('JOURNAL_VOICE_ASR_API_KEY=different-approved-test-key\n')
        self.save()
        with self.assertRaisesRegex(OperationError, 'explicit rotation'):
            self.prepare()
        self.assertEqual(key.read_bytes(), original)
        real = key.with_suffix('.real')
        key.rename(real)
        key.symlink_to(real)
        with self.assertRaises(OperationError):
            self.prepare()
        self.assertTrue(key.is_symlink())
        self.assertEqual(real.read_bytes(), original)

    def test_unapproved_resource_and_unsafe_directory_are_not_repaired(self):
        self.prepare()
        root = self.private / 'development'
        root.chmod(0o755)
        with self.assertRaises(OperationError):
            self.prepare()
        self.assertEqual(stat.S_IMODE(root.stat().st_mode), 0o755)
        root.chmod(0o700)
        self.settings['JOURNAL_SPEECH_API_KEY'] = 'unapproved'
        self.save()
        with self.assertRaisesRegex(OperationError, 'approved test'):
            self.prepare()


if __name__ == '__main__':
    unittest.main()
