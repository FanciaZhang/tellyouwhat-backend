import copy
from pathlib import Path
import tempfile
import unittest

import platform_test_albums as albums
from ops_common import OperationError


class AlbumsSharedGatewayRuntimeTests(unittest.TestCase):
    def test_actual_connections_and_read_only_secret_mount_are_required(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            (folder / 'compose.env').write_text('PLATFORM_ALBUM_MYSQL_PASSWORD=' + '4' * 64 +
                                               '\nPLATFORM_ALBUM_REDIS_PASSWORD=' + '5' * 64 + '\n')
            env = {**albums.REGISTRY, 'ALBUM_DEPLOYMENT_ENVIRONMENT': 'test',
                   'DATABASE_DSN': 'platform_test:synthetic@tcp(tellyouwhat-platform-test-mysql-1:3306)/platform_test?parseTime=true',
                   'REDIS_URL': 'redis://:synthetic@tellyouwhat-platform-test-redis-1:6379/0',
                   'ALBUM_DATABASE_DSN': 'albums_development_test:' + '4' * 64 + '@tcp(tellyouwhat-albums-development-mysql-1:3306)/albums_test?charset=utf8mb4&collation=utf8mb4_0900_ai_ci',
                   'ALBUM_REDIS_URL': 'redis://:' + '5' * 64 + '@tellyouwhat-albums-development-redis-1:6379/0'}
            container = {'Config': {'Env': [k + '=' + v for k, v in env.items()]},
                         'NetworkSettings': {'Networks': {'tellyouwhat-platform-test_platform-test': {}, albums.NETWORK: {}}},
                         'Mounts': [{'Type': 'bind', 'Source': '/opt/tellyouwhat/albums-development/secrets',
                                     'Destination': '/album-secrets', 'RW': False}]}
            albums.verify_gateway(container, folder)
            for field, bad in [('DATABASE_DSN', 'platform_test:synthetic@tcp(mysql:3306)/platform_test'),
                               ('ALBUM_REDIS_URL', 'redis://localhost:6379/0'),
                               ('ALBUM_APPLE_BUNDLE_ID', 'cn.tellyouwhat.albums'),
                               ('ALBUM_DEPLOYMENT_ENVIRONMENT', 'production')]:
                changed = copy.deepcopy(container)
                changed['Config']['Env'] = [k + '=' + v for k, v in {**env, field: bad}.items()]
                with self.subTest(field=field), self.assertRaises(OperationError):
                    albums.verify_gateway(changed, folder)
            for field, bad in [('RW', True), ('Source', '/opt/tellyouwhat/albums-production/secrets')]:
                changed = copy.deepcopy(container)
                changed['Mounts'][0][field] = bad
                with self.subTest(field=field), self.assertRaises(OperationError):
                    albums.verify_gateway(changed, folder)
            container['NetworkSettings']['Networks']['production'] = {}
            with self.assertRaises(OperationError):
                albums.verify_gateway(container, folder)


if __name__ == '__main__':
    unittest.main()
