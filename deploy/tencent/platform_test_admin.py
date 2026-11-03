"""Unified administration uses only the existing isolated test resources."""

import base64
import hashlib
import json
import re
import urllib.request

from ops_common import OperationError, read_environment
import platform_test_journal as journal

OVERLAY = 'compose.platform-admin-test.yaml'
OVERLAY_SHA256 = 'ce7c101ee6bfa2cb04b0e5bfca5676f3d1f515d5bedce98e5e67235874dc8636'
ROLLBACK_OVERLAY_SHA256 = 'd8b51b450846c7b28553ee05d98b0afcc8c607627e7b50b7d4dfab706683d51e'
HOST = 'admin-test.tellyouwhat.cn'
FILES = {OVERLAY: 32768, 'admin.env': 8192}
MODELS = {'JOURNAL_ARK_LITE_MODEL_ID', 'JOURNAL_ARK_PRO_MODEL_ID', 'JOURNAL_VOICE_MODEL_ID'}
KEY_FILE = '/journal-api-key.txt'
FIXED = {'APP_ENV': 'development', 'ADMIN_PORT': '8082', 'ADMIN_APPS': 'health,journal,birthday,albums',
         'ADMIN_RP_ID': HOST, 'ADMIN_ORIGIN': 'https://' + HOST,
         'ADMIN_WRITES_ENABLED': 'true', 'PLATFORM_OPERATIONS_WRITES_ENABLED': 'true',
         'HEALTH_APP_STORE_CONNECT_ENABLED': 'false', 'BIRTHDAY_APP_STORE_CONNECT_ENABLED': 'false',
         'ALBUM_APP_STORE_CONNECT_ENABLED': 'false', 'JOURNAL_APP_STORE_CONNECT_ENABLED': 'false',
         'JOURNAL_TTS_WRITES_ENABLED': 'true', 'JOURNAL_TTS_API_KEY_FILE': KEY_FILE,
         'JOURNAL_TTS_PROBE_DAILY_CHARACTERS': '10000', 'AI_PROJECT_MONTHLY_BUDGET_CNY': '10'}


# The exact previous recipe remains readable for matched rollback and backup.
ROLLBACK_FIXED = {k: v for k, v in {**FIXED, 'ADMIN_APPS': 'journal',
    'ADMIN_RP_ID': journal.HOST, 'ADMIN_ORIGIN': 'https://' + journal.HOST,
    'ADMIN_WRITES_ENABLED': 'false'}.items() if k not in {
        'PLATFORM_OPERATIONS_WRITES_ENABLED', 'HEALTH_APP_STORE_CONNECT_ENABLED',
        'BIRTHDAY_APP_STORE_CONNECT_ENABLED', 'ALBUM_APP_STORE_CONNECT_ENABLED'}}
OVERLAY_SETTINGS = {OVERLAY_SHA256: FIXED, ROLLBACK_OVERLAY_SHA256: ROLLBACK_FIXED}


def settings(folder):
    result = OVERLAY_SETTINGS.get(hashlib.sha256((folder / OVERLAY).read_bytes()).hexdigest())
    if result is None:
        raise OperationError('test admin overlay differs from its reviewed source')
    return result


def selected(folder):
    return (folder / 'admin.env').exists()


def validate_config(values, app):
    if set(values) != {'PAYLOAD_ENCRYPTION_KEY', 'ADMIN_PREVIEW_SIGNING_KEY'} | MODELS:
        raise OperationError('test admin cannot receive unrelated service credentials')
    if any(not re.fullmatch('ep-[a-z0-9-]+', values[k]) or values[k] != app.get(k) for k in MODELS):
        raise OperationError('test admin model identifiers must match the selected Journal configuration')
    try:
        payload = base64.b64decode(values['PAYLOAD_ENCRYPTION_KEY'], validate=True)
        preview = base64.b64decode(values['ADMIN_PREVIEW_SIGNING_KEY'] + '=', validate=True)
        if (len(payload) != 32 or len(preview) != 32 or payload == preview
                or values['PAYLOAD_ENCRYPTION_KEY'] != app.get('PAYLOAD_ENCRYPTION_KEY')):
            raise ValueError()
    except ValueError as error:
        raise OperationError('test admin requires distinct private preview and test payload keys') from error


