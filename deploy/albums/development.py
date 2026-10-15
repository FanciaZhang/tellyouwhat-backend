"""Bootstrap the isolated Albums .dev stack without changing the legacy service."""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tarfile
import time
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'tencent'))
from ops_common import OperationError, Runtime, atomic_json, read_environment
from ops_artifacts import development_deploy, require_operator
from blue_green import assert_route

PROJECT = 'tellyouwhat-albums-development'
HOST = 'api-test.albums.tellyouwhat.cn'
IMAGE_PREFIX = 'tellyouwhat-albums-test-development-'
SERVICES = ('albumgateway', 'albumworker', 'migrate')
FILES = {'source.tar': 64 * 1024**2, 'compose.albums-test.yaml': 32768,
         'runtime.env': 32768, 'compose.env': 16384}


def private_file(runtime, path, limit):
    if (path.is_symlink() or not path.is_file() or not 0 < path.stat().st_size <= limit
            or path.stat().st_uid != runtime.root.stat().st_uid or path.stat().st_mode & 0o077):
        raise OperationError('Albums deployment inputs must be bounded, private and operator owned')
    return path.read_bytes()


def selection(runtime, source):
    if not re.fullmatch('[a-f0-9]{40}', source or ''):
        raise OperationError('a complete Albums source revision is required')
    folder = runtime.root / '.albums-development/releases' / source
    for path in (folder.parent.parent, folder.parent, folder):
        if (path.is_symlink() or not path.is_dir() or path.stat().st_mode & 0o077
                or path.stat().st_uid != runtime.root.stat().st_uid):
            raise OperationError('Albums deployment directory must be private and operator owned')
    record = json.loads(private_file(runtime, folder / 'selection.json', 4096))
    if set(record) != {'source', 'files'} or record['source'] != source or set(record['files']) != set(FILES):
        raise OperationError('Albums source selection is incomplete')
    for name, limit in FILES.items():
        if hashlib.sha256(private_file(runtime, folder / name, limit)).hexdigest() != record['files'][name]:
            raise OperationError('Albums source or configuration changed after selection')
    compose = read_environment(folder / 'compose.env')
    required = {'ALBUM_TEST_ENV_FILE': str(folder / 'runtime.env'),
                'ALBUM_TEST_SECRET_DIRECTORY': str(runtime.root.parent / 'albums-development/secrets'),
                'ALBUM_TEST_IMAGE_PREFIX': IMAGE_PREFIX[:-1], 'ALBUM_TEST_IMAGE_TAG': source,
                'ALBUM_TEST_API_PORT': '18092', 'ALBUM_TEST_NETWORK_CIDR': '172.29.92.0/24',
                'ALBUM_TEST_PROXY_CIDR': '172.29.92.1/32', 'ALBUM_TEST_MYSQL_USER': 'albums_development_test'}
    if any(compose.get(k) != v for k, v in required.items()):
        raise OperationError('Albums development resources are not isolated')
    passwords = [compose.get(k, '') for k in ('ALBUM_TEST_MYSQL_PASSWORD', 'ALBUM_TEST_MYSQL_ROOT_PASSWORD', 'ALBUM_TEST_REDIS_PASSWORD')]
    if any(not re.fullmatch('[a-f0-9]{64}', p) for p in passwords) or len(set(passwords)) != 3:
        raise OperationError('independent generated database and Redis credentials are required')
    app = read_environment(folder / 'runtime.env')
    required = {'ALBUM_API_HOST': HOST, 'ALBUM_APPLE_BUNDLE_ID': 'cn.tellyouwhat.albums.dev',
                'ALBUM_APPLE_TEAM_ID': 'T6DD96HY2B',
                'ALBUM_APPLE_SIGN_IN_KEY_ID': '9H4R3FJ9D8',
                'ALBUM_COS_BUCKET': 'tellyouwhat-albums-1251995834', 'ALBUM_COS_REGION': 'ap-guangzhou',
                'ALBUM_SUBSCRIPTIONS_ENABLED': 'false', 'ALBUM_SHARING_ENABLED': 'false',
                'ALBUM_TRASH_ENABLED': 'false', 'ALBUM_ACCOUNT_MEDIA_ERASURE_ENABLED': 'false',
                'ALBUM_ACCOUNT_RECORD_ERASURE_ENABLED': 'false', 'ALBUM_RETENTION_PUSH_ENABLED': 'false'}
    required.update({k: 'false' for k in ('ALBUM_PHOTO_PROCESSING_ENABLED', 'ALBUM_VIDEO_PROCESSING_ENABLED', 'ALBUM_SUBSCRIPTION_REBINDING_ENABLED')})
    if (any(app.get(k) != v for k, v in required.items())
            or any(p in value for p in passwords for value in app.values())
            or any(app.get(k) for k in ('ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES', 'ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS', 'ALBUM_DEVELOPMENT_CLOUD_EXPIRES_AT'))):
        raise OperationError('Albums bootstrap identity, features or credential boundary is invalid')
    with tarfile.open(folder / 'source.tar') as archive:
        members = archive.getmembers()
        if (sum(m.size for m in members) > 128 * 1024**2 or any(
                Path(m.name).is_absolute() or '..' in Path(m.name).parts or not (m.isfile() or m.isdir()) for m in members)):
            raise OperationError('unsafe Albums source archive')
        names = {m.name for m in members if m.isfile()}
        if not {'Dockerfile', 'go.mod', 'go.sum', 'cmd/albumgateway/main.go', 'cmd/albumworker/main.go', 'cmd/migrate/main.go'} <= names:
            raise OperationError('Albums build source is incomplete')
        migrations = sorted(name.split('/')[-1] for name in names if re.fullmatch(r'migrations/\d{4}_[^/]+\.sql', name))
        if not migrations:
            raise OperationError('Albums source contains no migrations')
    return folder, record, migrations


