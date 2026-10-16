"""Deploy the independent platform test database and selected development APIs."""

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

from blue_green import assert_route
from ops_artifacts import development_deploy, require_operator
from ops_common import OperationError, Runtime, atomic_json, read_environment
from test_routes import RejectRedirects

PROJECT = 'tellyouwhat-platform-test'
HOST = 'api-test.birthday.tellyouwhat.cn'
FILES = {'source.tar': 64 * 1024**2, 'compose.platform-test.yaml': 32768,
         'runtime.env': 32768, 'compose.env': 16384}
HEALTH_HOST = 'api-test.health.tellyouwhat.cn'
TOS_SETTINGS = {'TOS_ENDPOINT', 'TOS_REGION', 'TOS_BUCKET', 'TOS_ACCESS_KEY', 'TOS_SECRET_KEY'}
HEALTH_ENDPOINTS = {'HEALTH_ARK_ENDPOINT_' + name for name in (
    'VOICE_TRANSCRIPTION', 'MEAL_PHOTO_CAPTURE', 'HYDRATION_CUP_ESTIMATE', 'MEAL_TEXT_CAPTURE',
    'MEAL_DECISION', 'DIET_ANALYSIS', 'HEALTH_NUTRITION_ANALYSIS', 'HEALTH_BEHAVIOR_ANALYSIS')}
HEALTH_SETTINGS = HEALTH_ENDPOINTS | {'HEALTH_APPLE_BUNDLE_ID', 'HEALTH_APPLE_TEAM_ID', 'HEALTH_API_HOSTS',
    'HEALTH_APP_ATTEST_ENV', 'HEALTH_ALLOWED_APP_BUILDS', 'HEALTH_SCHEMA_MANIFEST_PATH',
    'HEALTH_ARK_API_KEY', 'HEALTH_ARK_BASE_URL'}


def applications(folder):
    return read_environment(folder / 'runtime.env').get('PLATFORM_APPS', 'birthday').split(',')


def running_services(folder):
    return ('gateway', 'worker') if 'health' in applications(folder) else ('gateway',)


def private_file(runtime, path, limit):
    if (path.is_symlink() or not path.is_file() or path.stat().st_uid != runtime.root.stat().st_uid
            or path.stat().st_mode & 0o077 or not 0 < path.stat().st_size <= limit):
        raise OperationError('platform test inputs must be private, bounded and operator owned')
    return path.read_bytes()


