"""Activate the reviewed, source-bound private OTA service through its owner wrapper."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import time
import urllib.request

from blue_green import assert_route
from ops_artifacts import development_deploy, require_operator
from ops_common import OperationError, Runtime, atomic_json, atomic_text


ORIGIN = 'https://install.tellyouwhat.cn'
IMAGE = 'tellyouwhat-apple-development:'
FILES = ('service', 'Dockerfile', 'compose.apple-delivery.yaml', 'apple-delivery.caddy', 'apple-delivery-logging.caddy')


def read_record(runtime, path):
    if (path.is_symlink() or not path.is_file() or path.stat().st_size > 16384
            or path.stat().st_uid != runtime.root.stat().st_uid or path.stat().st_mode & 0o077):
        raise OperationError('Apple delivery record must be private, bounded and operator owned')
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise OperationError('invalid Apple delivery record')
    return value


def selection(runtime, source):
    if not re.fullmatch(r'[0-9a-f]{40}', source or ''):
        raise OperationError('Apple delivery requires a complete source revision')
    folder = runtime.root / '.apple-delivery/releases' / source
    for path in (folder.parent.parent, folder.parent, folder):
        if path.is_symlink() or not path.is_dir() or path.stat().st_uid != runtime.root.stat().st_uid or path.stat().st_mode & 0o077:
            raise OperationError('Apple delivery release must be private and operator owned')
    value = read_record(runtime, folder / 'selection.json')
    if (value.get('source') != source or value.get('image') != IMAGE + source or value.get('origin') != ORIGIN
            or not isinstance(value.get('files'), dict) or set(value['files']) != set(FILES)):
        raise OperationError('Apple delivery selection does not match its source')
    for name, digest in value['files'].items():
        path = folder / name
        if (not isinstance(digest, str) or not re.fullmatch(r'[0-9a-f]{64}', digest) or path.is_symlink()
                or not path.is_file() or not 0 < path.stat().st_size <= (16 * 1024**2 if name == 'service' else 256 * 1024)
                or path.stat().st_uid != runtime.root.stat().st_uid or path.stat().st_mode & 0o077
                or hashlib.sha256(path.read_bytes()).hexdigest() != digest):
            raise OperationError('Apple delivery release file is missing, unsafe or modified')
    return folder, value


def compose(runtime, folder, *args):
    return ['docker', 'compose', '--project-name', 'tellyouwhat-apple-delivery',
            '--env-file', str(folder / '.env'), '-f', str(folder / 'compose.apple-delivery.yaml'), *args]


def environment(runtime, source):
    operator = runtime.root.stat()
    return {'APPLE_DELIVERY_IMAGE_TAG': source, 'APPLE_DELIVERY_UID': str(operator.st_uid),
            'APPLE_DELIVERY_GID': str(operator.st_gid), 'APPLE_DELIVERY_ORIGIN': ORIGIN,
            'APPLE_DELIVERY_DATA_DIR': str(runtime.root / '.apple-delivery/data')}


def ready():
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    for attempt in range(30):
        try:
            with opener.open('http://127.0.0.1:18791/readyz', timeout=2) as response:
                if response.geturl() == 'http://127.0.0.1:18791/readyz' and response.status == 200 and json.load(response) == {'status': 'ready'}:
                    return
        except (OSError, ValueError):
            pass
        time.sleep(1)
    raise OperationError('Apple delivery readiness failed')


def activate(runtime, source):
    require_operator(runtime.root)
    folder, value = selection(runtime, source)
    stable = json.loads((runtime.state / 'blue-green.json').read_text())
    if stable.get('phase') != 'STABLE':
        raise OperationError('wait for a stable production release')
    assert_route(stable['current'])
    state_file = runtime.state / 'apple-delivery.json'
    owner_file = runtime.state / 'artifact-owners/apple-development.json'
    previous = read_record(runtime, state_file) if state_file.exists() or state_file.is_symlink() else None
    old_folder = None
    if previous:
        old_folder, old = selection(runtime, previous.get('source'))
        owner = read_record(runtime, owner_file)
        if (previous.get('image') != old['image'] or previous.get('release') != str(old_folder)
                or old['image'] not in owner.get('current', {}).get('images', [])):
            raise OperationError('Apple delivery recovery selection is not registered')
    elif state_file.exists() or owner_file.exists() or owner_file.is_symlink() or runtime.execute('apple-delivery-inventory', [
            'docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=tellyouwhat-apple-delivery']).strip():
        raise OperationError('unmanaged Apple delivery service or recovery state exists')

    existing = runtime.execute('apple-delivery-image-inventory', ['docker', 'image', 'ls', '--quiet', value['image']]).strip()
    if existing:
        details = json.loads(runtime.execute('apple-delivery-image', ['docker', 'image', 'inspect', value['image']]))[0]
        labels = details.get('Config', {}).get('Labels', {})
        if labels.get('org.opencontainers.image.revision') != source or labels.get('cn.tellyouwhat.binary-sha256') != value['files']['service']:
            raise OperationError('Apple delivery image tag already identifies different bytes')
    else:
        runtime.execute('apple-delivery-build', ['docker', 'build', '--network=none',
            '--label', 'org.opencontainers.image.revision=' + source,
            '--label', 'cn.tellyouwhat.binary-sha256=' + value['files']['service'],
            '-t', value['image'], '-f', str(folder / 'Dockerfile'), str(folder)], timeout=180)
    env = environment(runtime, source)
    atomic_text(folder / '.env', ''.join(key + '=' + val + '\n' for key, val in env.items()))
    data = runtime.root / '.apple-delivery/data'
    if data.is_symlink():
        raise OperationError('Apple delivery data directory cannot be a symlink')
    data.mkdir(mode=0o700, exist_ok=True)
    if data.stat().st_uid != runtime.root.stat().st_uid or data.stat().st_mode & 0o077:
        raise OperationError('Apple delivery data must be private and operator owned')
    # Initialize before the server receives its read-only mount.
    runtime.execute('apple-delivery-initialize', compose(runtime, folder, 'run', '--rm', '--no-deps', 'cleanup'), env=env, timeout=120)
    try:
        runtime.execute('apple-delivery-start', compose(runtime, folder, 'up', '--no-build', '-d', 'server'), env=env, timeout=120)
        ready()
        assert_route(stable['current'])
        current = {'source': source, 'image': value['image'], 'release': str(folder), 'origin': ORIGIN}
        current['previous'] = previous.get('previous') if previous else None
        if previous and previous['source'] != source:
            current['previous'] = {k: v for k, v in previous.items() if k != 'previous'}
        atomic_json(state_file, current)
    except Exception as error:
        try:
            if old_folder:
                runtime.execute('apple-delivery-restore', compose(runtime, old_folder, 'up', '--no-build', '-d', 'server'),
                                env=environment(runtime, previous['source']), timeout=120)
                ready()
            else:
                runtime.execute('apple-delivery-remove-failed', compose(runtime, folder, 'down'), env=env, timeout=120)
            if previous:
                atomic_json(state_file, previous)
            else:
                state_file.unlink(missing_ok=True)
            assert_route(stable['current'])
        except Exception as recovery:
            raise OperationError('Apple delivery activation and recovery failed; operator recovery required') from recovery
        raise OperationError('Apple delivery activation failed; prior service restored') from error
    return current


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', default='/opt/tellyouwhat/backend')
    parser.add_argument('--source', required=True)
    parser.add_argument('--source-run', default='remote-development')
    parser.add_argument('--activate', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    runtime = Runtime(args.backend_dir)
    folder, value = selection(runtime, args.source)
    if args.activate:
        # Only the owner wrapper's child runs this path while both host locks remain held.
        activate(runtime, args.source)
        return
    command = [sys.executable, str(Path(__file__).resolve()), '--backend-dir', str(runtime.root), '--source', args.source, '--activate']
    result = development_deploy(runtime, 'apple-development', [str(folder)], [value['image']], args.source,
                               args.source_run, command, 'http://127.0.0.1:18791/readyz')
    print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (OperationError, OSError, ValueError) as error:
        print(json.dumps({'passed': False, 'error': str(error)}), file=sys.stderr)
        sys.exit(1)
