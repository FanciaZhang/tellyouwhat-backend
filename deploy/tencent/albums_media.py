"""Enable the existing development media worker without restarting the shared API."""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import time

from ops_common import OperationError, Runtime, atomic_json, atomic_text, read_environment

PROJECT = 'tellyouwhat-albums-development'
WORKER = PROJECT + '-albumworker-1'
HOST = 'api-test.albums.tellyouwhat.cn'
FLAGS = ('ALBUM_PHOTO_PROCESSING_ENABLED', 'ALBUM_VIDEO_PROCESSING_ENABLED')
MEDIA_FILES = ('manifest.json', 'before.yaml', 'after.yaml')


def overlay(enabled):
    value = 'true' if enabled else 'false'
    return ('services:\n  albumworker:\n    environment:\n' +
            ''.join('      ' + key + ': "' + value + '"\n' for key in FLAGS)).encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def private(runtime, path, maximum=32768):
    if (path.is_symlink() or not path.is_file() or path.stat().st_uid != runtime.root.stat().st_uid
            or path.stat().st_mode & 0o077 or not 0 < path.stat().st_size <= maximum):
        raise OperationError('media configuration must be bounded, private and operator owned')
    return path.read_bytes()


def media_folder(runtime, run):
    if not re.fullmatch('[a-f0-9]{32}', run or ''):
        raise OperationError('invalid media configuration run')
    path = runtime.root / '.albums-development/media' / run
    for parent in (path, path.parent, path.parent.parent):
        if (parent.is_symlink() or not parent.is_dir() or parent.stat().st_uid != runtime.root.stat().st_uid
                or parent.stat().st_mode & 0o077):
            raise OperationError('media configuration directory must be private')
    return path


def validate_media(data, state):
    record = json.loads(data['manifest.json'])
    media = state.get('media')
    if (not isinstance(media, dict) or set(media) != {'run', 'manifestSHA256'}
            or media['manifestSHA256'] != digest(data['manifest.json'])
            or record.get('run') != media['run'] or not re.fullmatch('[a-f0-9]{32}', media['run'])
            or record.get('source') != state.get('source')
            or not re.fullmatch('[a-f0-9]{40}', record.get('source', ''))
            or not re.fullmatch('sha256:[a-f0-9]{64}', record.get('imageID', ''))
            or data['before.yaml'] != overlay(False) or data['after.yaml'] != overlay(True)
            or record.get('overlays') != {name: digest(data[name]) for name in ('before.yaml', 'after.yaml')}):
        raise OperationError('media recovery selection is invalid')
    return record


def recovery_files(runtime, state):
    if 'media' not in state:
        return {}
    if not isinstance(state['media'], dict):
        raise OperationError('invalid media recovery metadata')
    folder = media_folder(runtime, state['media'].get('run'))
    data = {name: private(runtime, folder / name) for name in MEDIA_FILES}
    validate_media(data, state)
    return {'media/' + name: value for name, value in data.items()}


def inspect(runtime, name):
    return json.loads(runtime.execute('albums-media-inspect', ['docker', 'inspect', name]))[0]


def environment(container):
    return dict(item.split('=', 1) for item in container['Config'].get('Env', []) if '=' in item)


def unchanged_containers(runtime):
    names = [PROJECT + '-' + service + '-1' for service in ('mysql', 'redis')]
    names += ['tellyouwhat-platform-test-' + service + '-1' for service in ('gateway', 'worker', 'admin')]
    return {name: inspect(runtime, name)['Id'] for name in names}