def selection(runtime, source):
    if not re.fullmatch('[a-f0-9]{40}', source or ''):
        raise OperationError('a complete platform test source revision is required')
    folder = runtime.root / '.platform-test/releases' / source
    for path in (folder.parent.parent, folder.parent, folder):
        if (path.is_symlink() or not path.is_dir() or path.stat().st_mode & 0o077
                or path.stat().st_uid != runtime.root.stat().st_uid):
            raise OperationError('platform test release must be private and operator owned')
    record = json.loads(private_file(runtime, folder / 'selection.json', 4096))
    if set(record) != {'source', 'files'} or record['source'] != source or set(record['files']) != set(FILES):
        raise OperationError('platform test source selection is incomplete')
    for name, limit in FILES.items():
        if hashlib.sha256(private_file(runtime, folder / name, limit)).hexdigest() != record['files'][name]:
            raise OperationError('platform test input changed after selection')
    compose = read_environment(folder / 'compose.env')
    required = {'PLATFORM_TEST_ENV_FILE': str(folder / 'runtime.env'), 'PLATFORM_TEST_IMAGE_TAG': source,
                'PLATFORM_TEST_API_PORT': '18790', 'PLATFORM_TEST_NETWORK_CIDR': '172.29.93.0/24'}
    if any(compose.get(k) != v for k, v in required.items()):
        raise OperationError('platform test resources are not isolated')
    passwords = [compose.get(k, '') for k in ('PLATFORM_TEST_MYSQL_PASSWORD', 'PLATFORM_TEST_MYSQL_ROOT_PASSWORD', 'PLATFORM_TEST_REDIS_PASSWORD')]
    if any(not re.fullmatch('[a-f0-9]{64}', p) for p in passwords) or len(set(passwords)) != 3:
        raise OperationError('independent platform test database and Redis credentials are required')
    app = read_environment(folder / 'runtime.env')
    required = {'BIRTHDAY_APPLE_BUNDLE_ID': 'cn.tellyouwhat.birthday.dev', 'BIRTHDAY_APPLE_TEAM_ID': 'T6DD96HY2B',
                'BIRTHDAY_API_HOSTS': HOST, 'BIRTHDAY_APP_ATTEST_ENV': 'production',
                'AI_PROJECT_MONTHLY_BUDGET_CNY': '10'}
    if (any(app.get(k) != v for k, v in required.items())
            or (app.get('BIRTHDAY_DEV_ACTIVATION_SECRET') and not re.fullmatch('[a-f0-9]{64}', app['BIRTHDAY_DEV_ACTIVATION_SECRET']))
            or not re.fullmatch(r'\d+(,\d+)*', app.get('BIRTHDAY_ALLOWED_APP_BUILDS', ''))
            or any(k.startswith(('JOURNAL_', 'ARK_MANAGEMENT_')) for k in app)
            or any(k in app for k in ('DATABASE_DSN', 'REDIS_URL', 'MYSQL_ROOT_PASSWORD'))
            or any(p in v for p in passwords for v in app.values())):
        raise OperationError('platform test identity, cost limit or credential boundary is invalid')
    with tarfile.open(folder / 'source.tar') as archive:
        members = archive.getmembers()
        if (sum(m.size for m in members) > 128 * 1024**2 or any(
                Path(m.name).is_absolute() or '..' in Path(m.name).parts or not (m.isfile() or m.isdir()) for m in members)):
            raise OperationError('unsafe platform test source archive')
        names = {m.name for m in members if m.isfile()}
        if not {'deploy/tencent/platform-test.Dockerfile', 'go.mod', 'go.sum', 'cmd/gateway/main.go', 'cmd/migrate/main.go'} <= names:
            raise OperationError('platform test build source is incomplete')
        if archive.extractfile('compose.platform-test.yaml').read() != (folder / 'compose.platform-test.yaml').read_bytes():
            raise OperationError('platform test Compose differs from selected source')
        config = archive.extractfile('internal/config/platform.go') if 'internal/config/platform.go' in names else None
        explicit_apps = config is not None and b'"PLATFORM_APPS"' in config.read()
        # Retained recovery selections must be validated using their own configuration contract.
        if explicit_apps:
            if (app.get('PLATFORM_APPS') not in ('birthday', 'health,birthday')
                    or any(k in app for k in ('BIRTHDAY_AI_ENABLED', 'BIRTHDAY_AI_ONLY', 'BIRTHDAY_DEV_ACTIVATION_SECRET'))):
                raise OperationError('platform test applications must be selected explicitly')
        elif (app.get('BIRTHDAY_AI_ENABLED') != 'true' or app.get('BIRTHDAY_AI_ONLY') != 'true'
              or 'PLATFORM_APPS' in app):
            raise OperationError('retained platform test configuration differs from its source contract')
        migrations = sorted(n.split('/')[-1] for n in names if re.fullmatch(r'migrations/\d{4}_[^/]+\.sql', n))
        if not migrations:
            raise OperationError('platform test source contains no migrations')
    if 'health' in applications(folder):
        health = {'HEALTH_APPLE_BUNDLE_ID': 'cn.tellyouwhat.healthapp.dev', 'HEALTH_APPLE_TEAM_ID': 'T6DD96HY2B',
                  'HEALTH_API_HOSTS': HEALTH_HOST, 'HEALTH_APP_ATTEST_ENV': 'production',
                  'HEALTH_SCHEMA_MANIFEST_PATH': '/config/schema-manifest.json',
                  'HEALTH_ARK_BASE_URL': 'https://ark.cn-beijing.volces.com',
                  'TOS_ENDPOINT': 'https://tos-cn-beijing.volces.com', 'TOS_REGION': 'cn-beijing',
                  'TOS_BUCKET': 'health-ai-test'}
        if (any(app.get(k) != v for k, v in health.items())
                or not re.fullmatch(r'\d+(,\d+)*', app.get('HEALTH_ALLOWED_APP_BUILDS', ''))
                or any(not app.get(k) for k in ('TOS_ACCESS_KEY', 'TOS_SECRET_KEY', 'HEALTH_ARK_API_KEY'))
                or any(not re.fullmatch('ep-[a-z0-9-]+', app.get(k, '')) for k in HEALTH_ENDPOINTS)
                or any(k.startswith('HEALTH_') and k not in HEALTH_SETTINGS for k in app)
                or any(k.startswith('TOS_') and k not in TOS_SETTINGS for k in app)):
            raise OperationError('Health test identity, media or model resources are invalid')
    elif any(k.startswith(('HEALTH_', 'TOS_')) for k in app):
        raise OperationError('Birthday-only test service cannot receive Health or media credentials')
    return folder, record, migrations


