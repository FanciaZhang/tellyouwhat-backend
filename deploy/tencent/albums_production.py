"""First production Albums data/worker deployment, invoked under release.py's locks."""

import hashlib
import json
from pathlib import Path
import re
import tarfile
import time

from ops_common import OperationError, atomic_json, atomic_text, read_environment
from ops_artifacts import register, require_operator
from ops_albums import PROJECT, NETWORK, REGISTRY, private_file
from albums_artifacts import PRODUCTION_FILES, production_keys, selected_files
from albums_checkpoint import wrapper
from blue_green import assert_route, read_state, storage_check, require_memory_capacity
from ops_backup import create_backup, verify_backup


def compose(folder, *args):
    return ['docker', 'compose', '--project-name', PROJECT, '--env-file', str(folder / 'compose.env'),
            '-f', str(folder / 'compose.albums-production.yaml'), *args]


def selection(runtime, source):
    if not re.fullmatch('[a-f0-9]{40}', source or ''):
        raise OperationError('Albums resources require a complete source revision')
    folder = runtime.root / '.albums-production/releases' / source
    owner = runtime.root.stat().st_uid
    for path in (folder, folder.parent, folder.parent.parent):
        if path.is_symlink() or not path.is_dir() or path.stat().st_uid != owner or path.stat().st_mode & 0o077:
            raise OperationError('Albums resource selection must be private and operator owned')
    files = selected_files(runtime, folder, source, PRODUCTION_FILES)
    config = read_environment(folder / 'compose.env')
    base = runtime.root.parent / 'albums-production'
    expected = {'ALBUM_PRODUCTION_ENV_FILE': str(folder / 'runtime.env'),
                'ALBUM_PRODUCTION_SECRET_DIRECTORY': str(base / 'secrets'),
                'ALBUM_PRODUCTION_IMAGE_PREFIX': PROJECT, 'ALBUM_PRODUCTION_IMAGE_TAG': source,
                'ALBUM_PRODUCTION_NETWORK_CIDR': '172.29.94.0/24', 'ALBUM_PRODUCTION_MYSQL_USER': 'albums_production'}
    credentials = ['ALBUM_PRODUCTION_' + key for key in ('MYSQL_PASSWORD', 'MYSQL_ROOT_PASSWORD', 'REDIS_PASSWORD')]
    if (set(config) != set(expected) | set(credentials) or any(config.get(k) != v for k, v in expected.items())
            or any(not re.fullmatch('[a-f0-9]{64}', config.get(k, '')) for k in credentials)
            or len({config[k] for k in credentials}) != 3
            or any(config[k] in runtime.config.values() for k in credentials)):
        raise OperationError('Albums resource credentials, source or network are not isolated')
    app = read_environment(folder / 'runtime.env')
    keys = production_keys(app)
    required_values = ('ALBUM_COS_ARCHIVE_SECRET_ID', 'ALBUM_COS_ARCHIVE_SECRET_KEY',
                       'ALBUM_COS_UPLOAD_SECRET_ID', 'ALBUM_COS_UPLOAD_SECRET_KEY', 'ALBUM_APPLE_SIGN_IN_KEY_ID')
    if (any(not app.get(k) for k in required_values) or any(not k.startswith('ALBUM_') for k in app) or any(app.get(k) != v for k, v in REGISTRY.items() if k != 'ALBUM_DEPLOYMENT_ENVIRONMENT')
            or app.get('ALBUM_COS_REGION') != 'ap-guangzhou'
            or any(app.get(k) for k in ('ALBUM_DATABASE_DSN', 'ALBUM_REDIS_URL', 'ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS',
                                      'ALBUM_DEVELOPMENT_CLOUD_EXPIRES_AT', 'ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES'))
            or any(config[k] in value for k in credentials for value in app.values())):
        raise OperationError('Albums resource runtime contains another environment or database credentials')
    for name in keys:
        path = base / 'secrets' / name
        if (path.parent.is_symlink() or path.is_symlink() or not path.is_file() or path.stat().st_uid != 0
                or path.stat().st_gid != 65532 or path.stat().st_mode & 0o777 != 0o440 or not 0 < path.stat().st_size <= 16384):
            raise OperationError('Albums resource signing material must be prepared with restricted ownership')
    private_file(base / 'backup.env', owner, 8192)
    backup = read_environment(base / 'backup.env')
    expected_backup = {'MYSQL_HOST': 'localhost', 'MYSQL_PORT': '3306', 'MYSQL_DATABASE': 'albums_production',
                       'MYSQL_USER': 'albums_production_backup'}
    if (set(backup) != set(expected_backup) | {'MYSQL_PASSWORD', 'ALBUM_BACKUP_ENCRYPTION_KEY'}
            or any(backup.get(k) != v for k, v in expected_backup.items())
            or any(not re.fullmatch('[a-f0-9]{64}', backup.get(k, '')) for k in ('MYSQL_PASSWORD', 'ALBUM_BACKUP_ENCRYPTION_KEY'))
            or len({*map(config.get, credentials), backup['MYSQL_PASSWORD'], backup['ALBUM_BACKUP_ENCRYPTION_KEY']}) != 5):
        raise OperationError('Albums independent backup credentials must be prepared before resource creation')
    with tarfile.open(folder / 'source.tar') as archive:
        members = archive.getmembers()
        if (sum(m.size for m in members) > 128 * 1024**2
                or len({m.name for m in members}) != len(members)
                or any(Path(m.name).is_absolute() or '..' in Path(m.name).parts or not (m.isfile() or m.isdir()) for m in members)):
            raise OperationError('unsafe Albums production source archive')
        names = {m.name for m in members if m.isfile()}
        if not {'Dockerfile', 'go.mod', 'go.sum', 'cmd/albumworker/main.go', 'cmd/migrate/main.go',
                'deploy/albums/backup.py', 'deploy/albums/deletion_ledger.py', 'compose.albums-production.yaml'} <= names:
            raise OperationError('Albums production source is incomplete')
        if archive.extractfile('compose.albums-production.yaml').read() != files['compose.albums-production.yaml']:
            raise OperationError('Albums Compose descriptor differs from its selected source')
        migrations = sorted(Path(name).name for name in names if re.fullmatch(r'migrations/\d{4}_[^/]+\.sql', name))
        if not migrations:
            raise OperationError('Albums production source has no database schema')
    return folder, config, backup, migrations


