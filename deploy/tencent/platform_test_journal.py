"""Admit the already provisioned Journal development resources to the shared gateway."""

import hashlib
import json
import os
from pathlib import Path
import re
import stat

from ops_common import OperationError, read_environment

HOST = 'api-test.journal.tellyouwhat.cn'
FIXED = {'JOURNAL_APPLE_BUNDLE_ID': 'cn.tellyouwhat.journalapp.dev', 'JOURNAL_APPLE_TEAM_ID': 'T6DD96HY2B',
         'JOURNAL_API_HOSTS': HOST, 'JOURNAL_APP_ATTEST_ENV': 'production',
         'JOURNAL_ARK_BASE_URL': 'https://ark.cn-beijing.volces.com/api/v3',
         'JOURNAL_IMAGE_BASE_URL': 'https://ark.cn-beijing.volces.com/api/v3',
         'JOURNAL_VOICE_ENABLED': 'true', 'JOURNAL_IMAGE_ENABLED': 'true'}
RESOURCE_KEYS = {'JOURNAL_ARK_API_KEY': 'JOURNAL_ARK_API_KEY',
                 'JOURNAL_ARK_LITE_MODEL_ID': 'JOURNAL_ARK_LITE_MODEL',
                 'JOURNAL_ARK_PRO_MODEL_ID': 'JOURNAL_ARK_PRO_MODEL',
                 'JOURNAL_VOICE_MODEL_ID': 'JOURNAL_VOICE_MODEL',
                 'JOURNAL_SPEECH_API_KEY': 'JOURNAL_VOICE_ASR_API_KEY',
                 'JOURNAL_IMAGE_API_KEY': 'JOURNAL_IMAGE_API_KEY',
                 'JOURNAL_IMAGE_MODEL_ID': 'JOURNAL_IMAGE_MODEL_ID',
                 'JOURNAL_IMAGE_MAX_COST_NANOS': 'JOURNAL_IMAGE_MAX_COST_NANOS'}
READ_ALOUD_SETTINGS = {'JOURNAL_TTS_ENABLED', 'JOURNAL_TTS_API_KEY_FILE', 'JOURNAL_TTS_MONTHLY_CHARACTERS',
                       'JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS', 'JOURNAL_READ_ALOUD_SIMULATOR_CREDENTIALS_FILE'}
SETTINGS = set(FIXED) | set(RESOURCE_KEYS) | {'JOURNAL_ALLOWED_APP_BUILDS', 'JOURNAL_RECORDING_ENABLED'} | READ_ALOUD_SETTINGS
READ_ALOUD_OVERLAY = 'compose.platform-read-aloud-test.yaml'
READ_ALOUD_OVERLAY_SHA256 = '54097d3626c2ea8474d5bbbf856f70879a54db20110ce19e827870d590168d76'
READ_ALOUD_FILES = {READ_ALOUD_OVERLAY: 32768}
BACKEND_ROOT = Path('/opt/tellyouwhat/backend')
PRIVATE_BASE = Path('/opt/tellyouwhat/journal-read-aloud')
SERVICE_UID = 65532
KEY_FILE = '/journal-read-aloud/api-key.txt'
SIMULATOR_FILE = '/journal-read-aloud/simulators.json'


def validate(app):
    from platform_test_deploy import STOREKIT_SETTINGS
    if (any(app.get(k) != v for k, v in FIXED.items())
            or app.get('JOURNAL_RECORDING_ENABLED') not in ('false', 'true')
            or not re.fullmatch(r'\d+(,\d+)*', app.get('JOURNAL_ALLOWED_APP_BUILDS', ''))
            or any(not app.get(k) for k in RESOURCE_KEYS)
            or any(k.startswith('JOURNAL_') and k not in SETTINGS | STOREKIT_SETTINGS for k in app)
            or any(not re.fullmatch(r'ep-[a-z0-9-]+', app.get(k, '')) for k in RESOURCE_KEYS if k.endswith('_MODEL_ID'))
            or not re.fullmatch(r'\d{1,10}', app.get('JOURNAL_IMAGE_MAX_COST_NANOS', ''))):
        raise OperationError('Journal test identity, inference or resource boundary is invalid')
    if not 0 < int(app['JOURNAL_IMAGE_MAX_COST_NANOS']) <= 1_000_000_000:
        raise OperationError('Journal test image price ceiling is invalid')
    validate_read_aloud(app)


