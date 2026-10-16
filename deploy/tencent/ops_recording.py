"""Keep temporary recording jobs outside release slots and configuration backups."""

import os
from pathlib import Path
import stat

from ops_common import OperationError

BASE = Path('/opt/tellyouwhat/journal-recordings')
UID = 65532


def configured_root(config, environment):
    if environment not in ('production', 'development'):
        raise OperationError('invalid recording environment')
    enabled = config.get('JOURNAL_RECORDING_ENABLED', '')
    value = config.get('JOURNAL_RECORDING_ROOT', '')
    if enabled not in ('', 'true', 'false') or (enabled == 'true' and not value):
        raise OperationError('invalid recording storage configuration')
    if not value:
        return None
    if value != str(BASE / environment) or config.get('APP_ENV', 'production') != environment:
        raise OperationError('recording storage must use its fixed environment directory')
    return Path(value)


def require_continuity(before, after):
    previous = before.get('JOURNAL_RECORDING_ROOT', '')
    if previous and after.get('JOURNAL_RECORDING_ROOT') != previous:
        raise OperationError('recording storage must remain mounted for retention and account deletion')
    if after.get('JOURNAL_RECORDING_ENABLED') == 'true' and not previous:
        raise OperationError('deploy disabled recording storage before enabling it to retain a compatible rollback')


def check_directory(path, owner, mode):
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != owner or stat.S_IMODE(info.st_mode) != mode:
        raise OperationError('recording directory ownership or permissions are invalid')


def prepare_directory(environment):
    # Called under the deployment lock as root; never chmod/chown an existing store.
    if environment not in ('production', 'development'):
        raise OperationError('invalid recording environment')
    for parent in reversed(BASE.parents):
        info = parent.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o022:
            raise OperationError('recording storage ancestors must be real, non-publicly-writable directories')
    try:
        BASE.mkdir(mode=0o711)
        BASE.chmod(0o711)
    except FileExistsError:
        pass
    check_directory(BASE, 0, 0o711)
    path = BASE / environment
    try:
        path.mkdir(mode=0o700)
    except FileExistsError:
        check_directory(path, UID, 0o700)
    else:
        os.chown(path, UID, UID)
    # The service validates/initializes .environment under its existing file lock.


def prepare(runtime, config, environment='production'):
    path = configured_root(config, environment)
    if path is not None:
        runtime.execute('recording-storage-prepare', ['sudo', '-n', 'python3', str(Path(__file__).resolve()), environment])


def compose_overlay(root, config, environment='production'):
    path = configured_root(config, environment)
    if path is None:
        return []
    overlay = Path(root) / 'compose.journal-recording.yaml'
    if overlay.is_symlink() or not overlay.is_file():
        raise OperationError('recording storage Compose overlay is missing')
    # Missing mounts must fail before Docker can create a root-owned empty directory.
    for parent in reversed(path.parents):
        if parent.is_symlink():
            raise OperationError('recording storage cannot traverse a symlink')
    try:
        check_directory(BASE, 0, 0o711)
        check_directory(path, UID, 0o700)
    except OSError as error:
        raise OperationError('recording storage must be provisioned before starting services') from error
    return ['-f', str(overlay)]


if __name__ == '__main__':
    import sys
    if len(sys.argv) != 2 or os.geteuid() != 0:
        raise SystemExit('recording storage preparation requires the privileged deployment step')
    prepare_directory(sys.argv[1])
