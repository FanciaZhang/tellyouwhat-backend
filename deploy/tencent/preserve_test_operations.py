"""Keep registered test recovery tools when preparing a production release."""

import argparse
import hashlib
import io
import json
from pathlib import Path
import re
import shutil
import tarfile

from ops_common import OperationError, Runtime, atomic_json
from ops_artifacts import require_operator
from platform_test_deploy import private_file

FILES = ('platform_test_deploy.py', 'platform_test_journal.py', 'platform_test_albums.py',
         'platform_test_schema.py', 'platform_test_artifacts.py', 'platform_test_admin.py', 'test_routes.py')


def preserve(runtime, bundle):
    bundle = Path(bundle).resolve()
    if bundle.parent != runtime.root or not re.fullmatch(r'\.incoming-[a-f0-9]{40}', bundle.name):
        raise OperationError('test operations require the selected production bundle')
    state_path = runtime.state / 'platform-test.json'
    if not state_path.exists():
        return {}
    state = json.loads(private_file(runtime, state_path, 16384))
    source = state.get('source', '')
    if not re.fullmatch('[a-f0-9]{40}', source):
        raise OperationError('registered test source is invalid')
    folder = runtime.root / '.platform-test/releases' / source
    if state.get('release') != str(folder):
        raise OperationError('registered test release differs from its source')
    for path in (folder.parent.parent, folder.parent, folder):
        if (path.is_symlink() or not path.is_dir() or path.stat().st_uid != runtime.root.stat().st_uid
                or path.stat().st_mode & 0o077):
            raise OperationError('registered test release must remain private and operator owned')
    owner = json.loads(private_file(runtime, runtime.state / 'artifact-owners/platform-test.json', 16384))['current']
    if owner.get('source') != source or str(folder) not in owner.get('paths', []):
        raise OperationError('test operations source is not registered')
    record = json.loads(private_file(runtime, folder / 'selection.json', 4096))
    data = private_file(runtime, folder / 'source.tar', 64 * 1024**2)
    if record.get('source') != source or hashlib.sha256(data).hexdigest() != record.get('files', {}).get('source.tar'):
        raise OperationError('registered test source archive changed')
    installed, staged = runtime.root / 'deploy/tencent', bundle / 'deploy/tencent'
    # Shared backup policy must be integrated, rather than replaced by a test toolchain.
    if (installed / 'ops_backup.py').read_bytes() != (staged / 'ops_backup.py').read_bytes():
        raise OperationError('production and registered test backup policies differ')
    hashes = {}
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        for name in FILES:
            path = installed / name
            target = staged / name
            if (path.is_symlink() or not path.is_file() or path.stat().st_uid != runtime.root.stat().st_uid
                    or path.stat().st_mode & 0o022 or target.is_symlink()):
                raise OperationError('test operations must be operator owned regular files')
            member = archive.getmember('deploy/tencent/' + name)
            if (not member.isfile() or member.size > 131072
                    or archive.extractfile(member).read() != path.read_bytes()):
                raise OperationError('installed test operations differ from their registered source')
            hashes[name] = hashlib.sha256(path.read_bytes()).hexdigest()
    for name in FILES:
        shutil.copy2(installed / name, staged / name)
    result = {'source': source, 'files': hashes}
    atomic_json(bundle / 'test-operations.json', result)
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', required=True)
    parser.add_argument('--bundle', required=True)
    args = parser.parse_args()
    runtime = Runtime(args.backend_dir)
    require_operator(runtime.root)
    with runtime.lock(wait_seconds=600), runtime.lock('deployment', wait_seconds=600):
        print(json.dumps(preserve(runtime, args.bundle), sort_keys=True))
