"""Prepare/recover the isolated Albums API configuration; apply only via ops_artifacts.py."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import urllib.request
import uuid

ROOT = Path('/opt/tellyouwhat/albums-test')
REGISTRY = Path('/opt/tellyouwhat/backend/.operations')


def update_environment(text, issuer, key):
    if str(uuid.UUID(issuer)) != issuer or not re.fullmatch('[A-Z0-9]{10}', key):
        raise ValueError('invalid Apple key identifiers')
    values = {
        'ALBUM_SUBSCRIPTIONS_ENABLED': 'true',
        'ALBUM_APP_STORE_ISSUER_ID': issuer,
        'ALBUM_APP_STORE_KEY_ID': key,
        'ALBUM_APP_STORE_KEY_FILE': '/album-secrets/album-app-store.p8',
        'ALBUM_APP_STORE_ROOTS_FILE': '/album-secrets/apple-app-store-roots.pem',
        'ALBUM_APP_STORE_APP_APPLE_ID': '6822133304',
    }
    seen, lines = set(), []
    for line in text.splitlines():
        name = line.split('=', 1)[0].strip()
        if name in values:
            if name in seen:
                raise ValueError('duplicate subscription setting')
            seen.add(name)
            line = name + '=' + values[name]
        lines.append(line)
    lines.extend(name + '=' + value for name, value in values.items() if name not in seen)
    return '\n'.join(lines) + '\n'


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def atomic_write(path, data):
    previous = path.stat() if path.exists() else None
    fd, temporary = tempfile.mkstemp(dir=path.parent, prefix='.sandbox-')
    try:
        with os.fdopen(fd, 'wb') as stream:
            if previous:
                os.fchown(stream.fileno(), previous.st_uid, previous.st_gid)
            os.fchmod(stream.fileno(), 0o600)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)


def ready():
    request = urllib.request.Request('http://127.0.0.1:18090/readyz', headers={'Host': 'api.albums.tellyouwhat.cn'})
    for _ in range(30):
        try:
            with urllib.request.urlopen(request, timeout=2) as response:
                if response.status == 200 and response.geturl() == request.full_url and json.load(response) == {'code': 'ok'}:
                    return
        except (OSError, ValueError):
            pass
        time.sleep(1)
    raise RuntimeError('Albums API did not become ready')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=['prepare', 'apply'])
    parser.add_argument('--run', required=True)
    parser.add_argument('--source', required=True)
    parser.add_argument('--issuer', required=True)
    parser.add_argument('--key', required=True)
    args = parser.parse_args()
    if not re.fullmatch('[a-z0-9-]{1,64}', args.run) or not re.fullmatch('[a-f0-9]{40}', args.source):
        raise ValueError('invalid run or source')
    os.umask(0o077)
    runtime = ROOT / '.env.albums-test'
    compose_env = ROOT / '.env.compose-albums-test'
    compose_file = ROOT / ('source-' + args.source) / 'compose.albums-test.yaml'
    recovery = ROOT / args.run
    paths = {name: REGISTRY / (args.run + '-' + name) for name in ('before', 'after')}
    if args.mode == 'prepare':
        ready()
        recovery.mkdir(mode=0o700)
        before = runtime.read_bytes()
        after = update_environment(before.decode(), args.issuer, args.key).encode()
        for name, data in [('before', before), ('after', after)]:
            atomic_write(recovery / (name + '.env'), data)
            paths[name].mkdir(mode=0o700)
            manifest = {'source': args.source, 'runtime_file': str(recovery / (name + '.env')),
                        'runtime_sha256': hashlib.sha256(data).hexdigest(),
                        'compose_file': str(compose_file), 'compose_sha256': digest(compose_file),
                        'compose_env_sha256': digest(compose_env),
                        'secrets': {str(p): digest(p) for p in sorted((ROOT / 'secrets').iterdir()) if p.is_file()}}
            atomic_write(paths[name] / 'selection.json', (json.dumps(manifest, indent=2) + '\n').encode())
        print('Prepared before/after selections; runtime unchanged')
        return
    before, after = (recovery / 'before.env').read_bytes(), (recovery / 'after.env').read_bytes()
    manifest = json.loads((paths['after'] / 'selection.json').read_text())
    if runtime.read_bytes() != before or update_environment(before.decode(), args.issuer, args.key).encode() != after:
        raise ValueError('runtime changed since preparation')
    if digest(compose_file) != manifest['compose_sha256'] or digest(compose_env) != manifest['compose_env_sha256']:
        raise ValueError('compose changed since preparation')
    if any(digest(Path(p)) != expected for p, expected in manifest['secrets'].items()):
        raise ValueError('signing material changed since preparation')
    command = ['docker', 'compose', '--env-file', str(compose_env), '-f', str(compose_file)]

    def activate(data):
        atomic_write(runtime, data)
        subprocess.run(command + ['config', '--quiet'], check=True, capture_output=True, timeout=30)
        subprocess.run(command + ['up', '-d', '--no-deps', '--force-recreate', 'albumgateway'],
                       check=True, capture_output=True, timeout=120)
        ready()

    try:
        activate(after)
    except Exception:
        try:
            activate(before)
        except Exception:
            raise RuntimeError('activation and recovery failed; inspect private diagnostics') from None
        raise RuntimeError('activation failed; previous API configuration restored') from None
    print('Albums Sandbox API ready; image and schema unchanged')


if __name__ == '__main__':
    main()
