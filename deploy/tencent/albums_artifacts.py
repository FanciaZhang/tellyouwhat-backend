"""Include each isolated Albums environment in authenticated recovery."""

import hashlib
import json
import os
from pathlib import Path
import re
import tempfile

from ops_common import OperationError, read_environment
from albums_checkpoint import checkpoint, database_scope

FILES = ('source.tar', 'compose.albums-test.yaml', 'runtime.env', 'compose.env', 'selection.json')
PRODUCTION_FILES = tuple('compose.albums-production.yaml' if name == 'compose.albums-test.yaml' else name for name in FILES)
OPTIONAL_KEYS = ('album-app-store.p8', 'apple-app-store-roots.pem', 'album-apns.p8',
                 'share-key.base64', 'share-previous-key.base64')
KEYS = ('apple-sign-in.p8', 'credential-key.base64', 'login-limit-key.base64')


def production_keys(app):
    required = {'ALBUM_APPLE_BUNDLE_ID': 'cn.tellyouwhat.albums',
                'ALBUM_API_HOST': 'api.albums.tellyouwhat.cn',
                'ALBUM_COS_BUCKET': 'tellyouwhat-albums-production-1251995834'}
    if any(app.get(k) != v for k, v in required.items()):
        raise OperationError('Albums production recovery contains another environment')
    keys = set(KEYS)
    for flag, names in (('SUBSCRIPTIONS', ('album-app-store.p8', 'apple-app-store-roots.pem')),
                        ('RETENTION_PUSH', ('album-apns.p8',)), ('SHARING', ('share-key.base64',))):
        if app.get('ALBUM_' + flag + '_ENABLED') == 'true':
            keys.update(names)
    paths = {'ALBUM_APPLE_SIGN_IN_KEY_FILE': 'apple-sign-in.p8',
             'ALBUM_CREDENTIAL_KEY_FILE': 'credential-key.base64', 'ALBUM_LOGIN_LIMIT_KEY_FILE': 'login-limit-key.base64',
             'ALBUM_APP_STORE_KEY_FILE': 'album-app-store.p8', 'ALBUM_APP_STORE_ROOTS_FILE': 'apple-app-store-roots.pem',
             'ALBUM_APNS_KEY_FILE': 'album-apns.p8', 'ALBUM_SHARE_KEY_FILE': 'share-key.base64',
             'ALBUM_SHARE_PREVIOUS_KEY_FILE': 'share-previous-key.base64'}
    for field, name in paths.items():
        if app.get(field) and app[field] != '/album-secrets/' + name:
            raise OperationError('Albums production key path is outside its recovery selection')
    if app.get('ALBUM_SHARE_PREVIOUS_KEY_FILE'):
        keys.add('share-previous-key.base64')
    return keys


def selected_files(runtime, folder, source, required_files):
    result = {}
    for name in required_files:
        path = folder / name
        if (path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077
                or path.stat().st_uid != runtime.root.stat().st_uid
                or not 0 < path.stat().st_size <= (64 * 1024**2 if name == 'source.tar' else 32768)):
            raise OperationError('unsafe Albums recovery input')
        result[name] = path.read_bytes()
    record = json.loads(result['selection.json'])
    if record.get('source') != source or set(record.get('files', {})) != set(required_files) - {'selection.json'}:
        raise OperationError('Albums recovery manifest is incomplete')
    for name, content in result.items():
        if name != 'selection.json' and hashlib.sha256(content).hexdigest() != record['files'][name]:
            raise OperationError('Albums recovery differs from selected configuration')
    return result


