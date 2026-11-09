"""Parse production topology with native Compose and synthetic credentials only."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


@unittest.skipUnless(shutil.which('docker'), 'native Docker Compose is unavailable')
class AlbumsComposeTests(unittest.TestCase):
    def test_shared_gateway_and_data_only_production_topology(self):
        source = Path(__file__).resolve().parents[2]
        with tempfile.TemporaryDirectory(prefix='albums-compose-') as directory:
            root = Path(directory)
            for name in ('compose.production.yaml', 'compose.shared-albums-production.yaml', 'compose.albums-production.yaml'):
                shutil.copy2(source / name, root / name)
            (root / '.env.production').write_text('APP_ENV=production\n')
            (root / 'albums.env').write_text('ALBUM_COS_ARCHIVE_SECRET_KEY=fixture-only-albums-secret\n')
            env = {**os.environ, 'MYSQL_USER': 'shared', 'MYSQL_PASSWORD': 'shared-fixture', 'MYSQL_DATABASE': 'shared',
                   'MYSQL_HOST': '10.0.0.6', 'REDIS_HOST': '10.0.0.4', 'REDIS_PASSWORD': 'redis-fixture',
                   'HEALTH_API_DOMAIN': 'health.example', 'JOURNAL_API_DOMAIN': 'journal.example',
                   'ADMIN_DOMAIN': 'admin.example', 'ACME_EMAIL': 'ops@example.com',
                   'ALBUM_PRODUCTION_ENV_FILE': str(root / 'albums.env'), 'ALBUM_PRODUCTION_IMAGE_TAG': 'a' * 40,
                   'ALBUM_PRODUCTION_MYSQL_PASSWORD': '1' * 64, 'ALBUM_PRODUCTION_MYSQL_ROOT_PASSWORD': '3' * 64,
                   'ALBUM_PRODUCTION_REDIS_PASSWORD': '2' * 64}

            def parse(*files):
                command = ['docker', 'compose', '--profile', '*']
                for name in files:
                    command.extend(['-f', str(root / name)])
                result = subprocess.run([*command, 'config', '--format', 'json'], env=env, cwd=root,
                                        capture_output=True, text=True, check=True)
                return json.loads(result.stdout)

            shared = parse('compose.production.yaml', 'compose.shared-albums-production.yaml')
            gateway = shared['services']['gateway']
            self.assertEqual(gateway['environment']['ALBUM_COS_ARCHIVE_SECRET_KEY'], 'fixture-only-albums-secret')
            self.assertEqual(set(gateway['networks']), {'tellyouwhat-backend', 'albums-production'})
            self.assertEqual(int(gateway['mem_limit']), 512 * 1024**2)
            self.assertIn('@tcp(10.0.0.6:', gateway['environment']['DATABASE_DSN'])
            mounts = [v for v in gateway['volumes'] if v['target'] == '/album-secrets']
            self.assertEqual(len(mounts), 1)
            self.assertTrue(mounts[0]['read_only'])
            self.assertFalse(mounts[0].get('bind', {}).get('create_host_path', False))
            self.assertTrue(shared['networks']['albums-production']['external'])
            for name, service in shared['services'].items():
                if name != 'gateway':
                    self.assertNotIn('ALBUM_COS_ARCHIVE_SECRET_KEY', service.get('environment', {}))
                    self.assertNotIn('albums-production', service.get('networks', {}))
                    self.assertFalse(any(v['target'] == '/album-secrets' for v in service.get('volumes', [])))
            data = parse('compose.albums-production.yaml')
            self.assertEqual(set(data['services']), {'mysql', 'redis', 'migrate', 'albumworker'})
            self.assertFalse(any(service.get('ports') for service in data['services'].values()))
            self.assertFalse(data['services']['albumworker'].get('volumes'))
            self.assertNotIn('3' * 64, json.dumps(data['services']['albumworker']))


if __name__ == '__main__':
    unittest.main()