def validate_compose(runtime, folder, source):
    parsed = json.loads(runtime.execute('albums-production-compose', compose(folder, 'config', '--format', 'json')))
    if parsed.get('name') != PROJECT or set(parsed.get('services', {})) != {'mysql', 'redis', 'migrate', 'albumworker'}:
        raise OperationError('Albums resources must contain only their database, Redis, migration and worker')
    if (set(parsed.get('networks', {})) != {'albums-production'}
            or parsed['networks']['albums-production'].get('external')
            or parsed['networks']['albums-production'].get('ipam', {}).get('config') != [{'subnet': '172.29.94.0/24'}]):
        raise OperationError('Albums resource network is not isolated')
    volumes = parsed.get('volumes', {})
    if (set(volumes) != {'albums-production-mysql'} or volumes['albums-production-mysql'].get('external')
            or volumes['albums-production-mysql'].get('name') != PROJECT + '_albums-production-mysql'):
        raise OperationError('Albums database requires its own persistent volume')
    for name, value in parsed['services'].items():
        mounts = value.get('volumes', [])
        if (value.get('ports') or value.get('privileged') or value.get('network_mode') or value.get('pid')
                or set(value.get('networks', {})) != {'albums-production'}
                or (mounts if name != 'mysql' else len(mounts) != 1 or any(m.get('type') != 'volume'
                    or m.get('source') != 'albums-production-mysql' or m.get('target') != '/var/lib/mysql' for m in mounts))):
            raise OperationError('Albums resources cannot expose ports or mount unrelated storage')
        if name in ('albumworker', 'migrate') and value.get('image') != PROJECT + '-' + name + ':' + source:
            raise OperationError('Albums resource image does not match the selected source')


