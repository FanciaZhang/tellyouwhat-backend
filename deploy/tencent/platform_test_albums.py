"""Attach the shared test API to the existing, independently backed-up Albums store."""

import hashlib
import json
import re

from ops_common import OperationError, read_environment
from albums_artifacts import FILES as ALBUM_FILES, selected_files

HOST = 'api-test.albums.tellyouwhat.cn'
PROJECT = 'tellyouwhat-albums-development'
NETWORK = PROJECT + '_albums-test'
OVERLAY = 'compose.platform-albums-test.yaml'
OVERLAY_SHA256 = '57038263f918d602a7b21cf72838f77fdb79a683a003585b998fe6ac420980a7'
FILES = {OVERLAY: 32768, 'albums.env': 32768}
REGISTRY = {'ALBUM_API_HOST': HOST, 'ALBUM_APPLE_BUNDLE_ID': 'cn.tellyouwhat.albums.dev',
            'ALBUM_APPLE_TEAM_ID': 'T6DD96HY2B'}
COMPOSE_KEYS = {'PLATFORM_ALBUM_ENV_FILE', 'PLATFORM_ALBUM_SOURCE',
                'PLATFORM_ALBUM_MYSQL_PASSWORD', 'PLATFORM_ALBUM_REDIS_PASSWORD'}


def selected(folder):
    return 'albums' in read_environment(folder / 'runtime.env').get('PLATFORM_APPS', '').split(',')


def validate(folder, app, compose):
    if not selected(folder):
        if any(k.startswith('ALBUM_') for k in app) or any(k.startswith('PLATFORM_ALBUM_') for k in compose):
            raise OperationError('Albums credentials require explicit shared test admission')
        return
    values = read_environment(folder / 'albums.env')
    required = {**REGISTRY, 'ALBUM_COS_BUCKET': 'tellyouwhat-albums-1251995834', 'ALBUM_COS_REGION': 'ap-guangzhou',
                'ALBUM_APPLE_SIGN_IN_KEY_ID': '9H4R3FJ9D8',
                'ALBUM_APPLE_SIGN_IN_KEY_FILE': '/album-secrets/apple-sign-in.p8',
                'ALBUM_CREDENTIAL_KEY_FILE': '/album-secrets/credential-key.base64',
                'ALBUM_LOGIN_LIMIT_KEY_FILE': '/album-secrets/login-limit-key.base64'}
    passwords = [compose.get(k, '') for k in ('PLATFORM_ALBUM_MYSQL_PASSWORD', 'PLATFORM_ALBUM_REDIS_PASSWORD')]
    if (app.get('PLATFORM_APPS') != 'health,journal,birthday,albums'
            or any(app.get(k) != v for k, v in REGISTRY.items())
            or any(k.startswith('ALBUM_') and k not in REGISTRY for k in app)
            or any(values.get(k) != v for k, v in required.items())
            or any(not k.startswith('ALBUM_') for k in values)
            or any(k in values for k in ('ALBUM_DATABASE_DSN', 'ALBUM_REDIS_URL', 'ALBUM_TRUSTED_PROXIES', 'ALBUM_DEPLOYMENT_ENVIRONMENT'))
            or compose.get('PLATFORM_ALBUM_ENV_FILE') != str(folder / 'albums.env')
            or not re.fullmatch('[a-f0-9]{40}', compose.get('PLATFORM_ALBUM_SOURCE', ''))
            or any(k.startswith('PLATFORM_ALBUM_') and k not in COMPOSE_KEYS for k in compose)
            or any(not re.fullmatch('[a-f0-9]{64}', p) for p in passwords) or len(set(passwords)) != 2
            or any(p == compose.get(k) for p in passwords for k in ('PLATFORM_TEST_MYSQL_PASSWORD', 'PLATFORM_TEST_MYSQL_ROOT_PASSWORD', 'PLATFORM_TEST_REDIS_PASSWORD'))
            or hashlib.sha256((folder / OVERLAY).read_bytes()).hexdigest() != OVERLAY_SHA256):
        raise OperationError('Albums shared test identity, topology or credential boundary is invalid')


