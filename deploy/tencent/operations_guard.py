"""Reject a production bundle that would downgrade independently registered operations."""

import argparse
import hashlib
import json
from pathlib import Path
import tarfile

from ops_common import OperationError, Runtime
from platform_test_deploy import private_file, selection


SHARED = ('operations.py', 'ops_common.py', 'ops_backup.py', 'ops_offsite.py', 'ops_cos.py')


def verify(runtime, bundle):
    state = runtime.state / 'platform-test.json'
    if not state.exists():
        return {'passed': True, 'registered_test_operations': False}
    if state.is_symlink() or not state.is_file():
        raise OperationError('registered operations state must be a regular file')
    current = json.loads(private_file(runtime, state, 16384))
    folder, _, _ = selection(runtime, current.get('source', ''))
    hashes = {}
    with tarfile.open(folder / 'source.tar') as archive:
        for name in SHARED:
            member = archive.getmember('deploy/tencent/' + name)
            if not member.isfile() or not 0 < member.size <= 131072:
                raise OperationError('registered shared operations source is invalid')
            expected = archive.extractfile(member).read()
            for root in (runtime.root, Path(bundle)):
                path = root / 'deploy/tencent' / name
                installed = root == runtime.root
                info = path.stat() if path.is_file() and not path.is_symlink() else None
                if (path.is_symlink() or not path.is_file()
                        or info.st_uid != runtime.root.stat().st_uid
                        or info.st_mode & (0o002 if installed else 0o022)
                        or (installed and info.st_mode & 0o020 and info.st_gid != runtime.root.stat().st_gid)
                        or path.read_bytes() != expected):
                    raise OperationError('shared operations differ from the registered recovery source: ' + name)
            hashes[name] = hashlib.sha256(expected).hexdigest()
    return {'passed': True, 'registered_test_operations': True,
            'source': current['source'], 'shared_sha256': hashes}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', default='/opt/tellyouwhat/backend')
    parser.add_argument('--bundle', required=True)
    args = parser.parse_args()
    try:
        print(json.dumps(verify(Runtime(args.backend_dir), args.bundle)))
    except (OperationError, OSError, ValueError, KeyError, tarfile.TarError):
        raise SystemExit('Production operations selection is unsafe; no runtime files were replaced.')
