"""Reclaim only expired Apple development installation artifacts."""

import json
import hashlib
import os
from pathlib import Path
import re

from ops_common import OperationError

CADDY_ROOT = Path('/etc/caddy/Caddyfile')
CADDY_FILES = {'delivery.caddy': Path('/etc/caddy/apple-delivery.caddy'),
               'logging.caddy': Path('/etc/caddy/apple-delivery-logging.caddy')}
ROOT_PREFIX = '{\n    import /etc/caddy/apple-delivery-logging.caddy\n}\n\n'


def recovery_files(runtime):
    from apple_delivery_deploy import FILES, environment, read_record, selection
    state_file = runtime.state / 'apple-delivery.json'
    owner_file = runtime.state / 'artifact-owners/apple-development.json'
    if state_file.is_symlink():
        raise OperationError('Apple delivery recovery state cannot be a symlink')
    if not state_file.exists():
        if owner_file.exists() or owner_file.is_symlink():
            raise OperationError('Apple delivery recovery state is missing')
        return {}
    state = read_record(runtime, state_file)
    owner = read_record(runtime, owner_file)
    paths = {'state.json': state_file, 'owner.json': owner_file}
    for label, current in [('current', state), ('previous', state.get('previous'))]:
        if current is None and label == 'previous':
            continue
        if not isinstance(current, dict):
            raise OperationError('invalid Apple delivery recovery selection')
        folder, value = selection(runtime, current.get('source'))
        registered = owner.get(label, {}) or {}
        if (current.get('image') != value['image'] or current.get('release') != str(folder)
                or value['image'] not in registered.get('images', []) or str(folder) not in registered.get('paths', [])):
            raise OperationError('Apple delivery configuration is not bound to its owner registration')
        env = folder / '.env'
        if (env.is_symlink() or not env.is_file() or env.stat().st_size > 16384 or env.stat().st_mode & 0o077
                or env.stat().st_uid != runtime.root.stat().st_uid
                or env.read_text() != ''.join(key + '=' + val + '\n' for key, val in environment(runtime, value['source']).items())):
            raise OperationError('Apple delivery environment differs from its recovery selection')
        for name in (*FILES, 'selection.json', '.env'):
            paths[label + '/' + name] = folder / name
    route_file = runtime.state / 'apple-delivery-routing.json'
    if route_file.is_symlink():
        raise OperationError('Apple delivery routing record cannot be a symlink')
    if route_file.exists():
        route = read_record(runtime, route_file)
        root = CADDY_ROOT.read_text()
        if not root.startswith(ROOT_PREFIX) or root.splitlines().count('import ' + str(CADDY_FILES['delivery.caddy'])) != 1:
            raise OperationError('Apple delivery logging import differs from managed configuration')
        paths.update({'Caddyfile': CADDY_ROOT, 'routing.json': route_file, **CADDY_FILES})
        for name, path in CADDY_FILES.items():
            if path.is_symlink() or not path.is_file() or path.stat().st_size > 256 * 1024:
                raise OperationError('Apple delivery proxy recovery file is missing or unsafe')
            if hashlib.sha256(path.read_bytes()).hexdigest() != route.get(name):
                raise OperationError('Apple delivery proxy configuration has changed')
    elif (any(path.exists() for path in CADDY_FILES.values()) or
          (CADDY_ROOT.exists() and '/etc/caddy/apple-delivery' in CADDY_ROOT.read_text())):
        raise OperationError('Apple delivery proxy has no managed recovery state')
    for name, source in paths.items():
        if source.is_symlink() or not source.is_file() or source.stat().st_size > 16 * 1024**2:
            raise OperationError('Apple delivery recovery input is missing, oversized or a symlink')
    return paths


def backup_configuration(runtime, destination):
    paths = recovery_files(runtime)
    if not paths:
        return {}
    destination.mkdir(mode=0o700)
    hashes = {}
    for name, source in paths.items():
        data = source.read_bytes()
        target = destination / name
        target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        target.write_bytes(data)
        target.chmod(0o600)
        hashes[name] = hashlib.sha256(data).hexdigest()
    return hashes


def verify_recovery(archive, hashes):
    from apple_delivery_deploy import FILES
    current = {'current/' + name for name in (*FILES, 'selection.json', '.env')}
    previous = {'previous/' + name for name in (*FILES, 'selection.json', '.env')}
    proxy = {'Caddyfile', 'routing.json', *CADDY_FILES}
    required = {'state.json', 'owner.json', *current}
    names = set(hashes)
    if (not required <= names or names - required - previous - proxy
            or (names & previous and not previous <= names) or (names & proxy and not proxy <= names)):
        raise OperationError('Apple delivery recovery selection is incomplete')
    for name, digest in hashes.items():
        if not isinstance(digest, str) or not re.fullmatch(r'[0-9a-f]{64}', digest):
            raise OperationError('invalid Apple delivery recovery digest')
        try:
            member = archive.getmember('recovery/apple-delivery/' + name)
        except KeyError as error:
            raise OperationError('Apple delivery recovery file is missing') from error
        if (not member.isfile() or not 0 < member.size <= 16 * 1024**2
                or hashlib.sha256(archive.extractfile(member).read()).hexdigest() != digest):
            raise OperationError('Apple delivery recovery differs from the authenticated manifest')


def cleanup_packages(runtime):
    state_file = runtime.state / "apple-delivery.json"
    owner_file = runtime.state / "artifact-owners/apple-development.json"
    if not state_file.exists():
        if owner_file.exists():
            raise OperationError("Apple delivery recovery state is missing")
        return {"configured": False}
    if state_file.is_symlink() or state_file.stat().st_size > 16 * 1024:
        raise OperationError("invalid Apple delivery recovery state")
    state = json.loads(state_file.read_text())
    if not isinstance(state, dict):
        raise OperationError("invalid Apple delivery recovery state")
    image = state.get("image", "")
    if not re.fullmatch(r"tellyouwhat-apple-development:[0-9a-f]{40}", image):
        raise OperationError("Apple delivery image is outside its owner scope")
    if owner_file.is_symlink() or not owner_file.is_file() or owner_file.stat().st_size > 16 * 1024:
        raise OperationError("Apple delivery owner registration is missing")
    owner = json.loads(owner_file.read_text())
    if not isinstance(owner, dict) or not isinstance(owner.get("current"), dict) or image not in owner["current"].get("images", []):
        raise OperationError("Apple delivery cleanup is not bound to the current registration")
    data = runtime.root / ".apple-delivery/data"
    operator = runtime.root.stat()
    if ((runtime.root / ".apple-delivery").is_symlink() or data.is_symlink() or not data.is_dir()
            or not data.resolve().is_relative_to(runtime.root) or data.stat().st_uid != operator.st_uid
            or data.stat().st_mode & 0o077 or os.geteuid() != operator.st_uid):
        raise OperationError("Apple delivery store must be private and operator owned")
    output = runtime.execute("apple-delivery-cleanup", [
        "docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges:true", "--memory", "64m", "--pids-limit", "64",
        "--user", f"{operator.st_uid}:{operator.st_gid}",
        "--mount", f"type=bind,source={data},target=/data", image, "cleanup", "--data", "/data"
    ], timeout=120)
    result = json.loads(output)
    if (not isinstance(result, dict) or set(result) != {"grants_removed", "packages_removed"}
            or any(type(n) is not int or n < 0 for n in result.values())):
        raise OperationError("invalid Apple delivery cleanup result")
    return {"configured": True, **result}