def pull_images(runtime, folder, source, source_run):
    digest = hashlib.sha256((folder / 'source.tar').read_bytes()).hexdigest()
    images = {}
    for service in ('albumworker', 'migrate'):
        repository = 'ghcr.io/fanciazhang/tellyouwhat-ai-' + service
        reference = repository + ':' + source
        runtime.execute('albums-production-pull-' + service, ['docker', 'pull', reference], timeout=900)
        details = json.loads(runtime.execute('albums-production-image', ['docker', 'image', 'inspect', reference]))[0]
        labels = details.get('Config', {}).get('Labels', {})
        digests = [d for d in details.get('RepoDigests', []) if re.fullmatch(re.escape(repository) + r'@sha256:[a-f0-9]{64}', d)]
        if (labels.get('org.opencontainers.image.revision') != source
                or labels.get('cn.tellyouwhat.source-tar-sha256') != digest
                or labels.get('cn.tellyouwhat.source-run') != source_run or len(digests) != 1):
            raise OperationError('Albums production image differs from its selected source or CI run')
        local = PROJECT + '-' + service + ':' + source
        if runtime.execute('albums-production-existing-image', ['docker', 'image', 'ls', '-q', local]).strip():
            existing = json.loads(runtime.execute('albums-production-local-image', ['docker', 'image', 'inspect', local]))[0]
            if existing.get('Id') != details.get('Id'):
                raise OperationError('Albums production local image already identifies another artifact')
        else:
            runtime.execute('albums-production-image-tag', ['docker', 'tag', digests[0], local])
        images[service] = digests[0]
    return images


