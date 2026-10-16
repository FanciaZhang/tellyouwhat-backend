"""Reuse Albums' full snapshot and deletion fences with an environment-specific read account."""

import importlib.util
import json
import os
from pathlib import Path
import re
import sys
import tarfile
import tempfile
import time

from ops_common import OperationError, read_environment


DATABASES = {
    'development': ('albums_test', 'albums_development_backup_test'),
    'production': ('albums_production', 'albums_production_backup'),
}


def database_scope(environment):
    if environment not in DATABASES:
        raise OperationError('unknown Albums backup environment')
    return DATABASES[environment]


def wrapper(name, environment='development'):
    database_scope(environment)
    if name not in ('mysql', 'mysqldump'):
        raise OperationError('unknown Albums backup client')
    return ('#!/bin/sh\nset -eu\nexec docker exec --env MYSQL_PWD '
            'tellyouwhat-albums-' + environment + '-mysql-1 ' + name + ' "$@"\n').encode()


def private(path, maximum):
    if (path.is_symlink() or not path.is_file() or path.stat().st_uid != os.getuid()
            or path.stat().st_mode & 0o077 or not 0 < path.stat().st_size <= maximum):
        raise OperationError('Albums checkpoint requires private operator-owned inputs')
    return path.read_bytes()


def checkpoint(runtime, folder, environment='development'):
    database, user = database_scope(environment)
    app = runtime.root.parent / ('albums-' + environment)
    settings = app / 'backup.env'
    private(settings, 8192)
    config = read_environment(settings)
    required = {'MYSQL_HOST': 'localhost', 'MYSQL_PORT': '3306', 'MYSQL_DATABASE': database,
                'MYSQL_USER': user}
    if (set(config) != set(required) | {'MYSQL_PASSWORD', 'ALBUM_BACKUP_ENCRYPTION_KEY'}
            or any(config.get(k) != v for k, v in required.items())
            or any(not re.fullmatch('[a-f0-9]{64}', config.get(k, '')) for k in ('MYSQL_PASSWORD', 'ALBUM_BACKUP_ENCRYPTION_KEY'))
            or config['MYSQL_PASSWORD'] == config['ALBUM_BACKUP_ENCRYPTION_KEY']):
        raise OperationError('Albums checkpoint database/key isolation is invalid')
    for name in ('backups', 'operations', 'backup-bin'):
        path = app / name
        path.mkdir(mode=0o700, exist_ok=True)
        if path.is_symlink() or path.stat().st_uid != os.getuid() or path.stat().st_mode & 0o077:
            raise OperationError('Albums checkpoint directories must be private')
    binaries = app / 'backup-bin'
    for name in ('mysql', 'mysqldump'):
        path = binaries / name
        if private(path, 1024) != wrapper(name, environment) or not path.stat().st_mode & 0o100:
            raise OperationError('Albums backup client must target only the selected environment container')
    with tempfile.TemporaryDirectory(prefix='.albums-checkpoint-', dir=runtime.state) as temporary:
        modules = {}
        with tarfile.open(folder / 'source.tar') as archive:
            for name in ('backup', 'deletion_ledger'):
                member = archive.getmember('deploy/albums/' + name + '.py')
                if not member.isfile() or not 0 < member.size < 65536:
                    raise OperationError('Albums snapshot implementation is unavailable')
                path = Path(temporary) / (name + '.py')
                path.write_bytes(archive.extractfile(member).read())
                spec = importlib.util.spec_from_file_location('albums_checkpoint_' + name, path)
                module = importlib.util.module_from_spec(spec)
                spec.loader.exec_module(module)
                modules[name] = module
        backup = modules['backup']
        child = backup.Runtime(runtime.root, app / 'backups', settings)
        child.state = app / 'operations'
        backup.configure(child)
        # The existing snapshot library imports its sibling by its original module name.
        missing = object()
        previous = sys.modules.get('deletion_ledger', missing)
        sys.modules['deletion_ledger'] = modules['deletion_ledger']
        try:
            snapshot = backup.create(child, binaries)
            sql = [str(binaries / 'mysql'), '--no-defaults', *backup.connection(child),
                   '--database=' + database, '--batch', '--skip-column-names']
            ledger = modules['deletion_ledger'].publish(child, sql, {'MYSQL_PWD': config['MYSQL_PASSWORD']})
            manifest = backup.verify(child, child.backups / snapshot['filename'])
            modules['deletion_ledger'].verify(child, child.backups / ledger['filename'], ledger['sha256'], manifest['created_at'])
            names = [snapshot['filename'], snapshot['filename'] + '.json', ledger['filename']]
            result = {'checkpoint/' + name: private(child.backups / name, 32 * 1024**2) for name in names}
            for old in child.backups.glob('albums-deletions-*.json'):
                if old.name == ledger['filename']:
                    continue
                if not re.fullmatch(r'albums-deletions-[a-f0-9]{32}\.json', old.name):
                    raise OperationError('unexpected local Albums deletion checkpoint')
                data = private(old, 32 * 1024**2)
                modules['deletion_ledger'].verify(child, old, backup.file_digest(old), 0)
                if json.loads(data)['payload']['exported_at'] <= time.time() - backup.RETENTION:
                    old.unlink()
        except ImportError as error:
            raise OperationError('Albums checkpoint implementation has an unavailable dependency') from error
        finally:
            if previous is missing:
                sys.modules.pop('deletion_ledger', None)
            else:
                sys.modules['deletion_ledger'] = previous
    # Keep the independent decryption key in its separate local escrow, outside this bundle.
    result['backup.env'] = ''.join(k + '=' + v + '\n' for k, v in config.items() if k != 'ALBUM_BACKUP_ENCRYPTION_KEY').encode()
    return result
