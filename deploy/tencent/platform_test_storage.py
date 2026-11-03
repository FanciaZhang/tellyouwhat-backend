"""The reviewed test topology adds only persistent recording storage and maintenance."""

import hashlib

from ops_common import OperationError, read_environment
import ops_recording

PREVIOUS_COMPOSE = '3dbbc4d83c3222dd5d5d63d039a82f4e5a7de63d2ab1f517b26f63332515773b'
RECORDING_COMPOSE = '5c25c9b1d9826ff0584fedc1045db93d0770cc7cf472c45dd7467ca5fb37389e'
ROOT = '/opt/tellyouwhat/journal-recordings/development'


def digest(folder):
    return hashlib.sha256((folder / 'compose.platform-test.yaml').read_bytes()).hexdigest()


def available(folder):
    return digest(folder) == RECORDING_COMPOSE


def require_transition(before, after):
    old, new = digest(before), digest(after)
    if old != new and (old, new) != (PREVIOUS_COMPOSE, RECORDING_COMPOSE):
        raise OperationError('unreviewed platform test topology change')
    config = read_environment(after / 'runtime.env')
    previous = read_environment(before / 'runtime.env')
    if config.get('JOURNAL_RECORDING_ENABLED') == 'true' and (
            old != RECORDING_COMPOSE or 'journal' not in previous.get('PLATFORM_APPS', '').split(',')):
        raise OperationError('deploy Journal with mounted storage and recordings disabled before enabling recordings')


def prepare(runtime, folder):
    if available(folder):
        ops_recording.prepare(runtime, {'APP_ENV': 'development', 'JOURNAL_RECORDING_ROOT': ROOT}, 'development')


def verify_gateway(container):
    mounts = [m for m in container.get('Mounts', []) if m.get('Destination') == ROOT]
    settings = container.get('Config', {})
    env = settings.get('Env', [])
    if (len(mounts) != 1 or mounts[0].get('Source') != ROOT or mounts[0].get('Type') != 'bind'
            or mounts[0].get('RW') is not True or settings.get('User') != '65532:65532'
            or 'APP_ENV=development' not in env or 'JOURNAL_RECORDING_ROOT=' + ROOT not in env):
        raise OperationError('shared test gateway recording mount or identity is invalid')