def compose(folder, *args):
    return ['docker', 'compose', '--project-name', PROJECT, '--env-file', str(folder / 'compose.env'),
            '-f', str(folder / 'compose.platform-test.yaml'), *args]


def ready(folder=None):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), RejectRedirects)
    for _ in range(30):
        try:
            for host in ([HOST, HEALTH_HOST] if folder and 'health' in applications(folder) else [HOST]):
                request = urllib.request.Request('http://127.0.0.1:18790/readyz', headers={'Host': host})
                with opener.open(request, timeout=2) as response:
                    if response.status != 200 or json.load(response).get('status') != 'ready':
                        raise ValueError('not ready')
            return
        except (OSError, ValueError):
            pass
        time.sleep(1)
    raise OperationError('platform test readiness failed')


def build_images(runtime, folder, record, source):
    for service in (*running_services(folder), 'migrate'):
        image = PROJECT + '-' + service + ':' + source
        existing = runtime.execute('platform-test-image-existing', ['docker', 'image', 'ls', '-q', image]).strip()
        if existing:
            labels = json.loads(runtime.execute('platform-test-image-check', ['docker', 'image', 'inspect', image]))[0]['Config']['Labels']
            if labels.get('org.opencontainers.image.revision') != source or labels.get('cn.tellyouwhat.source-tar-sha256') != record['files']['source.tar']:
                raise OperationError('platform test image tag identifies different source')
            continue
        runtime.execute('platform-test-build-' + service, ['docker', 'build', '--network=default',
            '--memory=1024m', '--memory-swap=1536m', '--cpu-quota=100000',
            '--build-arg', 'GOPROXY=https://goproxy.cn,direct', '--target', service,
            '-f', 'deploy/tencent/platform-test.Dockerfile', '--label', 'org.opencontainers.image.revision=' + source,
            '--label', 'cn.tellyouwhat.source-tar-sha256=' + record['files']['source.tar'], '-t', image, '-'],
            input=(folder / 'source.tar').read_bytes(), timeout=700)


def verify_running(runtime, folder, source, label):
    for service in running_services(folder):
        name = PROJECT + '-' + service + '-1'
        running = json.loads(runtime.execute(label + '-' + service, ['docker', 'inspect', name]))[0]
        if (running['Config']['Image'] != PROJECT + '-' + service + ':' + source or not running['State']['Running']
                or running['State'].get('OOMKilled') or running.get('RestartCount')):
            raise OperationError('platform test process differs from the selected healthy image')
        if service == 'worker':
            for attempt in range(30):
                try:
                    status = json.loads(runtime.execute(label + '-worker-status', ['docker', 'exec', name,
                        '/servicecheck', '--lifecycle', 'status', '--role', 'worker'], timeout=5))
                    if status.get('httpEnabled') and status.get('backgroundEnabled'):
                        break
                except (OperationError, ValueError):
                    pass
                if attempt == 29:
                    raise OperationError('platform test worker is not accepting work')
                time.sleep(1)


def require_separate_health_resources(runtime, folder):
    if 'health' not in applications(folder):
        return
    app = read_environment(folder / 'runtime.env')
    for key in ('HEALTH_ARK_API_KEY', 'TOS_ACCESS_KEY', 'TOS_SECRET_KEY', *HEALTH_ENDPOINTS):
        if not runtime.config.get(key) or app[key] == runtime.config[key]:
            raise OperationError('Health test credentials and endpoints must differ from verified production resources')


