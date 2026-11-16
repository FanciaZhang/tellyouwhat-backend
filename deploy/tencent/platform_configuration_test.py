import tempfile
from pathlib import Path
import unittest

from platform_configuration import configure


class PlatformConfigurationTests(unittest.TestCase):
    def test_dedicated_flag_preserves_offer_and_credentials(self):
        with tempfile.TemporaryDirectory() as root:
            target = Path(root) / '.env.production'
            original = 'ADMIN_WRITES_ENABLED=false\nCREDENTIAL=fixture\n'
            target.write_text(original + 'PLATFORM_OPERATIONS_WRITES_ENABLED=false\n')
            target.chmod(0o600)
            configure(target, 'true')
            self.assertEqual(target.read_text(), original + 'PLATFORM_OPERATIONS_WRITES_ENABLED=true\n')
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
            configure(target, '')
            self.assertIn('PLATFORM_OPERATIONS_WRITES_ENABLED=true', target.read_text())
            with self.assertRaises(ValueError):
                configure(target, 'true\nADMIN_WRITES_ENABLED=true')
            self.assertTrue(target.read_text().startswith(original))

    def test_journal_quota_override_preserves_other_settings_and_rejects_invalid_values(self):
        with tempfile.TemporaryDirectory() as root:
            target = Path(root) / '.env.production'
            original = 'CREDENTIAL=fixture\nJOURNAL_TTS_MONTHLY_CHARACTERS=10000\n'
            target.write_text(original)
            target.chmod(0o600)
            configure(target, '', '50000')
            self.assertEqual(target.read_text(), 'CREDENTIAL=fixture\nJOURNAL_TTS_MONTHLY_CHARACTERS=50000\n')
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
            for value in ('0', '100000001', '-1', '5\nCREDENTIAL=other', '５０'):
                with self.subTest(value=value), self.assertRaises(ValueError):
                    configure(target, 'true', value)
                self.assertEqual(target.read_text(), 'CREDENTIAL=fixture\nJOURNAL_TTS_MONTHLY_CHARACTERS=50000\n')
            configure(target, '', '')
            self.assertIn('JOURNAL_TTS_MONTHLY_CHARACTERS=50000', target.read_text())