def validate_read_aloud(app):
    fields = READ_ALOUD_SETTINGS & set(app)
    if not fields or fields == {'JOURNAL_TTS_ENABLED'} and app['JOURNAL_TTS_ENABLED'] == 'false':
        return
    if (app.get('JOURNAL_TTS_ENABLED') not in ('true', 'false')
            or app.get('JOURNAL_TTS_API_KEY_FILE') != KEY_FILE
            or app.get('JOURNAL_READ_ALOUD_SIMULATOR_CREDENTIALS_FILE', '') not in ('', SIMULATOR_FILE)):
        raise OperationError('Journal read-aloud requires its fixed private test mount')
    for name, maximum in [('JOURNAL_TTS_MONTHLY_CHARACTERS', 100000), ('JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS', 10000)]:
        if not re.fullmatch(r'[1-9]\d{0,5}', app.get(name, '')) or int(app[name]) > maximum:
            raise OperationError('Journal test read-aloud character allowance is invalid')


def read_aloud_selected(folder):
    return bool(read_environment(folder / 'runtime.env').get('JOURNAL_TTS_API_KEY_FILE'))


def validate_read_aloud_overlay(folder, archive):
    if not read_aloud_selected(folder):
        return
    body = (folder / READ_ALOUD_OVERLAY).read_bytes()
    try:
        source = archive.extractfile(READ_ALOUD_OVERLAY)
    except KeyError as error:
        raise OperationError('Journal read-aloud overlay is absent from its source') from error
    if (source is None or source.read() != body
            or hashlib.sha256(body).hexdigest() != READ_ALOUD_OVERLAY_SHA256):
        raise OperationError('Journal read-aloud overlay differs from its reviewed source')


def prepare_read_aloud(runtime, folder):
    if read_aloud_selected(folder):
        runtime.execute('journal-read-aloud-private-prepare', ['sudo', '-n', 'python3', str(Path(__file__).resolve()),
                                                             str(folder / 'runtime.env')])


def verify_read_aloud(container, folder, service):
    mounts = [m for m in container.get('Mounts', []) if m.get('Destination') == '/journal-read-aloud']
    config = container.get('Config', {})
    env = dict(e.split('=', 1) for e in config.get('Env', []) if '=' in e)
    selected = read_aloud_selected(folder)
    if service == 'gateway' and selected:
        app = read_environment(folder / 'runtime.env')
        if (config.get('User') != '65532:65532' or env.get('APP_ENV') != 'development'
                or len(mounts) != 1 or mounts[0].get('Source') != str(PRIVATE_BASE / 'development')
                or mounts[0].get('Type') != 'bind' or mounts[0].get('RW') is not False
                or any(env.get(k, '') != app.get(k, '') for k in READ_ALOUD_SETTINGS)):
            raise OperationError('Journal read-aloud test mount or gateway configuration is invalid')
    elif (mounts or service == 'worker' and selected and any(
            env.get(k, '') != ('false' if k == 'JOURNAL_TTS_ENABLED' else '') for k in READ_ALOUD_SETTINGS)
            or not selected and any(env.get(k, '') not in (('', 'false') if k == 'JOURNAL_TTS_ENABLED' else ('',))
                                    for k in READ_ALOUD_SETTINGS)):
        raise OperationError('Journal read-aloud private mount escaped its selected test gateway')


def _private_read(path, owner, limit):
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != owner or stat.S_IMODE(info.st_mode) != 0o600
            or not 0 < info.st_size <= limit):
        raise OperationError('Journal read-aloud file must be private, bounded and owned by its operator')
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), 'rb') as handle:
        if not os.path.samestat(info, os.fstat(handle.fileno())):
            raise OperationError('Journal read-aloud private file changed while opening')
        body = handle.read(limit + 1)
    if len(body) > limit:
        raise OperationError('Journal read-aloud private file exceeds its bound')
    return body


