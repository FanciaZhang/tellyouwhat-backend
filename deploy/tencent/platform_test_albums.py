"""Attach the shared test API to the existing, independently backed-up Albums store."""

import hashlib
from datetime import datetime, timedelta
import json
import re
import time
from pathlib import Path

from ops_common import OperationError, read_environment
from albums_artifacts import FILES as ALBUM_FILES, selected_files

HOST = 'api-test.albums.tellyouwhat.cn'
PROJECT = 'tellyouwhat-albums-development'
NETWORK = PROJECT + '_albums-test'
OVERLAY = 'compose.platform-albums-test.yaml'
OVERLAY_SHA256 = 'e7c7dda5dfdafb8d0af14d64d35a2a973019ce5ae096b7c48fcc93b5301a79f5'
ROLLBACK_OVERLAY_SHA256 = '57038263f918d602a7b21cf72838f77fdb79a683a003585b998fe6ac420980a7'
PRODUCT_MIGRATION = 'migrations/0073_album_development_subscription_products.sql'
COPY_MIGRATION = 'migrations/0074_album_system_copy_locators.sql'
FILES = {OVERLAY: 32768, 'albums.env': 32768}
REGISTRY = {'ALBUM_API_HOST': HOST, 'ALBUM_APPLE_BUNDLE_ID': 'cn.tellyouwhat.albums.dev',
            'ALBUM_APPLE_TEAM_ID': 'T6DD96HY2B'}
COMPOSE_KEYS = {'PLATFORM_ALBUM_ENV_FILE', 'PLATFORM_ALBUM_SOURCE',
                'PLATFORM_ALBUM_MYSQL_PASSWORD', 'PLATFORM_ALBUM_REDIS_PASSWORD', 'PLATFORM_ALBUM_WORKER_ENV_FILE'}
WORKER_COMPOSE = 'compose.platform-album-worker.yaml'
WORKER_SHA256 = 'cbd8e5a37bb98f5439662d5b02520dd05fffb93c79e714ff018a6e6cba682598'
WORKER_FILES = {WORKER_COMPOSE: 32768, 'album-worker.env': 8192}
WORKER_VALUES = {'ALBUM_PHOTO_PROCESSING_ENABLED': 'true', 'ALBUM_VIDEO_PROCESSING_ENABLED': 'true',
                 'ALBUM_CI_VIDEO_QUEUE_ID': 'p543daa336f7a4d4b896654b933f8881b',
                 'ALBUM_DERIVED_CLEANUP_ENABLED': 'false', 'ALBUM_ACCOUNT_MEDIA_ERASURE_ENABLED': 'false'}


def worker_selected(folder):
    return any((folder / name).exists() for name in WORKER_FILES)


def worker_values(values):
    keys = {'ALBUM_COS_BUCKET', 'ALBUM_COS_REGION', 'ALBUM_MAX_ORIGINAL_BYTES', 'ALBUM_MAX_DERIVED_BYTES',
            'ALBUM_COS_ARCHIVE_SECRET_ID', 'ALBUM_COS_ARCHIVE_SECRET_KEY',
            'ALBUM_COS_UPLOAD_SECRET_ID', 'ALBUM_COS_UPLOAD_SECRET_KEY'}
    result = {k: values.get(k, '') for k in keys}
    if any(not value for value in result.values()):
        raise OperationError('Albums media requires its existing scoped object credentials and byte limits')
    for key in ('ALBUM_COS_ARCHIVE_SESSION_TOKEN', 'ALBUM_COS_UPLOAD_SESSION_TOKEN'):
        if values.get(key):
            result[key] = values[key]
    if values.get('ALBUM_CI_VIDEO_QUEUE_ID') != WORKER_VALUES['ALBUM_CI_VIDEO_QUEUE_ID']:
        raise OperationError('Albums media must use its approved test processing queue')
    return {**result, **WORKER_VALUES}


def validate_worker(folder, compose, archive):
    if not worker_selected(folder):
        if 'PLATFORM_ALBUM_WORKER_ENV_FILE' in compose:
            raise OperationError('Albums worker configuration is incomplete')
        return
    try:
        recipe = (folder / WORKER_COMPOSE).read_bytes()
        if (not selected(folder) or hashlib.sha256(recipe).hexdigest() != WORKER_SHA256
                or archive.extractfile(WORKER_COMPOSE).read() != recipe
                or compose.get('PLATFORM_ALBUM_WORKER_ENV_FILE') != str(folder / 'album-worker.env')
                or read_environment(folder / 'album-worker.env') != worker_values(read_environment(folder / 'albums.env'))):
            raise OperationError('Albums media selection differs from its reviewed recipe or scoped configuration')
    except (KeyError, AttributeError, FileNotFoundError) as error:
        raise OperationError('Albums media source or configuration is incomplete') from error


