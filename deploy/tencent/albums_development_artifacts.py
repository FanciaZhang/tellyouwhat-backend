"""Include the isolated Albums development selection in authenticated recovery."""

import hashlib
import json
import os
from pathlib import Path
import re

from ops_common import OperationError
from albums_development_checkpoint import checkpoint

FILES = ('source.tar', 'compose.albums-test.yaml', 'runtime.env', 'compose.env', 'selection.json')
KEYS = ('apple-sign-in.p8', 'credential-key.base64', 'login-limit-key.base64')


def backup_configuration(runtime, destination):
    state_path = runtime.state / 'albums-development.json'
    if not state_path.exists():
        return {}
    if state_path.is_symlink() or not state_path.is_file() or state_path.stat().st_size > 16384:
        raise OperationError('invalid Albums development recovery state')
    state = json.loads(state_path.read_text())
    source = state.get('source', '')
    folder = runtime.root / '.albums-development/releases' / source
    owner_path = runtime.state / 'artifact-owners/albums-test.json'
    if (not re.fullmatch('[a-f0-9]{40}', source) or state.get('release') != str(folder)
            or any(p.is_symlink() or not p.is_dir() for p in (folder, folder.parent, folder.parent.parent))
            or owner_path.is_symlink() or not owner_path.is_file()):
        raise OperationError('Albums development recovery selection is missing')
    owner = json.loads(owner_path.read_text())
    selected = [owner.get('current', {}), owner.get('previous', {})]
    if not any(s and s.get('source') == source and str(folder) in s.get('paths', []) for s in selected):
        raise OperationError('Albums development selection is not registered')
    record = json.loads((folder / 'selection.json').read_text())
    if record.get('source') != source or set(record.get('files', {})) != set(FILES) - {'selection.json'}:
        raise OperationError('Albums development recovery manifest is incomplete')
    files = {'state.json': state_path, 'owner.json': owner_path,
             **{'current/' + name: folder / name for name in FILES},
             'database.env': runtime.root.parent / 'albums-development/database.env'}
    data = {}
    for name, path in files.items():
        if (path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077
                or path.stat().st_uid != runtime.root.stat().st_uid
                or not 0 < path.stat().st_size <= (64 * 1024**2 if name.endswith('source.tar') else 32768)):
            raise OperationError('unsafe Albums development recovery input')
        data[name] = path.read_bytes()
        short = name.removeprefix('current/')
        if name.startswith('current/') and short != 'selection.json' and hashlib.sha256(data[name]).hexdigest() != record['files'][short]:
            raise OperationError('Albums development recovery differs from selected configuration')
    for name in KEYS:
        path = runtime.root.parent / 'albums-development/secrets' / name
        if (path.parent.is_symlink() or path.is_symlink() or not path.is_file()
                or path.stat().st_uid != 0 or path.stat().st_gid != 65532
                or path.stat().st_mode & 0o777 != 0o440 or not 0 < path.stat().st_size <= 16384):
            raise OperationError('unsafe Albums development recovery credential')
        data['secrets/' + name] = runtime.execute('backup-albums-development-key', ['sudo', '-n', 'cat', '--', str(path)])
    data.update(checkpoint(runtime, folder))
    destination.mkdir(mode=0o700)
    for name, content in data.items():
        path = destination / name
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, 'wb') as output:
            output.write(content)
    return {name: hashlib.sha256(content).hexdigest() for name, content in data.items()}


def verify_recovery(archive, manifest):
    expected = {'state.json', 'owner.json', 'database.env', 'backup.env',
                *('current/' + name for name in FILES), *('secrets/' + name for name in KEYS)}
    extra = set(manifest) - expected
    snapshots = [name for name in extra if re.fullmatch(r'checkpoint/albums-[a-f0-9]{32}\.sql\.gz\.enc', name)]
    ledgers = [name for name in extra if re.fullmatch(r'checkpoint/albums-deletions-[a-f0-9]{32}\.json', name)]
    if (not expected <= set(manifest) or len(extra) != 3 or len(snapshots) != 1 or len(ledgers) != 1
            or snapshots[0] + '.json' not in extra):
        raise OperationError('Albums development recovery files are incomplete')
    for name, digest in manifest.items():
        try:
            member = archive.getmember('recovery/albums-development/' + name)
        except KeyError as error:
            raise OperationError('Albums development recovery file is missing') from error
        if not member.isfile() or hashlib.sha256(archive.extractfile(member).read()).hexdigest() != digest:
            raise OperationError('Albums development recovery content verification failed')