def base_selection(runtime):
    from albums_artifacts import FILES, selected_files
    state_bytes = private(runtime, runtime.state / 'albums-development.json')
    state = json.loads(state_bytes)
    source = state.get('source', '')
    if not re.fullmatch('[a-f0-9]{40}', source):
        raise OperationError('invalid registered development source')
    folder = runtime.root / '.albums-development/releases' / source
    for parent in (folder, folder.parent, folder.parent.parent):
        if (parent.is_symlink() or not parent.is_dir() or parent.stat().st_mode & 0o077
                or parent.stat().st_uid != runtime.root.stat().st_uid):
            raise OperationError('development selection must remain private')
    if state.get('release') != str(folder):
        raise OperationError('development source selection changed')
    owner = json.loads(private(runtime, runtime.state / 'artifact-owners/albums-test.json'))
    if (owner.get('current', {}).get('source') != source
            or str(folder) not in owner['current'].get('paths', [])
            or 'tellyouwhat-albums-test-development-albumworker:' + source not in owner['current'].get('images', [])):
        raise OperationError('media worker must have a current registered owner')
    files = selected_files(runtime, folder, source, FILES)
    app = read_environment(folder / 'runtime.env')
    if (app.get('ALBUM_COS_BUCKET') != 'tellyouwhat-albums-1251995834'
            or app.get('ALBUM_API_HOST') != HOST or app.get('ALBUM_APPLE_BUNDLE_ID') != 'cn.tellyouwhat.albums.dev'
            or any(app.get(key) != 'false' for key in FLAGS)
            or not re.fullmatch('p[a-f0-9]{32}', app.get('ALBUM_CI_VIDEO_QUEUE_ID', ''))):
        raise OperationError('media activation requires the reviewed development resources')
    return state_bytes, state, owner, folder, files


def verify_worker(runtime, record, enabled, *, startup=False):
    container = inspect(runtime, WORKER)
    env = environment(container)
    if (not container['State'].get('Running') or container['State'].get('OOMKilled') or container.get('RestartCount')
            or container['Image'] != record['imageID']
            or container['Config'].get('Image') != 'tellyouwhat-albums-test-development-albumworker:' + record['source']
            or container['Config'].get('Labels', {}).get('com.docker.compose.project') != PROJECT
            or container.get('Mounts') or set(container['NetworkSettings']['Networks']) != {PROJECT + '_albums-test'}
            or any(env.get(key) != str(enabled).lower() for key in FLAGS)):
        raise OperationError('media worker image, features or isolation differs from selection')
    for key in FLAGS:
        env[key] = 'false'
    if digest(json.dumps(env, sort_keys=True).encode()) != record['environmentSHA256']:
        raise OperationError('media activation changed unrelated worker settings')
    if startup:
        for _ in range(30):
            logs = runtime.execute('albums-media-startup', ['sh', '-c', 'exec docker logs --since "$1" "$2" 2>&1',
                                   'albums-media-log', container['State']['StartedAt'], WORKER]).decode().splitlines()
            if any(json.loads(line).get('msg') == 'album workers started' for line in logs if line.startswith('{')):
                verify_worker(runtime, record, enabled)
                return
            time.sleep(1)
        raise OperationError('media worker did not complete startup checks')


def prepare(runtime, run):
    state_bytes, state, _, base, files = base_selection(runtime)
    if 'media' in state:
        raise OperationError('media configuration already exists; do not repeat activation')
    if not re.fullmatch('[a-f0-9]{32}', run):
        raise OperationError('invalid media run')
    container = inspect(runtime, WORKER)
    record = {'run': run, 'source': state['source'], 'imageID': container['Image'],
              'environmentSHA256': digest(json.dumps(environment(container), sort_keys=True).encode()),
              'stateSHA256': digest(state_bytes), 'baseFiles': {name: digest(value) for name, value in files.items()},
              'overlays': {name: digest(overlay(enabled)) for name, enabled in [('before.yaml', False), ('after.yaml', True)]},
              'unchangedContainers': unchanged_containers(runtime)}
    verify_worker(runtime, record, False)
    folder = runtime.root / '.albums-development/media' / run
    folder.parent.mkdir(mode=0o700, exist_ok=True)
    folder.mkdir(mode=0o700, exist_ok=False)
    media_folder(runtime, run)
    for name, enabled in [('before.yaml', False), ('after.yaml', True)]:
        atomic_text(folder / name, overlay(enabled).decode())
    atomic_json(folder / 'manifest.json', record)
    return folder