def require_configuration(runtime, folder):
    from platform_test_deploy import private_file
    compose = read_environment(folder / 'compose.env')
    source = compose['PLATFORM_ALBUM_SOURCE']
    original = runtime.root / '.albums-development/releases' / source
    for path in (original, original.parent, original.parent.parent):
        if path.is_symlink() or not path.is_dir() or path.stat().st_mode & 0o077 or path.stat().st_uid != runtime.root.stat().st_uid:
            raise OperationError('Albums development selection must remain private')
    state = json.loads(private_file(runtime, runtime.state / 'albums-development.json', 16384))
    owner = json.loads(private_file(runtime, runtime.state / 'artifact-owners/albums-test.json', 16384))
    if (state.get('source') != source or state.get('release') != str(original)
            or not any(s and s.get('source') == source and str(original) in s.get('paths', [])
                       for s in (owner.get('current'), owner.get('previous')))):
        raise OperationError('shared test Albums resources must match their registered development selection')
    approved = selected_files(runtime, original, source, ALBUM_FILES)
    if approved['runtime.env'] != (folder / 'albums.env').read_bytes():
        raise OperationError('shared test Albums configuration differs from its approved resources')
    previous = read_environment(original / 'compose.env')
    if (previous.get('ALBUM_TEST_SECRET_DIRECTORY') != '/opt/tellyouwhat/albums-development/secrets'
            or previous.get('ALBUM_TEST_MYSQL_USER') != 'albums_development_test'
            or previous.get('ALBUM_TEST_NETWORK_CIDR') != '172.29.92.0/24'
            or any(compose['PLATFORM_ALBUM_' + k] != previous.get('ALBUM_TEST_' + k) for k in ('MYSQL_PASSWORD', 'REDIS_PASSWORD'))):
        raise OperationError('shared test Albums database credentials differ from the existing store')


def require_resources(runtime, folder):
    require_configuration(runtime, folder)
    compose = read_environment(folder / 'compose.env')
    for service in ('mysql', 'redis', 'albumworker'):
        container = json.loads(runtime.execute('platform-test-albums-' + service, ['docker', 'inspect', PROJECT + '-' + service + '-1']))[0]
        if (not container.get('State', {}).get('Running') or container['State'].get('OOMKilled')
                or set(container.get('NetworkSettings', {}).get('Networks', {})) != {NETWORK}
                or container.get('Config', {}).get('Labels', {}).get('com.docker.compose.project') != PROJECT):
            raise OperationError('Albums development database, Redis or media worker is unavailable')
        env = dict(item.split('=', 1) for item in container['Config'].get('Env', []) if '=' in item)
        if service == 'mysql':
            expected = {'MYSQL_DATABASE': 'albums_test', 'MYSQL_USER': 'albums_development_test',
                        'MYSQL_PASSWORD': compose['PLATFORM_ALBUM_MYSQL_PASSWORD']}
        elif service == 'redis':
            expected = {'REDIS_PASSWORD': compose['PLATFORM_ALBUM_REDIS_PASSWORD']}
        else:
            expected = {}
            if container['Config'].get('Image') != 'tellyouwhat-albums-test-development-albumworker:' + compose['PLATFORM_ALBUM_SOURCE']:
                raise OperationError('Albums media worker differs from its registered source')
        if any(env.get(k) != v for k, v in expected.items()):
            raise OperationError('Albums running data service differs from its selected credentials')


def verify_gateway(container, folder):
    env = dict(item.split('=', 1) for item in container.get('Config', {}).get('Env', []) if '=' in item)
    compose = read_environment(folder / 'compose.env')
    expected = {
        'ALBUM_DATABASE_DSN': 'albums_development_test:' + compose['PLATFORM_ALBUM_MYSQL_PASSWORD'] + '@tcp(' + PROJECT + '-mysql-1:3306)/albums_test?charset=utf8mb4&collation=utf8mb4_0900_ai_ci',
        'ALBUM_REDIS_URL': 'redis://:' + compose['PLATFORM_ALBUM_REDIS_PASSWORD'] + '@' + PROJECT + '-redis-1:6379/0',
        'ALBUM_DEPLOYMENT_ENVIRONMENT': 'test', **REGISTRY,
    }
    mounts = [m for m in container.get('Mounts', []) if m.get('Destination') == '/album-secrets']
    if (any(env.get(k) != v for k, v in expected.items())
            or '@tcp(tellyouwhat-platform-test-mysql-1:3306)/platform_test?' not in env.get('DATABASE_DSN', '')
            or '@tellyouwhat-platform-test-redis-1:6379/0' not in env.get('REDIS_URL', '')
            or set(container.get('NetworkSettings', {}).get('Networks', {})) != {'tellyouwhat-platform-test_platform-test', NETWORK}
            or len(mounts) != 1 or mounts[0].get('Source') != '/opt/tellyouwhat/albums-development/secrets'
            or mounts[0].get('Type') != 'bind' or mounts[0].get('RW') is not False):
        raise OperationError('shared test API is not connected to the isolated Albums resources')
