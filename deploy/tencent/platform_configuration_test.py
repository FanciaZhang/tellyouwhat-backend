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