def worker_command(folder, *args):
    return ['docker', 'compose', '--project-name', PROJECT, '--env-file', str(folder / 'compose.env'),
            '-f', str(folder / WORKER_COMPOSE), *args]


def verify_worker(runtime, folder, source):
    name = PROJECT + '-albumworker-1'
    container = json.loads(runtime.execute('platform-test-album-worker-inspect', ['docker', 'inspect', name]))[0]
    env = dict(item.split('=', 1) for item in container.get('Config', {}).get('Env', []) if '=' in item)
    compose = read_environment(folder / 'compose.env')
    expected = {**worker_values(read_environment(folder / 'albums.env')), 'ALBUM_DEPLOYMENT_ENVIRONMENT': 'test',
        'ALBUM_DATABASE_DSN': 'albums_development_test:' + compose['PLATFORM_ALBUM_MYSQL_PASSWORD'] + '@tcp(' + PROJECT + '-mysql-1:3306)/albums_test?charset=utf8mb4&collation=utf8mb4_0900_ai_ci'}
    config, host = container.get('Config', {}), container.get('HostConfig', {})
    if (config.get('Image') != 'tellyouwhat-platform-test-albumworker:' + source
            or not container.get('State', {}).get('Running') or container['State'].get('OOMKilled') or container.get('RestartCount')
            or config.get('Labels', {}).get('com.docker.compose.project') != PROJECT
            or config.get('User') != '65532:65532' or not host.get('ReadonlyRootfs')
            or host.get('Memory') != 384 * 1024**2 or container.get('Mounts') or host.get('PortBindings')
            or set(container.get('NetworkSettings', {}).get('Networks', {})) != {NETWORK}
            or any(env.get(k) != v for k, v in expected.items())
            or any(k.startswith(('ALBUM_', 'HEALTH_', 'JOURNAL_', 'BIRTHDAY_')) and k not in expected for k in env)):
        raise OperationError('Albums media worker differs from the isolated selected image or credentials')
    for attempt in range(30):
        started = container['State']['StartedAt']
        until = (datetime.fromisoformat(started.replace('Z', '+00:00')) + timedelta(minutes=1)).isoformat()
        logs = runtime.execute('platform-test-album-worker-startup', ['docker', 'logs', '--since', started, '--until', until, name])
        if any(line.get('msg') == 'album workers started' and line.get('photo_processing') is True
               and line.get('video_processing') is True and line.get('derived_cleanup') is False
               and line.get('account_media_erasure') is False for line in _worker_logs(logs)):
            current = json.loads(runtime.execute('platform-test-album-worker-inspect', ['docker', 'inspect', name]))[0]
            if current.get('State') != container.get('State') or current.get('RestartCount') != container.get('RestartCount'):
                raise OperationError('Albums media worker stopped or restarted during its startup check')
            return
        if attempt == 29:
            raise OperationError('Albums media worker has not completed its database and provider startup checks')
        time.sleep(1)


def _worker_logs(raw):
    for line in raw.splitlines():
        try:
            value = json.loads(line)
            if isinstance(value, dict):
                yield value
        except ValueError:
            continue


def start_worker(runtime, folder, *, recovering=False):
    name = PROJECT + '-albumworker-1'
    runtime.execute('platform-test-album-worker-stop', ['docker', 'stop', '--time', '40', name], timeout=60)
    stopped = json.loads(runtime.execute('platform-test-album-worker-stopped', ['docker', 'inspect', name]))[0]['State']
    if stopped.get('Running') or (not recovering and (stopped.get('OOMKilled') or stopped.get('ExitCode') != 0)):
        raise OperationError('Albums worker did not drain cleanly; restore its previous selection')
    if worker_selected(folder):
        command = worker_command(folder, 'up', '-d', '--no-build', '--no-deps', 'albumworker')
    else:
        source = read_environment(folder / 'compose.env')['PLATFORM_ALBUM_SOURCE']
        original = runtime.root / '.albums-development/releases' / source
        command = ['docker', 'compose', '--project-name', PROJECT, '--env-file', str(original / 'compose.env'),
                   '-f', str(original / 'compose.albums-test.yaml'), 'up', '-d', '--no-build', '--no-deps', 'albumworker']
    runtime.execute('platform-test-album-worker-start', command, timeout=120)


SUBSCRIPTION_SETTINGS = {'ALBUM_SUBSCRIPTIONS_ENABLED', 'ALBUM_APP_STORE_APP_APPLE_ID',
    'ALBUM_APP_STORE_ISSUER_ID', 'ALBUM_APP_STORE_KEY_ID', 'ALBUM_APP_STORE_KEY_FILE', 'ALBUM_APP_STORE_ROOTS_FILE',
    'ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS', 'ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES', 'ALBUM_DEVELOPMENT_CLOUD_EXPIRES_AT'}
