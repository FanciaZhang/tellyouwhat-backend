"""Restore authenticated Albums data only into an owned, network-isolated drill."""
import contextlib
import copy
import importlib.util
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time

from albums_checkpoint import backup_settings, database_scope, mysql_container
from ops_common import OperationError, read_environment
from ops_backup import file_digest, restore_workspace


@contextlib.contextmanager
def _implementation():
    # Use the installed recovery tool, never execute code supplied by a backup.
    names = ('backup', 'deletion_ledger', 'asset_deletion_ledger', 'collection_deletion_ledger')
    missing = object()
    previous = {name: sys.modules.get(name, missing) for name in names}
    search_path = sys.path[:]
    try:
        modules = {}
        for name in names:
            path = Path(__file__).resolve().parent.parent / 'albums' / (name + '.py')
            info = path.lstat()
            if (path.is_symlink() or not path.is_file() or info.st_uid != os.getuid()
                    or info.st_mode & 0o022 or not 0 < info.st_size < 65536):
                raise OperationError('unsafe installed Albums recovery implementation')
            spec = importlib.util.spec_from_file_location('albums_recovery_' + name, path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            modules[name] = module
        sys.modules.update(modules)
        yield modules
    finally:
        for name, module in previous.items():
            if module is missing:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = module
        sys.path[:] = search_path


def _source_connection(runtime, backup, environment):
    return (['docker', 'exec', '--env', 'MYSQL_PWD', mysql_container(environment), 'mysql',
             '--no-defaults', *backup.connection(runtime), '--database=' + runtime.config['MYSQL_DATABASE'],
             '--batch', '--skip-column-names'], {'MYSQL_PWD': runtime.config['MYSQL_PASSWORD']})


def recheck_source_checkpoint(runtime, environment, pin):
    _, config = backup_settings(runtime, environment)
    child = copy.copy(runtime)
    child.config = {**config, 'BACKUP_ENCRYPTION_KEY': config['ALBUM_BACKUP_ENCRYPTION_KEY']}
    with _implementation() as modules:
        ledger = modules['deletion_ledger']
        checkpoint = ledger.verify_checkpoint(child, *pin, 0)
        sql, credentials = _source_connection(child, modules['backup'], environment)
        ledger.require_current(child, sql, credentials, checkpoint)
    return int(time.time())


def verify_runtime_recovery(runtime, archive, manifest, *, environment, image, backup_filename,
                            independent_checkpoint=None, verify_checkpoint_source=False):
    from albums_artifacts import verify_recovery
    if verify_checkpoint_source and independent_checkpoint is None:
        raise OperationError('source verification requires an independent Albums checkpoint')
    verify_recovery(archive, manifest, environment)
    database, user = database_scope(environment)
    _, config = backup_settings(runtime, environment)
    snapshots = [name for name in manifest if re.fullmatch(r'checkpoint/albums-[a-f0-9]{32}\.sql\.gz\.enc', name)]
    ledgers = [name for name in manifest if re.fullmatch(r'checkpoint/albums-deletions-[a-f0-9]{32}\.json', name)]
    if len(snapshots) != 1 or len(ledgers) != 1 or not isinstance(image, str) or not image.strip():
        raise OperationError('incomplete Albums runtime restore selection')
    snapshot, ledger = Path(snapshots[0]).name, Path(ledgers[0]).name
    container = 'tellyouwhat-restore-' + secrets.token_hex(8)
    source_checks = {}
    with restore_workspace(runtime, 'albums-sql', container, snapshot=snapshot, ledger=ledger) as folder:
        prefix = 'recovery/albums-' + environment + '/'
        selected = (snapshots[0], snapshots[0] + '.json', ledgers[0], 'backup.env')
        for name in selected:
            member = archive.getmember(prefix + name)
            if not member.isfile() or not 0 < member.size <= (8192 if name == 'backup.env' else 32 * 1024**2):
                raise OperationError('invalid Albums runtime restore input')
            with archive.extractfile(member) as source, (folder / Path(name).name).open('xb') as output:
                os.chmod(output.name, 0o600)
                while block := source.read(1024 * 1024):
                    output.write(block)
        saved = read_environment(folder / 'backup.env')
        expected = {'MYSQL_HOST': 'localhost', 'MYSQL_PORT': '3306', 'MYSQL_DATABASE': database, 'MYSQL_USER': user}
        if (set(saved) != set(expected) | {'MYSQL_PASSWORD'}
                or any(saved.get(k) != v for k, v in expected.items())
                or not re.fullmatch('[a-f0-9]{64}', saved.get('MYSQL_PASSWORD', ''))):
            raise OperationError('Albums recovered database settings have the wrong scope')
        child = copy.copy(runtime)
        child.config = config.copy()
        child.backups = folder
        with _implementation() as modules:
            backup = modules['backup']
            deletion_ledger = modules['deletion_ledger']
            backup.configure(child)
            verified = backup.verify(child, folder / snapshot)
            if verified['created_at'] <= time.time() - backup.RETENTION:
                raise OperationError('Albums runtime snapshot is outside retention')
            checkpoint = deletion_ledger.verify_checkpoint(
                child, folder / ledger, file_digest(folder / ledger), verified['created_at'])
            if independent_checkpoint is not None:
                if not isinstance(independent_checkpoint, (list, tuple)) or len(independent_checkpoint) != 2:
                    raise OperationError('independent deletion checkpoint requires a path and pinned digest')
                selected = deletion_ledger.verify_checkpoint(
                    child, *independent_checkpoint, checkpoint['exported_at'])
                checkpoint = deletion_ledger.merge_recovery_checkpoints(checkpoint, selected)
            if verify_checkpoint_source:
                source_sql, source_credentials = _source_connection(child, backup, environment)
                deletion_ledger.require_current(child, source_sql, source_credentials, selected)
                source_checks['checkpoint_source_prechecked_at'] = int(time.time())
            raw = backup.prepare_restore_sql(child, folder / snapshot, folder)
            password = secrets.token_hex(32)
            client = folder / 'client.cnf'
            client.touch(mode=0o600, exist_ok=False)
            client.write_text('[client]\nuser=root\npassword=' + password + '\nprotocol=socket\n')
            runtime.execute('albums-runtime-restore-start', [
                'docker', 'run', '--detach', '--rm', '--name', container,
                '--label', 'cn.tellyouwhat.operation=restore-drill',
                '--label', 'cn.tellyouwhat.restore-workspace=' + folder.name,
                '--network', 'none', '--memory', '768m', '--pids-limit', '256',
                '--tmpfs', '/var/lib/mysql:rw,noexec,nosuid,size=768m',
                '--mount', 'type=bind,source=' + str(client) + ',target=/run/albums-client.cnf,readonly',
                '--env', 'MYSQL_ROOT_PASSWORD', image, '--innodb-buffer-pool-size=128M', '--performance-schema=OFF'],
                env={'MYSQL_ROOT_PASSWORD': password}, timeout=180)
            connection = ['docker', 'exec', '--interactive', container]
            sql = connection + ['mysql', '--defaults-extra-file=/run/albums-client.cnf', '--batch', '--skip-column-names']
            for _ in range(90):
                ready = subprocess.run(sql + ['--execute=SELECT 1'], capture_output=True, timeout=10,
                                       pass_fds=getattr(runtime, '_child_pass_fds', ()))
                if ready.returncode == 0:
                    break
                time.sleep(1)
            else:
                raise OperationError('isolated Albums runtime database did not become ready')
            diagnostic = folder / 'restore.stderr'
            diagnostic.touch(mode=0o600, exist_ok=False)
            with diagnostic.open('wb') as output:
                backup.restore_database(child, sql, connection + ['mysqldump', '--defaults-extra-file=/run/albums-client.cnf'],
                                        verified, raw, folder, output, checkpoint)
            if verify_checkpoint_source:
                deletion_ledger.require_current(child, source_sql, source_credentials, selected)
                source_checks['checkpoint_source_rechecked_at'] = int(time.time())
                source_checks['checkpoint_source_point_in_time_only'] = True
    return runtime.record('albums-' + environment + '-restore', backup_filename=backup_filename,
                          snapshot=snapshot, tables=len(verified['table_rows']),
                          deletion_intents=len(checkpoint['entries']), asset_deletion_intents=len(checkpoint['assets']),
                          collection_deletion_intents=len(checkpoint['collections']), quarantined=True,
                          independent_checkpoint_applied=independent_checkpoint is not None,
                          independent_checkpoint_sha256=independent_checkpoint[1] if independent_checkpoint is not None else None,
                          latest_checkpoint_proven=False, recovery_ready=False, **source_checks)