def compose(folder, *args):
    return ['docker', 'compose', '--project-name', PROJECT, '--env-file', str(folder / 'compose.env'),
            '-f', str(folder / 'compose.albums-test.yaml'), *args]


def ready():
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    for _ in range(30):
        try:
            request = urllib.request.Request('http://127.0.0.1:18092/readyz', headers={'Host': HOST})
            with opener.open(request, timeout=2) as response:
                if response.geturl() == request.full_url and response.status == 200 and json.load(response) == {'code': 'ok'}:
                    return
        except (OSError, ValueError):
            pass
        time.sleep(1)
    raise OperationError('Albums development readiness failed')


def activate(runtime, source):
    require_operator(runtime.root)
    for name in ('operations', 'deployment'):
        with (runtime.state / (name + '.lock')).open('r') as handle:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                continue
            raise OperationError('Albums bootstrap must run under both owner locks')
    folder, record, migrations = selection(runtime, source)
    stable = json.loads((runtime.state / 'blue-green.json').read_text())
    if stable.get('phase') != 'STABLE':
        raise OperationError('wait for a stable production release')
    assert_route(stable['current'])
    # shortcut: bootstrap only; existing development data requires a reviewed upgrade and rollback selection.
    if (runtime.state / 'albums-development.json').exists() or runtime.execute('albums-development-existing', [
            'docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + PROJECT]).strip():
        raise OperationError('development deployment already exists; inspect it instead of repeating bootstrap')
    if runtime.execute('albums-development-volumes', ['docker', 'volume', 'ls', '-q', '--filter', 'name=^' + PROJECT + '_']).strip():
        raise OperationError('development data already exists; do not reinitialize it')
    runtime.execute('albums-development-compose', compose(folder, 'config', '--quiet'))
    for service in SERVICES:
        image = IMAGE_PREFIX + service + ':' + source
        if runtime.execute('albums-development-image-existing', ['docker', 'image', 'ls', '-q', image]).strip():
            details = json.loads(runtime.execute('albums-development-image-check', ['docker', 'image', 'inspect', image]))[0]
            labels = details.get('Config', {}).get('Labels', {})
            if labels.get('org.opencontainers.image.revision') != source or labels.get('cn.tellyouwhat.source-tar-sha256') != record['files']['source.tar']:
                raise OperationError('Albums image tag already identifies another source')
            continue
        runtime.execute('albums-development-build-' + service, ['docker', 'build', '--network=default',
            '--memory=1024m', '--memory-swap=1536m', '--cpu-quota=100000',
            '--build-arg', 'GOPROXY=https://goproxy.cn,direct', '--build-arg', 'SERVICE=' + service,
            '--label', 'org.opencontainers.image.revision=' + source,
            '--label', 'cn.tellyouwhat.source-tar-sha256=' + record['files']['source.tar'], '-t', image, '-'],
            input=(folder / 'source.tar').read_bytes(), timeout=700)
    try:
        runtime.execute('albums-development-databases', compose(folder, 'up', '-d', '--wait', '--wait-timeout', '120', 'mysql', 'redis'), timeout=180)
        runtime.execute('albums-development-migrate', compose(folder, 'run', '--rm', '--no-deps', 'migrate'), timeout=180)
        runtime.execute('albums-development-start', compose(folder, 'up', '-d', '--no-build', '--no-deps', 'albumgateway', 'albumworker'), timeout=120)
        ready()
        rows = runtime.execute('albums-development-schema', ['docker', 'exec', PROJECT + '-mysql-1', 'sh', '-c',
            'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql --user="$MYSQL_USER" --database=albums_test --batch --skip-column-names '
            '-e "SELECT COUNT(*),MAX(name) FROM schema_migrations; SELECT COUNT(*) FROM album_accounts; SELECT COUNT(*) FROM album_uploads;"']).decode().splitlines()
        if rows != [str(len(migrations)) + '\t' + migrations[-1], '0', '0']:
            raise OperationError('fresh development database does not match the selected schema')
        assert_route(stable['current'])
        result = {'source': source, 'release': str(folder), 'project': PROJECT, 'api_host': HOST, 'port': 18092,
                  'files': record['files'], 'migrations': len(migrations), 'latest_migration': migrations[-1],
                  'previous': None, 'accounts': 0, 'uploads': 0, 'public_route_verified': False,
                  'real_apple_login_verified': False, 'backup_verified': False}
        atomic_json(runtime.state / 'albums-development.json', result)
        return result
    except Exception:
        runtime.execute('albums-development-stop-failed', compose(folder, 'stop', '-t', '40', 'albumgateway', 'albumworker'), timeout=90)
        assert_route(stable['current'])
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', default='/opt/tellyouwhat/backend')
    parser.add_argument('--source', required=True)
    parser.add_argument('--source-run', default='albums-development-bootstrap')
    parser.add_argument('--activate', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    runtime = Runtime(args.backend_dir)
    folder, _, _ = selection(runtime, args.source)
    if args.activate:
        activate(runtime, args.source)
        return
    command = [sys.executable, str(Path(__file__).resolve()), '--backend-dir', str(runtime.root), '--source', args.source, '--activate']
    result = development_deploy(runtime, 'albums-test', [str(folder)], [IMAGE_PREFIX + s + ':' + args.source for s in SERVICES],
                               args.source, args.source_run, command, 'http://127.0.0.1:18092/readyz', verify_host=HOST)
    print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (OperationError, OSError, ValueError, tarfile.TarError):
        print(json.dumps({'passed': False, 'error': 'Albums development bootstrap failed; inspect private operations diagnostics'}))
        sys.exit(1)