SHARE_SITE = 'https://development--tellyouwhat-albums.netlify.app'
SHARING_SETTINGS = {'ALBUM_SHARING_ENABLED', 'ALBUM_SHARE_SITE_ORIGIN', 'ALBUM_SHARE_KEY_FILE', 'ALBUM_SHARE_PREVIOUS_KEY_FILE'}

def sharing_keys(values):
    enabled = values.get('ALBUM_SHARING_ENABLED', 'false')
    if enabled in ('', 'false'):
        return set()
    if (enabled != 'true' or values.get('ALBUM_SUBSCRIPTIONS_ENABLED') != 'true'
            or values.get('ALBUM_SHARE_SITE_ORIGIN') != SHARE_SITE
            or values.get('ALBUM_SHARE_KEY_FILE') != '/album-secrets/share-key.base64'
            or values.get('ALBUM_SHARE_PREVIOUS_KEY_FILE', '') not in ('', '/album-secrets/share-previous-key.base64')):
        raise OperationError('Albums sharing must use the test recipient site, Sandbox and dedicated mounted keys')
    return {'share-key.base64'} | ({'share-previous-key.base64'} if values.get('ALBUM_SHARE_PREVIOUS_KEY_FILE') else set())


def require_transition(before, after):
    old = read_environment(before / ('albums.env' if (before / 'albums.env').exists() else 'runtime.env'))
    new = read_environment(after / 'albums.env')
    if old == new:
        return
    expected = {'ALBUM_SUBSCRIPTIONS_ENABLED': 'true', 'ALBUM_APP_STORE_APP_APPLE_ID': '6824089160',
        'ALBUM_APP_STORE_ISSUER_ID': '1f4f7f07-26b1-4226-9518-5bd19f8440b8', 'ALBUM_APP_STORE_KEY_ID': ('4TVBWNCPNR' if after.name == '0a6a8acd32414df5d55186c4a4f30e42f87f0b5e' else 'YTX5GYG2Z5'),
        'ALBUM_APP_STORE_KEY_FILE': '/album-secrets/album-app-store.p8',
        'ALBUM_APP_STORE_ROOTS_FILE': '/album-secrets/apple-app-store-roots.pem'}
    if (any(new.get(k) != v for k, v in expected.items())
            or any(k.startswith('ALBUM_DEVELOPMENT_CLOUD_') for k in new)
            or {k:v for k,v in old.items() if k not in SUBSCRIPTION_SETTINGS | SHARING_SETTINGS} !=
               {k:v for k,v in new.items() if k not in SUBSCRIPTION_SETTINGS | SHARING_SETTINGS}):
        raise OperationError('Albums subscription change must preserve approved resources and use the reviewed Sandbox catalogue')
    sharing_keys(new)


def selected(folder):
    return 'albums' in read_environment(folder / 'runtime.env').get('PLATFORM_APPS', '').split(',')


def validate(folder, app, compose):
    if not selected(folder):
        if any(k.startswith('ALBUM_') for k in app) or any(k.startswith('PLATFORM_ALBUM_') for k in compose):
            raise OperationError('Albums credentials require explicit shared test admission')
        return
    values = read_environment(folder / 'albums.env')
    sharing_keys(values)
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
            or hashlib.sha256((folder / OVERLAY).read_bytes()).hexdigest() not in (OVERLAY_SHA256, ROLLBACK_OVERLAY_SHA256)):
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
    require_transition(original, folder)
    keys = sharing_keys(read_environment(folder / 'albums.env'))
    secrets = runtime.root.parent / 'albums-development/secrets'
    for name in keys:
        path = secrets / name
        if (secrets.is_symlink() or path.is_symlink() or not path.is_file() or path.stat().st_uid != 0
                or path.stat().st_gid != 65532 or path.stat().st_mode & 0o777 != 0o440
                or not 0 < path.stat().st_size <= 128):
            raise OperationError('Albums sharing requires its private operator-installed recovery key')
    previous = read_environment(original / 'compose.env')
    if (previous.get('ALBUM_TEST_SECRET_DIRECTORY') != '/opt/tellyouwhat/albums-development/secrets'
            or previous.get('ALBUM_TEST_MYSQL_USER') != 'albums_development_test'
            or previous.get('ALBUM_TEST_NETWORK_CIDR') != '172.29.92.0/24'
            or any(compose['PLATFORM_ALBUM_' + k] != previous.get('ALBUM_TEST_' + k) for k in ('MYSQL_PASSWORD', 'REDIS_PASSWORD'))):
        raise OperationError('shared test Albums database credentials differ from the existing store')