def update_configuration(runtime, source, previous_source, folder, record):
    state_path = runtime.state / 'platform-test.json'
    previous = json.loads(private_file(runtime, state_path, 16384))
    owner = json.loads(private_file(runtime, runtime.state / 'artifact-owners/platform-test.json', 16384))
    old, _, _ = selection(runtime, previous_source)
    if (source == previous_source or previous.get('source') != previous_source or previous.get('release') != str(old)
            or owner.get('current', {}).get('source') != previous_source
            or str(old) not in owner['current'].get('paths', [])):
        raise OperationError('platform test update does not match the registered running release')
    # shortcut: same-schema updates only; schema and infrastructure upgrades need a separate recovery path.
    if (folder / 'compose.platform-test.yaml').read_bytes() != (old / 'compose.platform-test.yaml').read_bytes():
        raise OperationError('enrollment update cannot change the platform test topology')
    before_apps, after_apps = applications(old), applications(folder)
    if not set(before_apps) <= set(after_apps):
        raise OperationError('platform test update cannot silently disable an existing application')
    permitted = {'BIRTHDAY_DEV_ACTIVATION_SECRET', 'BIRTHDAY_ALLOWED_APP_BUILDS', 'HEALTH_ALLOWED_APP_BUILDS',
                 'BIRTHDAY_AI_ENABLED', 'BIRTHDAY_AI_ONLY', 'PLATFORM_APPS'}
    if 'health' not in before_apps and 'health' in after_apps:
        permitted |= HEALTH_SETTINGS | TOS_SETTINGS
    for filename, allowed in [('compose.env', {'PLATFORM_TEST_ENV_FILE', 'PLATFORM_TEST_IMAGE_TAG'}),
                              ('runtime.env', permitted)]:
        before, after = read_environment(old / filename), read_environment(folder / filename)
        if {k: v for k, v in before.items() if k not in allowed} != {k: v for k, v in after.items() if k not in allowed}:
            raise OperationError('enrollment update cannot replace unrelated configuration or credentials')
    with tarfile.open(old / 'source.tar') as before, tarfile.open(folder / 'source.tar') as after:
        def schemas(archive):
            return {m.name: hashlib.sha256(archive.extractfile(m).read()).hexdigest() for m in archive.getmembers()
                    if m.isfile() and m.name.startswith('migrations/')}
        if schemas(before) != schemas(after):
            raise OperationError('enrollment update cannot migrate or roll back database schemas')
    verify_running(runtime, old, previous_source, 'platform-test-update-running')
    rows = runtime.execute('platform-test-update-schema', ['docker', 'exec', PROJECT + '-mysql-1', 'sh', '-c',
        'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql --user="$MYSQL_USER" --database=platform_test --batch --skip-column-names '
        '-e "SELECT COUNT(*),MAX(name) FROM schema_migrations;"']).decode().strip()
    if rows != str(previous['migrations']) + '\t' + previous['latest_migration']:
        raise OperationError('platform test applied schema differs from the rollback selection')
    from ops_backup import create_backup, verify_backup
    snapshot = create_backup(runtime)
    manifest = verify_backup(runtime, runtime.backups / snapshot['filename'])
    if not manifest.get('recovery_platform_test'):
        raise OperationError('platform test update requires an authenticated offsite recovery selection')
    runtime.execute('platform-test-compose', compose(folder, 'config', '--quiet'))
    build_images(runtime, folder, record, source)
    try:
        runtime.execute('platform-test-update-start', compose(folder, 'up', '-d', '--no-build', '--no-deps', *running_services(folder)), timeout=120)
        ready(folder)
        verify_running(runtime, folder, source, 'platform-test-update-check')
        result = {**previous, 'source': source, 'release': str(folder), 'files': record['files'],
                  'apps': after_apps,
                  'previous': {k: v for k, v in previous.items() if k != 'previous'},
                  'pre_update_backup': snapshot['filename'], 'real_app_attest_verified': False}
        atomic_json(state_path, result)
        return result
    except Exception:
        try:
            if 'worker' in running_services(folder) and 'worker' not in running_services(old):
                runtime.execute('platform-test-update-stop-worker', compose(folder, 'stop', '-t', '30', 'worker'), timeout=90)
        finally:
            runtime.execute('platform-test-update-rollback', compose(old, 'up', '-d', '--no-build', '--no-deps', *running_services(old)), timeout=120)
            ready(old)
            verify_running(runtime, old, previous_source, 'platform-test-rollback-check')
        raise