def activate(runtime, run):
    from ops_backup import create_backup, verify_backup
    for name in ('operations', 'deployment'):
        with (runtime.state / (name + '.lock')).open('r') as handle:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                continue
            raise OperationError('media activation requires registered deployment locks')
    # Scheduled backups must understand the same recovery files as this producer.
    for name in ('albums_artifacts.py', 'albums_media.py'):
        installed = runtime.root / 'deploy/tencent' / name
        reviewed = Path(__file__).with_name(name)
        if (installed.is_symlink() or not installed.is_file()
                or installed.stat().st_uid != runtime.root.stat().st_uid or installed.stat().st_mode & 0o022
                or installed.read_bytes() != reviewed.read_bytes()):
            raise OperationError('install the matched scheduled recovery helpers before media activation')
    folder = media_folder(runtime, run)
    data = {name: private(runtime, folder / name) for name in MEDIA_FILES}
    old_bytes, old, _, base, files = base_selection(runtime)
    selected = {**old, 'media': {'run': run, 'manifestSHA256': digest(data['manifest.json'])}}
    record = validate_media(data, selected)
    if (digest(old_bytes) != record['stateSHA256'] or 'media' in old
            or {name: digest(value) for name, value in files.items()} != record['baseFiles']
            or unchanged_containers(runtime) != record['unchangedContainers']):
        raise OperationError('media activation inputs changed after preparation')
    verify_worker(runtime, record, False)
    snapshot = create_backup(runtime)
    verified = verify_backup(runtime, runtime.backups / snapshot['filename'])
    before_proof = verified.get('recovery_albums_development', {})
    if (before_proof.get('state.json') != digest(old_bytes)
            or before_proof.get('current/selection.json') != digest(files['selection.json'])
            or not any(name.startswith('checkpoint/albums-') and name.endswith('.sql.gz.enc') for name in before_proof)):
        raise OperationError('media activation requires a verified Albums backup')
    command = ['docker', 'compose', '--project-name', PROJECT, '--env-file', str(base / 'compose.env'),
               '-f', str(base / 'compose.albums-test.yaml')]

    def start(filename):
        runtime.execute('albums-media-start', [*command, '-f', str(folder / filename), 'up', '-d', '--no-build', '--no-deps', 'albumworker'], timeout=120)

    try:
        start('after.yaml')
        verify_worker(runtime, record, True, startup=True)
        if unchanged_containers(runtime) != record['unchangedContainers']:
            raise OperationError('unrelated development containers changed during activation')
        atomic_json(runtime.state / 'albums-development.json', selected)
        post = create_backup(runtime)
        proof = verify_backup(runtime, runtime.backups / post['filename'])
        media = proof.get('recovery_albums_development', {})
        if (media.get('state.json') != digest(private(runtime, runtime.state / 'albums-development.json'))
                or media.get('current/selection.json') != digest(files['selection.json'])
                or not any(name.startswith('checkpoint/albums-') and name.endswith('.sql.gz.enc') for name in media)
                or any(media.get('media/' + name) != digest(data[name]) for name in MEDIA_FILES)):
            raise OperationError('active media configuration is missing from verified recovery')
        return {'run': run, 'source': old['source'], 'preBackup': snapshot['filename'], 'postBackup': post['filename']}
    except Exception:
        start('before.yaml')
        verify_worker(runtime, record, False, startup=True)
        atomic_text(runtime.state / 'albums-development.json', old_bytes.decode())
        raise


def main():
    from ops_artifacts import development_deploy, require_operator
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', default='/opt/tellyouwhat/backend')
    parser.add_argument('--run', required=True)
    parser.add_argument('--prepare', action='store_true')
    parser.add_argument('--activate', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    runtime = Runtime(args.backend_dir)
    require_operator(runtime.root)
    if args.prepare:
        with runtime.lock(wait_seconds=60):
            print(prepare(runtime, args.run))
    elif args.activate:
        print(json.dumps(activate(runtime, args.run), sort_keys=True))
    else:
        _, state, owner, _, _ = base_selection(runtime)
        folder = media_folder(runtime, args.run)
        current = owner['current']
        command = [sys.executable, str(Path(__file__).resolve()), '--backend-dir', str(runtime.root), '--run', args.run, '--activate']
        result = development_deploy(runtime, 'albums-test', [*current['paths'], str(folder)], current['images'],
                                   state['source'], 'albums-media-' + args.run, command,
                                   'http://127.0.0.1:18790/readyz', verify_host=HOST)
        print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    main()