def require_resources(runtime, folder):
    require_configuration(runtime, folder)
    compose = read_environment(folder / 'compose.env')
    current = None
    state = runtime.state / 'platform-test.json'
    if state.exists():
        from platform_test_deploy import private_file, selection
        source = json.loads(private_file(runtime, state, 16384))['source']
        active, _, _ = selection(runtime, source)
        if worker_selected(active):
            current = active, source
    if current:
        verify_worker(runtime, *current)
    for service in (('mysql', 'redis') if current else ('mysql', 'redis', 'albumworker')):
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


def schema_plan(runtime, folder, state):
    from platform_test_schema import ADDITIONS, hashes
    source = read_environment(folder / 'compose.env')['PLATFORM_ALBUM_SOURCE']
    base = hashes(runtime.root / '.albums-development/releases' / source)
    candidate = hashes(folder)
    extension = state.get('album_schema_extensions', {})
    reviewed = {name: ADDITIONS[name] for name in (PRODUCT_MIGRATION, COPY_MIGRATION)}
    prefixes = [{}, {PRODUCT_MIGRATION: reviewed[PRODUCT_MIGRATION]}, reviewed]
    if (not base or any(name in base for name in reviewed) or any(candidate.get(k) != v for k, v in base.items())
            or extension not in prefixes or COPY_MIGRATION in candidate and PRODUCT_MIGRATION not in candidate):
        raise OperationError('Albums schema differs from its registered base or reviewed extension')
    target = dict(extension)
    steps = [dict(target)]
    for name, digest in reviewed.items():
        if name in candidate:
            if (candidate[name] != digest
                    or hashlib.sha256((folder / OVERLAY).read_bytes()).hexdigest() != OVERLAY_SHA256):
                raise OperationError('Albums metadata migration requires its reviewed SQL and isolated job')
            target[name] = digest
            steps.append(dict(target))
    names = lambda extra: sorted(Path(k).name for k in base | extra)
    actual = applied_schema(runtime)
    if actual not in [names(step) for step in steps]:
        raise OperationError('Albums applied schema differs from the registered recovery selection')
    return names(target), target, actual != names(target)


def applied_schema(runtime):
    return runtime.execute('platform-test-album-schema', [
        'docker', 'exec', PROJECT + '-mysql-1', 'sh', '-c',
        'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql --user="$MYSQL_USER" --database=albums_test '
        '--batch --skip-column-names -e "SELECT name FROM schema_migrations ORDER BY name;"']).decode().splitlines()


def verify_gateway(container, folder):
    env = dict(item.split('=', 1) for item in container.get('Config', {}).get('Env', []) if '=' in item)
    compose = read_environment(folder / 'compose.env')
    values = read_environment(folder / 'albums.env')
    keys = sharing_keys(values)
    expected = {
        'ALBUM_DATABASE_DSN': 'albums_development_test:' + compose['PLATFORM_ALBUM_MYSQL_PASSWORD'] + '@tcp(' + PROJECT + '-mysql-1:3306)/albums_test?charset=utf8mb4&collation=utf8mb4_0900_ai_ci',
        'ALBUM_REDIS_URL': 'redis://:' + compose['PLATFORM_ALBUM_REDIS_PASSWORD'] + '@' + PROJECT + '-redis-1:6379/0',
        'ALBUM_DEPLOYMENT_ENVIRONMENT': 'test', **REGISTRY,
    }
    if (env.get('ALBUM_SHARING_ENABLED') or 'false') != (values.get('ALBUM_SHARING_ENABLED') or 'false'):
        raise OperationError('running Albums sharing differs from the selected configuration')
    if keys:
        expected.update({k: values.get(k, '') for k in SHARING_SETTINGS})
        expected['ALBUM_SUBSCRIPTIONS_ENABLED'] = 'true'
        env.setdefault('ALBUM_SHARE_PREVIOUS_KEY_FILE', '')
    mounts = [m for m in container.get('Mounts', []) if m.get('Destination') == '/album-secrets']
    if (any(env.get(k) != v for k, v in expected.items())
            or '@tcp(tellyouwhat-platform-test-mysql-1:3306)/platform_test?' not in env.get('DATABASE_DSN', '')
            or '@tellyouwhat-platform-test-redis-1:6379/0' not in env.get('REDIS_URL', '')
            or set(container.get('NetworkSettings', {}).get('Networks', {})) != {'tellyouwhat-platform-test_platform-test', NETWORK}
            or len(mounts) != 1 or mounts[0].get('Source') != '/opt/tellyouwhat/albums-development/secrets'
            or mounts[0].get('Type') != 'bind' or mounts[0].get('RW') is not False):
        raise OperationError('shared test API is not connected to the isolated Albums resources')