def activate(runtime, source, update_from=None):
    require_operator(runtime.root)
    for name in ('operations', 'deployment'):
        with (runtime.state / (name + '.lock')).open('r') as handle:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                continue
            raise OperationError('platform test bootstrap requires both owner locks')
    folder, record, migrations = selection(runtime, source)
    require_separate_health_resources(runtime, folder)
    stable = json.loads((runtime.state / 'blue-green.json').read_text())
    if stable.get('phase') != 'STABLE':
        raise OperationError('wait for a stable production release')
    assert_route(stable['current'])
    if update_from:
        return update_configuration(runtime, source, update_from, folder, record)
    if ((runtime.state / 'platform-test.json').exists() or runtime.execute('platform-test-existing', [
            'docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + PROJECT]).strip()
            or runtime.execute('platform-test-volumes', ['docker', 'volume', 'ls', '-q', '--filter', 'name=^' + PROJECT + '_']).strip()):
        raise OperationError('platform test data already exists; do not repeat bootstrap')
    runtime.execute('platform-test-compose', compose(folder, 'config', '--quiet'))
    build_images(runtime, folder, record, source)
    try:
        runtime.execute('platform-test-databases', compose(folder, 'up', '-d', '--wait', '--wait-timeout', '120', 'mysql', 'redis'), timeout=180)
        runtime.execute('platform-test-migrate', compose(folder, 'run', '--rm', '--no-deps', 'migrate'), timeout=180)
        runtime.execute('platform-test-start', compose(folder, 'up', '-d', '--no-build', '--no-deps', *running_services(folder)), timeout=120)
        ready(folder)
        verify_running(runtime, folder, source, 'platform-test-running')
        rows = runtime.execute('platform-test-schema', ['docker', 'exec', PROJECT + '-mysql-1', 'sh', '-c',
            'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql --user="$MYSQL_USER" --database=platform_test --batch --skip-column-names '
            '-e "SELECT COUNT(*),MAX(name) FROM schema_migrations; SELECT COUNT(*) FROM apps WHERE app_id=\'birthday\'; SELECT COUNT(*) FROM app_attest_keys;"']).decode().splitlines()
        if rows != [str(len(migrations)) + '\t' + migrations[-1], '1', '0']:
            raise OperationError('fresh platform test schema or identity rows are unexpected')
        assert_route(stable['current'])
        result = {'source': source, 'release': str(folder), 'project': PROJECT, 'apps': applications(folder), 'api_host': HOST,
                  'port': 18790, 'files': record['files'], 'migrations': len(migrations), 'latest_migration': migrations[-1],
                  'previous': None, 'public_route_verified': False, 'real_app_attest_verified': False, 'backup_verified': False}
        atomic_json(runtime.state / 'platform-test.json', result)
        return result
    except Exception:
        runtime.execute('platform-test-stop-failed', compose(folder, 'stop', '-t', '30', *running_services(folder)), timeout=90)
        assert_route(stable['current'])
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', default='/opt/tellyouwhat/backend')
    parser.add_argument('--source', required=True)
    parser.add_argument('--source-run', default='platform-test-bootstrap')
    parser.add_argument('--update-from', help='exact current source; same-schema configuration updates only')
    parser.add_argument('--activate', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    runtime = Runtime(args.backend_dir)
    folder, _, _ = selection(runtime, args.source)
    if args.activate:
        activate(runtime, args.source, args.update_from)
        return
    command = [sys.executable, str(Path(__file__).resolve()), '--backend-dir', str(runtime.root), '--source', args.source, '--activate']
    if args.update_from:
        command += ['--update-from', args.update_from]
    result = development_deploy(runtime, 'platform-test', [str(folder)], [PROJECT + '-' + s + ':' + args.source for s in (*running_services(folder), 'migrate')],
                               args.source, args.source_run, command, 'http://127.0.0.1:18790/readyz', verify_host=HOST)
    print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (OperationError, OSError, ValueError, tarfile.TarError):
        print(json.dumps({'passed': False, 'error': 'platform test bootstrap failed; inspect private operations diagnostics'}))
        sys.exit(1)
