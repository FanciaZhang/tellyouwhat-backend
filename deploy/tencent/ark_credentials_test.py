import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from ark_credentials import install, materialize
from ops_common import OperationError, compose_command, read_environment


class ArkCredentialTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.stage = self.root / '.incoming-new'
        self.stage.mkdir()
        self.env = self.stage / '.env.production'
        self.env.write_text('AI_CONFIG_WRITES_ENABLED=true\nAI_ENDPOINT_WRITES_ENABLED=true\n')
        self.source = self.stage / 'ark-management.json'

    def materialize(self, value='{"accessKey":"fixture-ak","secretKey":"fixture-sk"}'):
        with patch.dict(os.environ, ARK_SERVICE_CREDENTIALS=value):
            materialize(self.source)

    def test_materialize_private_file_and_reject_invalid_secrets(self):
        self.materialize()
        self.assertEqual(self.source.stat().st_mode & 0o777, 0o600)
        self.source.unlink()
        for value in ['not-json', '{}', '{"accessKey":"ak","secretKey":"sk","extra":1}', '{"accessKey":7,"secretKey":"sk"}']:
            with self.assertRaises(OperationError):
                self.materialize(value)
            self.assertFalse(self.source.exists())

    def test_missing_secret_rejects_enabled_native_writes(self):
        with self.assertRaisesRegex(OperationError, 'required'):
            install(self.stage, self.root)
        self.env.write_text('AI_ENDPOINT_WRITES_ENABLED=false\n')
        install(self.stage, self.root)

    def test_install_uses_versioned_admin_only_file_and_preserves_flags(self):
        self.materialize()
        with patch('ark_credentials.subprocess.run') as run:
            install(self.stage, self.root)
        calls = [call.args[0] for call in run.call_args_list]
        self.assertIn('65532', calls[1])
        self.assertIn('0600', calls[1])
        config = read_environment(self.env)
        self.assertEqual(config['AI_ENDPOINT_WRITES_ENABLED'], 'true')
        self.assertEqual(config['ARK_MANAGEMENT_CREDENTIAL_FILE'], '/run/ark-management/credentials.json')
        self.assertEqual(Path(config['ARK_MANAGEMENT_CREDENTIAL_HOST_FILE']).parent, self.root / '.ark-management')
        self.assertNotIn('fixture-sk', self.env.read_text())
        self.assertFalse(self.source.exists())

    def test_install_failure_preserves_environment_and_removes_staged_secret(self):
        self.materialize()
        before = self.env.read_text()
        with patch('ark_credentials.subprocess.run', side_effect=OSError('fixture')):
            with self.assertRaises(OSError):
                install(self.stage, self.root)
        self.assertEqual(self.env.read_text(), before)
        self.assertFalse(self.source.exists())

    def test_compose_rejects_incomplete_mount_before_runtime_mutation(self):
        with self.assertRaises(OperationError):
            compose_command(self.stage, self.env)
        self.materialize()
        self.env.write_text('ARK_MANAGEMENT_CREDENTIAL_HOST_FILE=' + str(self.source) + '\n')
        with self.assertRaises(OperationError):
            compose_command(self.stage, self.env)


if __name__ == '__main__':
    unittest.main()