def bootstrap(runtime, source, source_run):
    require_operator(runtime.root)
    if not re.fullmatch('[0-9]+', source_run or ''):
        raise OperationError('Albums resources require the verified CI run ID')
    folder, config, backup, migrations = selection(runtime, source)
    stable = read_state(runtime)
    if not stable or stable.get('phase') != 'STABLE':
        raise OperationError('Albums resource creation requires a stable production gateway')
    assert_route(stable['current'])
    state_path = runtime.state / 'albums-production.json'
    if (state_path.exists() or (runtime.state / 'artifact-owners/albums-production.json').exists()
            or runtime.execute('albums-production-existing', ['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + PROJECT]).strip()
            or runtime.execute('albums-production-volumes', ['docker', 'volume', 'ls', '-q', '--filter', 'name=^' + PROJECT + '_']).strip()):
        raise OperationError('Albums production resources already exist; refuse to reinitialize data')
    network_ids = runtime.execute('albums-production-networks', ['docker', 'network', 'ls', '-q']).decode().split()
    if network_ids:
        import ipaddress
        existing = json.loads(runtime.execute('albums-production-network-ranges', ['docker', 'network', 'inspect', *network_ids]))
        selected = ipaddress.ip_network('172.29.94.0/24')
        if any(selected.overlaps(ipaddress.ip_network(c['Subnet'])) for n in existing for c in n.get('IPAM', {}).get('Config', [])
               if c.get('Subnet') and ':' not in c['Subnet']):
            raise OperationError('Albums production subnet overlaps an existing network')
    validate_compose(runtime, folder, source)
    storage_check(runtime)
    require_memory_capacity(dual=False)
    before = create_backup(runtime)
    verify_backup(runtime, runtime.backups / before['filename'])
    images = pull_images(runtime, folder, source, source_run)
    base = runtime.root.parent / 'albums-production'
    database = ''.join(k + '=' + config[k] + '\n' for k in sorted(config) if k.endswith('PASSWORD'))
    if (base / 'database.env').is_symlink():
        raise OperationError('Albums database credentials cannot be a symlink')
    if (base / 'database.env').exists() and private_file(base / 'database.env', runtime.root.stat().st_uid) != database.encode():
        raise OperationError('prepared Albums database credentials differ from the selected source')
    atomic_text(base / 'database.env', database)
    binaries = base / 'backup-bin'
    binaries.mkdir(mode=0o700, exist_ok=True)
    if binaries.is_symlink() or binaries.stat().st_uid != runtime.root.stat().st_uid or binaries.stat().st_mode & 0o077:
        raise OperationError('Albums backup wrapper directory must be private')
    for name in ('mysql', 'mysqldump'):
        target = binaries / name
        if target.is_symlink() or (target.exists() and target.read_bytes() != wrapper(name, 'production')):
            raise OperationError('Albums backup wrapper differs from the production container')
        atomic_text(target, wrapper(name, 'production').decode())
        target.chmod(0o700)
    register(runtime, 'albums-production', [str(folder)], [PROJECT + '-' + s + ':' + source for s in ('albumworker', 'migrate')], source, source_run)
    try:
        runtime.execute('albums-production-databases', compose(folder, 'up', '-d', '--wait', '--wait-timeout', '120', 'mysql', 'redis'), timeout=180)
        runtime.execute('albums-production-migrate', compose(folder, 'run', '--rm', '--no-deps', 'migrate'), timeout=300)
        rows = runtime.execute('albums-production-schema', ['docker', 'exec', PROJECT + '-mysql-1', 'sh', '-c',
            'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql --user="$MYSQL_USER" --database=albums_production --batch --skip-column-names '
            '-e "SELECT name FROM schema_migrations ORDER BY name;"']).decode().splitlines()
        if rows != migrations:
            raise OperationError('Albums production migration differs from the selected source')
        sql = ("CREATE USER 'albums_production_backup'@'%' IDENTIFIED BY '" + backup['MYSQL_PASSWORD'] + "';\n"
               "GRANT SELECT, SHOW VIEW ON albums_production.* TO 'albums_production_backup'@'%';\n")
        runtime.execute('albums-production-backup-user', ['docker', 'exec', '-i', PROJECT + '-mysql-1', 'sh', '-c',
            'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --user=root'], input=sql.encode())
        record = {'source': source, 'release': str(folder), 'phase': 'prepared', 'images': images, 'source_run': source_run, 'migrations': len(migrations),
                  'latest_migration': migrations[-1], 'pre_creation_backup': before['filename'], 'public_route_verified': False}
        atomic_json(state_path, record)
        snapshot = create_backup(runtime)
        manifest = verify_backup(runtime, runtime.backups / snapshot['filename'])
        recovery = manifest.get('recovery_albums_production', {})
        if (recovery.get('current/selection.json') != hashlib.sha256((folder / 'selection.json').read_bytes()).hexdigest()
                or not any(k.startswith('checkpoint/albums-') and k.endswith('.sql.gz.enc') for k in recovery)):
            raise OperationError('Albums production requires a complete authenticated recovery checkpoint')
        runtime.execute('albums-production-worker', compose(folder, 'up', '-d', '--no-build', '--no-deps', 'albumworker'), timeout=120)
        verify_worker(runtime, source)
        assert_route(stable['current'])
        record.update(phase='ready', backup=snapshot['filename'])
        atomic_json(state_path, record)
        return {'passed': True, 'resources': 'albums-production', 'source': source, 'gateway_changed': False}
    except Exception:
        runtime.execute('albums-production-stop-failed', compose(folder, 'stop', '-t', '40', 'albumworker'), timeout=90)
        assert_route(stable['current'])
        raise


def verify_worker(runtime, source):
    name = PROJECT + '-albumworker-1'
    digest = hashlib.sha256((runtime.root / '.albums-production/releases' / source / 'source.tar').read_bytes()).hexdigest()
    for _ in range(30):
        container = json.loads(runtime.execute('albums-production-worker-state', ['docker', 'inspect', name]))[0]
        if (not container.get('State', {}).get('Running') or container['State'].get('OOMKilled')
                or container.get('RestartCount') or container.get('Mounts')
                or container.get('Config', {}).get('Image') != PROJECT + '-albumworker:' + source
                or set(container.get('NetworkSettings', {}).get('Networks', {})) != {NETWORK}):
            raise OperationError('Albums media worker failed to start in its production resources')
        image = json.loads(runtime.execute('albums-production-worker-image',
            ['docker', 'image', 'inspect', PROJECT + '-albumworker:' + source]))[0]
        labels = image.get('Config', {}).get('Labels', {})
        if (image.get('Id') != container.get('Image') or labels.get('org.opencontainers.image.revision') != source
                or labels.get('cn.tellyouwhat.source-tar-sha256') != digest):
            raise OperationError('Albums media worker image differs from its selected source')
        logs = runtime.execute('albums-production-worker-startup', ['sh', '-c',
            'exec docker logs --since "$1" --tail 200 "$2" 2>&1', 'albums-production-startup', container['State']['StartedAt'], name]).decode().splitlines()
        if any(json.loads(line).get('msg') == 'album workers started' for line in logs if line.startswith('{')):
            return
        time.sleep(1)
    raise OperationError('Albums media worker did not finish startup validation')