def _create_private(path, body):
    if not path.exists() and not path.is_symlink():
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(descriptor, 'wb') as handle:
            os.fchown(handle.fileno(), SERVICE_UID, SERVICE_UID)
            handle.write(body)
            handle.flush()
            os.fsync(handle.fileno())
    return _private_read(path, SERVICE_UID, 32768 if path.name == 'simulators.json' else 8192)


def prepare_private_read_aloud(environment):
    environment = Path(environment)
    if (environment.name != 'runtime.env' or not re.fullmatch('[a-f0-9]{40}', environment.parent.name)
            or environment.parent.parent != BACKEND_ROOT / '.platform-test/releases'):
        raise OperationError('Journal read-aloud preparation requires a selected test release')
    approved = BACKEND_ROOT.parent / 'journal-development/runtime.env'
    for path in (*environment.parents, *PRIVATE_BASE.parents, *approved.parents):
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o022:
            raise OperationError('Journal read-aloud private storage has an unsafe ancestor')
    owner = BACKEND_ROOT.stat().st_uid
    _private_read(environment, owner, 32768)
    app = read_environment(environment)
    validate(app)
    if app.get('JOURNAL_TTS_API_KEY_FILE') != KEY_FILE:
        raise OperationError('Journal read-aloud private storage was not selected')
    _private_read(approved, owner, 32768)
    key = app.get('JOURNAL_SPEECH_API_KEY', '')
    if (key != read_environment(approved).get('JOURNAL_VOICE_ASR_API_KEY') or not 1 <= len(key) <= 4096
            or any(ord(c) < 33 or ord(c) > 126 for c in key)):
        raise OperationError('Journal read-aloud must use its approved test speech credential')
    root = PRIVATE_BASE / 'development'
    for path, uid, mode in [(PRIVATE_BASE, 0, 0o711), (root, SERVICE_UID, 0o700)]:
        try:
            path.mkdir(mode=mode)
        except FileExistsError:
            pass
        else:
            os.chown(path, uid, uid)
            path.chmod(mode)
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != uid or stat.S_IMODE(info.st_mode) != mode:
            raise OperationError('Journal read-aloud private directory ownership or permissions differ')
    if _create_private(root / 'api-key.txt', (key + '\n').encode()).strip() != key.encode():
        raise OperationError('Journal read-aloud credential changed; explicit rotation is required')
    if app.get('JOURNAL_READ_ALOUD_SIMULATOR_CREDENTIALS_FILE'):
        body = _create_private(root / 'simulators.json', b'{"version":1,"credentials":[]}\n')
        registry = json.loads(body)
        if (not isinstance(registry, dict) or set(registry) != {'version', 'credentials'}
                or type(registry['version']) is not int or registry['version'] != 1 or not isinstance(registry['credentials'], list)
                or len(registry['credentials']) > 16):
            raise OperationError('Journal read-aloud simulator registry is invalid')


def require_resources(runtime, app):
    from platform_test_deploy import private_file
    path = runtime.root.parent / 'journal-development/runtime.env'
    private_file(runtime, path, 32768)
    approved = read_environment(path)
    for key, source in RESOURCE_KEYS.items():
        if not approved.get(source) or app[key] != approved[source]:
            raise OperationError('Journal shared test must use its existing restricted development resources')
        if key != 'JOURNAL_IMAGE_MAX_COST_NANOS' and app[key] == runtime.config.get(key):
            raise OperationError('Journal test credentials and endpoints must differ from production')


if __name__ == '__main__':
    import sys
    if len(sys.argv) != 2 or os.geteuid() != 0:
        raise SystemExit('Journal read-aloud preparation requires its privileged deployment step')
    try:
        prepare_private_read_aloud(sys.argv[1])
    except (OSError, ValueError, OperationError):
        raise SystemExit('Journal read-aloud private preparation failed; check the selected test inputs') from None
