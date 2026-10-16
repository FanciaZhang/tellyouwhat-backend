import unittest
from configure_sandbox import update_environment

ISSUER = '1f4f7f07-26b1-4226-9518-5bd19f8440b8'


class SandboxConfigTests(unittest.TestCase):
    def test_changes_only_subscription_settings_and_is_idempotent(self):
        original = 'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums\n# preserve\nALBUM_SUBSCRIPTIONS_ENABLED=false\nALBUM_PHOTO_PROCESSING_ENABLED=false\nPRIVATE=opaque\n'
        updated = update_environment(original, ISSUER, 'YTX5GYG2Z5')
        self.assertIn('ALBUM_SUBSCRIPTIONS_ENABLED=true\n', updated)
        self.assertIn('ALBUM_PHOTO_PROCESSING_ENABLED=false\nPRIVATE=opaque\n', updated)
        self.assertEqual(updated, update_environment(updated, ISSUER, 'YTX5GYG2Z5'))

    def test_rejects_duplicate_settings_and_invalid_identity(self):
        for text, issuer, key in [('ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums\nALBUM_SUBSCRIPTIONS_ENABLED=false\nALBUM_SUBSCRIPTIONS_ENABLED=true', ISSUER, 'YTX5GYG2Z5'),
                                  ('', 'bad', 'YTX5GYG2Z5'), ('', ISSUER, 'key\nINJECT=true')]:
            with self.assertRaises(ValueError):
                update_environment(text, issuer, key)

    def test_formal_catalog_cannot_be_installed_into_development(self):
        for text in ['', 'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums.dev',
                     'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.healthapp',
                     'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums\nALBUM_API_HOST=api-test.albums.tellyouwhat.cn',
                     'ALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums\nALBUM_APPLE_BUNDLE_ID=cn.tellyouwhat.albums']:
            with self.subTest(text=text), self.assertRaises(ValueError):
                update_environment(text, ISSUER, 'YTX5GYG2Z5')


if __name__ == '__main__':
    unittest.main()