def backup_configuration(runtime, destination, environment='development'):
    database_scope(environment)
    label = 'albums-' + environment
    required_files = PRODUCTION_FILES if environment == 'production' else FILES
    state_path = runtime.state / (label + '.json')
    if not state_path.exists():
        return {}
    if state_path.is_symlink() or not state_path.is_file() or state_path.stat().st_size > 16384:
        raise OperationError('invalid Albums recovery state')
    state = json.loads(state_path.read_text())
    source = state.get('source', '')
    folder = runtime.root / ('.' + label) / 'releases' / source
    owner_path = runtime.state / 'artifact-owners' / ('albums-production.json' if environment == 'production' else 'albums-test.json')
    if (not re.fullmatch('[a-f0-9]{40}', source) or state.get('release') != str(folder)
            or any(p.is_symlink() or not p.is_dir() for p in (folder, folder.parent, folder.parent.parent))
            or owner_path.is_symlink() or not owner_path.is_file()):
        raise OperationError('Albums recovery selection is missing')
    owner = json.loads(owner_path.read_text())
    selected = [owner.get('current', {}), owner.get('previous', {})]
    if not any(s and s.get('source') == source and str(folder) in s.get('paths', []) for s in selected):
        raise OperationError('Albums selection is not registered')
    files = {'state.json': state_path, 'owner.json': owner_path,
             'database.env': runtime.root.parent / label / 'database.env'}
    data = {'current/' + name: content for name, content in selected_files(runtime, folder, source, required_files).items()}
    previous = owner.get('previous')
    if environment == 'production' and previous and previous.get('source') != source:
        revision = previous.get('source', '')
        old = folder.parent / revision
        if (not re.fullmatch('[a-f0-9]{40}', revision) or str(old) not in previous.get('paths', [])
                or old.is_symlink() or not old.is_dir()):
            raise OperationError('Albums previous recovery selection is missing')
        data.update({'previous/' + name: content for name, content in selected_files(runtime, old, revision, required_files).items()})
    for name, path in files.items():
        if (path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077
                or path.stat().st_uid != runtime.root.stat().st_uid
                or not 0 < path.stat().st_size <= (64 * 1024**2 if name.endswith('source.tar') else 32768)):
            raise OperationError('unsafe Albums recovery input')
        data[name] = path.read_bytes()
    secrets = runtime.root.parent / label / 'secrets'
    keys = list(KEYS)
    if environment == 'production':
        keys.extend(name for name in OPTIONAL_KEYS if (secrets / name).exists() or (secrets / name).is_symlink())
        selections = [folder, old] if 'previous/runtime.env' in data else [folder]
        for selection in selections:
            if not production_keys(read_environment(selection / 'runtime.env')) <= set(keys):
                raise OperationError('Albums enabled feature is missing its recovery credential')
    for name in keys:
        path = secrets / name
        if (path.parent.is_symlink() or path.is_symlink() or not path.is_file()
                or path.stat().st_uid != 0 or path.stat().st_gid != 65532
                or path.stat().st_mode & 0o777 != 0o440 or not 0 < path.stat().st_size <= 16384):
            raise OperationError('unsafe Albums recovery credential')
        data['secrets/' + name] = runtime.execute('backup-albums-' + environment + '-key', ['sudo', '-n', 'cat', '--', str(path)])
    data.update(checkpoint(runtime, folder, environment))
    destination.mkdir(mode=0o700)
    for name, content in data.items():
        path = destination / name
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, 'wb') as output:
            output.write(content)
    return {name: hashlib.sha256(content).hexdigest() for name, content in data.items()}


def verify_recovery(archive, manifest, environment='development'):
    database_scope(environment)
    required_files = PRODUCTION_FILES if environment == 'production' else FILES
    expected = {'state.json', 'owner.json', 'database.env', 'backup.env',
                *('current/' + name for name in required_files), *('secrets/' + name for name in KEYS)}
    optional = {'secrets/' + name for name in OPTIONAL_KEYS} if environment == 'production' else set()
    if environment == 'production' and any(name.startswith('previous/') for name in manifest):
        expected.update('previous/' + name for name in required_files)
    extra = set(manifest) - expected - optional
    snapshots = [name for name in extra if re.fullmatch(r'checkpoint/albums-[a-f0-9]{32}\.sql\.gz\.enc', name)]
    ledgers = [name for name in extra if re.fullmatch(r'checkpoint/albums-deletions-[a-f0-9]{32}\.json', name)]
    if (not expected <= set(manifest) or len(extra) != 3 or len(snapshots) != 1 or len(ledgers) != 1
            or snapshots[0] + '.json' not in extra):
        raise OperationError('Albums recovery files are incomplete')
    for name, digest in manifest.items():
        try:
            member = archive.getmember('recovery/albums-' + environment + '/' + name)
        except KeyError as error:
            raise OperationError('Albums recovery file is missing') from error
        if not member.isfile() or hashlib.sha256(archive.extractfile(member).read()).hexdigest() != digest:
            raise OperationError('Albums recovery content verification failed')
    if environment == 'production':
        with tempfile.TemporaryDirectory(prefix='albums-recovery-check-') as directory:
            path = Path(directory) / 'runtime.env'
            for label in ('current', 'previous'):
                name = label + '/runtime.env'
                if name not in manifest:
                    continue
                path.write_bytes(archive.extractfile('recovery/albums-production/' + name).read())
                path.chmod(0o600)
                required = {'secrets/' + key for key in production_keys(read_environment(path))}
                if not required <= set(manifest):
                    raise OperationError('Albums recovery is missing an enabled feature credential')
