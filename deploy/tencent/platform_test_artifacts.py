"""Back up the isolated platform test control plane with its own read account and key."""

import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re

from ops_common import OperationError, Runtime, read_environment
from ops_backup import create_backup, verify_backup
import ops_offsite
from platform_test_deploy import FILES, private_file, selection, selection_files
import platform_test_albums as albums


def checkpoint_runtime(runtime):
    app = runtime.root.parent / 'platform-test'
    if app.is_symlink() or not app.is_dir() or app.stat().st_uid != os.getuid() or app.stat().st_mode & 0o077:
        raise OperationError('platform test backup root must be private and operator owned')
    settings = app / 'backup.env'
    private_file(runtime, settings, 8192)
    config = read_environment(settings)
    required = {'MYSQL_DATABASE': 'platform_test', 'MYSQL_USER': 'platform_backup_test', 'MYSQL_PORT': '3306'}
    if (set(config) != set(required) | {'MYSQL_HOST', 'MYSQL_PASSWORD', 'BACKUP_ENCRYPTION_KEY'}
            or any(config.get(k) != v for k, v in required.items())
            or any(not re.fullmatch('[a-f0-9]{64}', config.get(k, '')) for k in ('MYSQL_PASSWORD', 'BACKUP_ENCRYPTION_KEY'))
            or config['MYSQL_PASSWORD'] == config['BACKUP_ENCRYPTION_KEY']
            or config['BACKUP_ENCRYPTION_KEY'] == runtime.config.get('BACKUP_ENCRYPTION_KEY')):
        raise OperationError('platform test backup database/key isolation is invalid')
    try:
        if ipaddress.ip_address(config['MYSQL_HOST']) not in ipaddress.ip_network('172.29.93.0/24'):
            raise ValueError()
    except ValueError as error:
        raise OperationError('platform test backup host is outside its isolated network') from error
    container = json.loads(runtime.execute('platform-test-backup-target', ['docker', 'inspect', 'tellyouwhat-platform-test-mysql-1']))[0]
    networks = container.get('NetworkSettings', {}).get('Networks', {})
    if (not container.get('State', {}).get('Running') or set(networks) != {'tellyouwhat-platform-test_platform-test'}
            or networks['tellyouwhat-platform-test_platform-test']['IPAddress'] != config['MYSQL_HOST']):
        raise OperationError('platform test backup does not target its dedicated MySQL')
    for name in ('backups', 'backup-runtime'):
        path = app / name
        path.mkdir(mode=0o700, exist_ok=True)
        if path.is_symlink() or path.stat().st_uid != os.getuid() or path.stat().st_mode & 0o077:
            raise OperationError('platform test backup directories must be private')
    child = Runtime(app / 'backup-runtime', app / 'backups', settings)
    if ops_offsite.enabled(child):
        raise OperationError('platform test checkpoints must use the outer authenticated transport')
    return child


def backup_configuration(runtime, destination):
    state_path = runtime.state / 'platform-test.json'
    if not state_path.exists():
        return {}
    state = json.loads(private_file(runtime, state_path, 16384))
    folder, record, _ = selection(runtime, state.get('source'))
    if albums.selected(folder):
        albums.require_configuration(runtime, folder)
    owner_path = runtime.state / 'artifact-owners/platform-test.json'
    owner = json.loads(private_file(runtime, owner_path, 16384))
    if (state.get('release') != str(folder) or not any(
            s and s.get('source') == state['source'] and str(folder) in s.get('paths', [])
            for s in (owner.get('current'), owner.get('previous')))):
        raise OperationError('platform test recovery selection is not registered')
    inputs = {'state.json': state_path, 'owner.json': owner_path,
              'database.env': runtime.root.parent / 'platform-test/database.env',
              **{'current/' + name: folder / name for name in (*selection_files(folder), 'selection.json')}}
    data = {name: private_file(runtime, path, 64 * 1024**2 if name.endswith('source.tar') else 32768)
            for name, path in inputs.items()}
    child = checkpoint_runtime(runtime)
    snapshot = create_backup(child)
    manifest = verify_backup(child, child.backups / snapshot['filename'])
    if manifest['database'] != 'platform_test' or manifest.get('recovery_filename'):
        raise OperationError('platform test checkpoint has an unexpected database or transport')
    for name in (snapshot['filename'], snapshot['filename'] + '.json'):
        data['checkpoint/' + name] = private_file(runtime, child.backups / name, 32 * 1024**2)
    data['backup.env'] = ''.join(k + '=' + v + '\n' for k, v in child.config.items() if k != 'BACKUP_ENCRYPTION_KEY').encode()
    if any(child.config['BACKUP_ENCRYPTION_KEY'].encode() in content for content in data.values()):
        raise OperationError('platform test recovery cannot include its SQL decryption key')
    destination.mkdir(mode=0o700)
    for name, content in data.items():
        path = destination / name
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, 'wb') as output:
            output.write(content)
    return {name: hashlib.sha256(content).hexdigest() for name, content in data.items()}


def verify_recovery(archive, hashes):
    expected = {'state.json', 'owner.json', 'database.env', 'backup.env', *('current/' + name for name in (*FILES, 'selection.json'))}
    if any('current/' + name in hashes for name in albums.FILES):
        expected.update('current/' + name for name in albums.FILES)
    extra = set(hashes) - expected
    snapshots = [name for name in extra if re.fullmatch(r'checkpoint/mysql-\d{8}T\d{6}Z-[a-f0-9]{6}\.sql\.gz\.enc', name)]
    if not expected <= set(hashes) or len(extra) != 2 or len(snapshots) != 1 or snapshots[0] + '.json' not in extra:
        raise OperationError('platform test recovery files are incomplete')
    for name, digest in hashes.items():
        try:
            member = archive.getmember('recovery/platform-test/' + name)
        except KeyError as error:
            raise OperationError('platform test recovery file is missing') from error
        if not member.isfile() or hashlib.sha256(archive.extractfile(member).read()).hexdigest() != digest:
            raise OperationError('platform test recovery differs from its authenticated manifest')
