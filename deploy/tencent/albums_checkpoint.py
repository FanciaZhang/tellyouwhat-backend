"""Reuse Albums' full snapshot and deletion fences with an environment-specific read account."""

import importlib.util
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


def mysql_container(environment):
    database_scope(environment)
    return 'tellyouwhat-albums-' + environment + '-mysql-1'


def wrapper(name, environment='development'):
    if name not in ('mysql', 'mysqldump'):
        raise OperationError('unknown Albums backup client')
    return ('#!/bin/sh\nset -eu\nexec docker exec --env MYSQL_PWD '
            + mysql_container(environment) + ' ' + name + ' "$@"\n').encode()


def private(path, maximum):
    if (path.is_symlink() or not path.is_file() or path.stat().st_uid != os.getuid()
            or path.stat().st_mode & 0o077 or not 0 < path.stat().st_size <= maximum):
        raise OperationError('Albums checkpoint requires private operator-owned inputs')
    return path.read_bytes()


def backup_settings(runtime, environment='development'):
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
    return app, config


def checkpoint(runtime, folder, environment='development'):
    app, config = backup_settings(runtime, environment)
    settings = app / 'backup.env'
    database = config['MYSQL_DATABASE']
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
        implementations = {}
        with tarfile.open(folder / 'source.tar') as archive:
            for name in ('backup', 'deletion_ledger', 'asset_deletion_ledger', 'collection_deletion_ledger'):
                if environment == 'development':
                    # Schema and all checkpoint modules advance together, outside the old data-service recipe.
                    implementation = Path(__file__).resolve().parent.parent / 'albums' / (name + '.py')
                    if (implementation.is_symlink() or not implementation.is_file()
                            or implementation.stat().st_uid != os.getuid() or implementation.stat().st_mode & 0o022
                            or not 0 < implementation.stat().st_size < 65536):
                        raise OperationError('Albums development snapshot implementation is unsafe')
                    content = implementation.read_bytes()
                else:
                    try:
                        member = archive.getmember('deploy/albums/' + name + '.py')
                    except KeyError as error:
                        if name == 'collection_deletion_ledger':
                            continue  # Production recovery uses its selected source's module set.
                        raise OperationError('Albums snapshot implementation is unavailable') from error
                    if not member.isfile() or not 0 < member.size < 65536:
                        raise OperationError('Albums snapshot implementation is unavailable')
                    content = archive.extractfile(member).read()
                implementations['checkpoint/' + name + '.py'] = content
        for label, content in implementations.items():
            name = Path(label).stem
            path = Path(temporary) / (name + '.py')
            path.write_bytes(content)
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
        previous = {name: sys.modules.get(name, missing) for name in ('deletion_ledger', 'asset_deletion_ledger', 'collection_deletion_ledger') if name in modules}
        sys.modules.update({name: modules[name] for name in previous})
        try:
            with child.lock():
                snapshot = backup.create(child, binaries)
                sql = [str(binaries / 'mysql'), '--no-defaults', *backup.connection(child),
                       '--database=' + database, '--batch', '--skip-column-names']
                ledger = modules['deletion_ledger'].publish(child, sql, {'MYSQL_PWD': config['MYSQL_PASSWORD']})
                manifest = backup.verify(child, child.backups / snapshot['filename'])
                modules['deletion_ledger'].verify(child, child.backups / ledger['filename'], ledger['sha256'], manifest['created_at'])
                names = [snapshot['filename'], snapshot['filename'] + '.json', ledger['filename']]
                result = {'checkpoint/' + name: private(child.backups / name, 32 * 1024**2) for name in names}
                if environment == 'development':
                    result.update(implementations)
                modules['deletion_ledger'].prune_expired(child, int(time.time()) - backup.RETENTION)
        except ImportError as error:
            raise OperationError('Albums checkpoint implementation has an unavailable dependency') from error
        finally:
            for name, module in previous.items():
                if module is missing:
                    sys.modules.pop(name, None)
                else:
                    sys.modules[name] = module
    # Keep the independent decryption key in its separate local escrow, outside this bundle.
    result['backup.env'] = ''.join(k + '=' + v + '\n' for k, v in config.items() if k != 'ALBUM_BACKUP_ENCRYPTION_KEY').encode()
    return result