def validate(folder, compose, archive):
    if not selected(folder):
        if 'PLATFORM_TEST_ADMIN_ENV_FILE' in compose:
            raise OperationError('test admin configuration was not selected')
        return
    app = read_environment(folder / 'runtime.env')
    if ('journal' not in app.get('PLATFORM_APPS', '').split(',') or not journal.read_aloud_selected(folder)
            or compose.get('PLATFORM_TEST_ADMIN_ENV_FILE') != str(folder / 'admin.env')):
        raise OperationError('test admin requires the selected Journal read-aloud test service')
    validate_config(read_environment(folder / 'admin.env'), app)
    body = (folder / OVERLAY).read_bytes()
    try:
        original = archive.extractfile(OVERLAY)
    except KeyError as error:
        raise OperationError('test admin overlay is absent from its source') from error
    if original is None or original.read() != body or hashlib.sha256(body).hexdigest() not in OVERLAY_SETTINGS:
        raise OperationError('test admin overlay differs from its reviewed source')


def require_transition(before, after):
    if selected(before) and (not selected(after) or (before / 'admin.env').read_bytes() != (after / 'admin.env').read_bytes()):
        raise OperationError('test admin update must preserve its active private configuration')
    if selected(before) and not set(settings(before)['ADMIN_APPS'].split(',')) <= set(settings(after)['ADMIN_APPS'].split(',')):
        raise OperationError('test admin update cannot narrow its active application scope')


def verify(container, folder):
    config = container.get('Config', {})
    env = dict(item.split('=', 1) for item in config.get('Env', []) if '=' in item)
    compose = read_environment(folder / 'compose.env')
    fixed = settings(folder)
    expected = {**fixed, **read_environment(folder / 'admin.env'),
                'DATABASE_DSN': 'platform_test:' + compose['PLATFORM_TEST_MYSQL_PASSWORD'] + '@tcp(mysql:3306)/platform_test?charset=utf8mb4&collation=utf8mb4_0900_ai_ci&parseTime=true',
                'REDIS_URL': 'redis://:' + compose['PLATFORM_TEST_REDIS_PASSWORD'] + '@redis:6379/1'}
    mounts = container.get('Mounts', [])
    host = container.get('HostConfig', {})
    if (not selected(folder) or config.get('User') != '65532:65532'
            or any(env.get(k) != v for k, v in expected.items())
            or any(k.startswith(('HEALTH_', 'ALBUM_', 'BIRTHDAY_', 'ARK_', 'JOURNAL_')) and k not in fixed and k not in MODELS and k != 'HEALTH_SCHEMA_MANIFEST_PATH' for k in env)
            or len(mounts) != 1 or mounts[0].get('Source') != str(journal.PRIVATE_BASE / 'development/api-key.txt')
            or mounts[0].get('Destination') != KEY_FILE or mounts[0].get('Type') != 'bind' or mounts[0].get('RW') is not False
            or host.get('PortBindings') != {'8082/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '18792'}]}
            or host.get('ReadonlyRootfs') is not True
            or set(container.get('NetworkSettings', {}).get('Networks', {})) != {'tellyouwhat-platform-test_platform-test'}):
        raise OperationError('test admin process escaped its reviewed credential, network or mount boundary')


def ready():
    from test_routes import RejectRedirects
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), RejectRedirects)
    with opener.open('http://127.0.0.1:18792/readyz', timeout=2) as response:
        if response.status != 200 or json.load(response).get('status') != 'ready':
            raise OperationError('test admin dependencies are not ready')
