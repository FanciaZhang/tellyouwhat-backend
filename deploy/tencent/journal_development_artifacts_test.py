import copy
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import journal_development_artifacts as journal
from ops_common import OperationError


class JournalRecoveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name) / 'backend'
        self.root.mkdir(mode=0o700)
        self.runtime = Mock(root=self.root, state=self.root / '.operations', config={'BACKUP_ENCRYPTION_KEY': '0' * 64})
        self.runtime.state.mkdir(mode=0o700)
        self.app = self.root.parent / 'journal-development'
        self.app.mkdir(mode=0o700)
        self.key = self.app / 'backup.env'
        self.key.write_text('BACKUP_ENCRYPTION_KEY=' + '1' * 64 + '\n')
        self.key.chmod(0o600)
        (self.app / 'bin').mkdir()
        (self.app / 'config').mkdir()
        self.program = b'fixture reviewed executable'
        self.binary = self.app / 'bin' / ('journaldevserver-' + journal.digest(self.program)[:16])
        self.binary.write_bytes(self.program)
        self.image = 'ghcr.io/fanciazhang/tellyouwhat-ai-gateway@sha256:' + 'a' * 64
        self.state = {'source': 'b' * 40, 'binary': str(self.binary), 'image': self.image}
        self.save(self.runtime.state / 'journal-development.json', self.state)
        self.owner = {'current': {'source': self.state['source'], 'source_run': 'test:binary-sha256:' + journal.digest(self.program),
                                 'images': ['sha256:pinned'], 'paths': [str(self.binary)]}}
        self.save(self.runtime.state / 'artifact-owners/journal-development.json', self.owner)
        self.settings = self.app / 'runtime.env'
        self.settings.write_text('JOURNAL_DEVELOPMENT_RUNTIME_IMAGE=' + self.image + '\nJOURNAL_ARK_API_KEY=restricted-provider-key\n')
        self.settings.chmod(0o600)
        self.save(self.app / 'config/prompt.json', {'scope': 'journal', 'publishedAt': '2026-10-16T00:00:00Z'})
        self.container = {'State': {'Running': True}, 'Image': 'sha256:pinned', 'Config': {'Image': self.image, 'User': '65532:65532',
                          'Env': ['JOURNAL_ARK_API_KEY=restricted-provider-key', 'JOURNAL_DEVELOPMENT_TOKEN=never-back-up-connection',
                                  'JOURNAL_PROMPT_FILE=/journal-config/prompt.json']},
                          'Mounts': [{'Destination': '/journaldevserver', 'Source': str(self.binary), 'RW': False},
                                     {'Destination': '/journal-config', 'Source': str(self.app / 'config'), 'RW': False}]}
        self.events = [{'now': '2026-10-16T00:00:00Z', 'id': 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'actual': 17, 'known': True}]
        self.runtime.execute.side_effect = self.execute

    def save(self, path, value):
        path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        path.write_text(json.dumps(value))
        path.chmod(0o600)

    def execute(self, label, command, **kwargs):
        if label == 'backup-journal-runtime':
            return json.dumps([self.container]).encode()
        if label == 'backup-crypto':
            result = subprocess.run(command, env=kwargs['env'], capture_output=True)
            if result.returncode:
                raise OperationError('crypto failed')
            return result.stdout
        self.fail('unexpected command: ' + label)

    def backup(self, name='checkpoint'):
        target = self.root / name
        with patch.object(journal, 'cost_snapshot', return_value=json.dumps(self.events).encode()):
            hashes = journal.backup_configuration(self.runtime, target)
        return target, hashes

    def test_absent_marker_does_not_change_existing_backups(self):
        (self.runtime.state / 'journal-development.json').unlink()
        self.assertEqual(journal.backup_configuration(self.runtime, self.root / 'unused'), {})
        self.runtime.execute.assert_not_called()

    def test_checkpoint_retains_exact_program_policy_and_cost_but_excludes_connections_consents_and_key(self):
        target, hashes = self.backup()
        self.assertEqual(set(hashes), {journal.CHECKPOINT, journal.ENVELOPE})
        data = journal.read_checkpoint(self.runtime, target)
        self.assertEqual(set(data), journal.FILES)
        self.assertEqual(data['journaldevserver'], self.program)
        self.assertEqual(json.loads(data['cost-events.json']), self.events)
        self.assertTrue(json.loads(data['selection.json'])['new_connection_required_after_restore'])
        for value in data.values():
            self.assertNotIn(b'never-back-up-connection', value)
            self.assertNotIn(('1' * 64).encode(), value)

    def test_wrong_inner_key_and_modified_checkpoint_fail_before_recovery(self):
        target, _ = self.backup()
        self.key.write_text('BACKUP_ENCRYPTION_KEY=' + '2' * 64 + '\n')
        with self.assertRaisesRegex(OperationError, 'authentication'):
            journal.read_checkpoint(self.runtime, target)
        self.key.write_text('BACKUP_ENCRYPTION_KEY=' + '1' * 64 + '\n')
        (target / journal.CHECKPOINT).write_bytes(b'tampered')
        with self.assertRaisesRegex(OperationError, 'authentication'):
            journal.read_checkpoint(self.runtime, target)

    def test_database_access_unmatched_source_and_mutable_configuration_are_rejected(self):
        self.container['Config']['Env'].append('JOURNAL_PROMPT_DATABASE_DSN=do-not-export')
        with self.assertRaisesRegex(OperationError, 'boundary'):
            self.backup()
        self.container['Config']['Env'].pop()
        self.owner['current']['source'] = 'c' * 40
        self.save(self.runtime.state / 'artifact-owners/journal-development.json', self.owner)
        with self.assertRaisesRegex(OperationError, 'registered binary'):
            self.backup()
        self.owner['current']['source'] = self.state['source']
        self.save(self.runtime.state / 'artifact-owners/journal-development.json', self.owner)
        self.settings.chmod(0o644)
        with self.assertRaises(OperationError):
            self.backup()

    def test_accounting_cannot_smuggle_content_into_backup(self):
        self.assertEqual(journal.validate_cost(b'null'), 0)
        for mutate in [lambda e: e.update(transcript='private content'),
                       lambda e: e.update(actual={'private': 'content'}),
                       lambda e: e.update(reserve={'input': 'private content'}),
                       lambda e: e.update(reserve={'Operation': 'private personal text'})]:
            events = copy.deepcopy(self.events)
            mutate(events[0])
            with self.assertRaises(OperationError):
                journal.validate_cost(json.dumps(events))

    def test_outer_recovery_requires_the_complete_authenticated_pair(self):
        target, hashes = self.backup()
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode='w') as output:
            for name in hashes:
                output.add(target / name, arcname='recovery/journal-development/' + name)
        buffer.seek(0)
        with tarfile.open(fileobj=buffer) as archive:
            journal.verify_recovery(archive, hashes)
            with self.assertRaises(OperationError):
                journal.verify_recovery(archive, {journal.CHECKPOINT: hashes[journal.CHECKPOINT]})
            with self.assertRaises(OperationError):
                journal.verify_recovery(archive, {**hashes, journal.ENVELOPE: '0' * 64})


if __name__ == '__main__':
    unittest.main()
