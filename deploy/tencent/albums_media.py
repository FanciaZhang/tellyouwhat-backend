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
MEDIA_FILES = ('manifest.json', 'before.yaml', 'after.yaml', 'before-state.json')


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
    before = json.loads(data['before-state.json'])
    media = state.get('media')
    if (not isinstance(media, dict) or set(media) != {'run', 'manifestSHA256'}
            or media['manifestSHA256'] != digest(data['manifest.json'])
            or record.get('run') != media['run'] or not re.fullmatch('[a-f0-9]{32}', media['run'])
            or record.get('stateSHA256') != digest(data['before-state.json'])
            or 'media' in before or state != {**before, 'media': media}
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


def verify_worker(runtime, record, enabled, *, startup=False, allow_stopped=False):
    container = inspect(runtime, WORKER)
    env = environment(container)
    feature_values = {env.get(key) for key in FLAGS}
    if ((not allow_stopped and (not container['State'].get('Running') or container['State'].get('OOMKilled') or container.get('RestartCount')))
            or container['Image'] != record['imageID']
            or container['Config'].get('Image') != 'tellyouwhat-albums-test-development-albumworker:' + record['source']
            or container['Config'].get('Labels', {}).get('com.docker.compose.project') != PROJECT
            or container.get('Mounts') or set(container['NetworkSettings']['Networks']) != {PROJECT + '_albums-test'}
            or (enabled is not None and feature_values != {str(enabled).lower()})
            or (enabled is None and feature_values not in ({'false'}, {'true'}))):
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
    atomic_text(folder / 'before-state.json', state_bytes.decode())
    atomic_json(folder / 'manifest.json', record)
    return folder


def activate(runtime, run, *, recover=False):
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
    current_bytes, _, _, base, files = base_selection(runtime)
    old_bytes = data['before-state.json']
    old = json.loads(old_bytes)
    selected = {**old, 'media': {'run': run, 'manifestSHA256': digest(data['manifest.json'])}}
    record = validate_media(data, selected)
    selected_bytes = (json.dumps(selected, sort_keys=True) + '\n').encode()
    if (current_bytes not in (old_bytes, selected_bytes)
            or {name: digest(value) for name, value in files.items()} != record['baseFiles']):
        raise OperationError('media activation inputs changed after preparation')
    journal_path = folder / 'attempt.json'
    attempt = json.loads(private(runtime, journal_path)) if journal_path.exists() else None
    if journal_path.exists() and (not isinstance(attempt, dict) or attempt.get('run') != run
                    or attempt.get('manifestSHA256') != digest(data['manifest.json'])
                    or attempt.get('phase') not in ('applying', 'verified', 'restoring', 'restored')
                    or not isinstance(attempt.get('preBackup'), str) or not attempt['preBackup']
                    or (attempt.get('phase') == 'verified' and not isinstance(attempt.get('postBackup'), str))):
        raise OperationError('invalid media activation journal')

    def journal(phase, **values):
        value = {**(attempt or {}), 'run': run, 'manifestSHA256': digest(data['manifest.json']), 'phase': phase, **values}
        atomic_json(journal_path, value)
        return value

    def backup_proof(snapshot, expected, *, active):
        filename = snapshot.get('filename', '')
        if not isinstance(filename, str) or not filename or filename in ('.', '..') or Path(filename).name != filename:
            raise OperationError('invalid media backup receipt')
        proof = verify_backup(runtime, runtime.backups / filename).get('recovery_albums_development', {})
        if (proof.get('state.json') != digest(expected)
                or proof.get('current/selection.json') != digest(files['selection.json'])
                or not any(name.startswith('checkpoint/albums-') and name.endswith('.sql.gz.enc') for name in proof)
                or (active and any(proof.get('media/' + name) != digest(data[name]) for name in MEDIA_FILES))):
            raise OperationError('media configuration is missing from verified recovery')

    def require_no_compose():
        # An orphaned Compose client can outlive its producer; never race its last write.
        runtime.execute('albums-media-no-compose', ['python3', '-c',
            'import os,pathlib,sys\n'
            'for p in pathlib.Path("/proc").iterdir():\n'
            ' if not p.name.isdigit(): continue\n'
            ' try:\n'
            '  if p.stat().st_uid != os.geteuid(): continue\n'
            '  args=(p/"cmdline").read_bytes().decode().split("\\x00")\n'
            ' except (OSError,UnicodeError): continue\n'
            ' if not args or os.path.basename(args[0]) not in ("docker","docker-compose"): continue\n'
            ' if any(a=="--project-name="+sys.argv[1] or a=="--project-name" and i+1<len(args) and args[i+1]==sys.argv[1] for i,a in enumerate(args)):\n'
            '  raise SystemExit("an existing Albums Compose operation must finish first")\n', PROJECT])

    require_no_compose()
    command = ['docker', 'compose', '--project-name', PROJECT, '--env-file', str(base / 'compose.env'),
               '-f', str(base / 'compose.albums-test.yaml')]

    def start(filename):
        runtime.execute('albums-media-start', [*command, '-f', str(folder / filename), 'up', '-d', '--no-build', '--no-deps', 'albumworker'], timeout=120)

    def restore():
        require_no_compose()
        untouched = unchanged_containers(runtime)
        verify_worker(runtime, record, None, allow_stopped=True)
        journal('restoring')
        start('before.yaml')
        verify_worker(runtime, record, False, startup=True)
        if unchanged_containers(runtime) != untouched:
            raise OperationError('unrelated containers changed during media recovery')
        atomic_text(runtime.state / 'albums-development.json', old_bytes.decode())
        journal('restored')

    if recover:
        if not attempt:
            raise OperationError('no started media activation to recover')
        backup_proof({'filename': attempt['preBackup']}, old_bytes, active=False)
        restore()
        return {'run': run, 'source': old['source'], 'restored': True}
    if attempt:
        if attempt['phase'] == 'verified':
            if current_bytes != selected_bytes:
                raise OperationError('verified media state has changed')
            verify_worker(runtime, record, True, startup=True)
            backup_proof({'filename': attempt['postBackup']}, selected_bytes, active=True)
            return {'run': run, 'source': old['source'], 'preBackup': attempt['preBackup'], 'postBackup': attempt['postBackup']}
        raise OperationError('previous media activation is unfinished or restored; use --recover before preparing another run')
    if current_bytes != old_bytes or unchanged_containers(runtime) != record['unchangedContainers']:
        raise OperationError('media activation inputs changed after preparation')
    verify_worker(runtime, record, False)
    snapshot = create_backup(runtime)
    backup_proof(snapshot, old_bytes, active=False)
    attempt = journal('applying', preBackup=snapshot['filename'])
    try:
        start('after.yaml')
        verify_worker(runtime, record, True, startup=True)
        if unchanged_containers(runtime) != record['unchangedContainers']:
            raise OperationError('unrelated development containers changed during activation')
        atomic_text(runtime.state / 'albums-development.json', selected_bytes.decode())
        post = create_backup(runtime)
        backup_proof(post, selected_bytes, active=True)
        journal('verified', postBackup=post['filename'])
        return {'run': run, 'source': old['source'], 'preBackup': snapshot['filename'], 'postBackup': post['filename']}
    except Exception:
        restore()
        raise


def main():
    from ops_artifacts import development_deploy, require_operator
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', default='/opt/tellyouwhat/backend')
    parser.add_argument('--run', required=True)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument('--prepare', action='store_true')
    mode.add_argument('--recover', action='store_true', help='restore the prepared prior configuration after interruption')
    parser.add_argument('--activate', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    runtime = Runtime(args.backend_dir)
    require_operator(runtime.root)
    if args.prepare:
        with runtime.lock(wait_seconds=60):
            print(prepare(runtime, args.run))
    elif args.activate:
        print(json.dumps(activate(runtime, args.run, recover=args.recover), sort_keys=True))
    else:
        _, state, owner, _, _ = base_selection(runtime)
        folder = media_folder(runtime, args.run)
        current = owner['current']
        command = [sys.executable, str(Path(__file__).resolve()), '--backend-dir', str(runtime.root), '--run', args.run, '--activate']
        if args.recover:
            command.append('--recover')
        paths = list(dict.fromkeys([*current['paths'], str(folder)]))
        result = development_deploy(runtime, 'albums-test', paths, current['images'],
                                   state['source'], 'albums-media-' + args.run, command,
                                   'http://127.0.0.1:18790/readyz', verify_host=HOST)
        print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    main()
