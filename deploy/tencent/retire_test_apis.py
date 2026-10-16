"""Stop registered standalone APIs after all four test hosts use the shared gateway."""

import argparse
import fcntl
import json
import os
from pathlib import Path
import re
import sys

import blue_green
import platform_test_deploy as platform
import test_routes
from ops_artifacts import development_deploy, require_operator
from ops_backup import create_backup, verify_backup
from ops_common import OperationError, Runtime
from ops_offsite import verify_recovery_bundle


TARGETS = {
    'journal-private-development': 'journal-development',
    'birthday-private-development': 'birthday-development',
    'birthday-private-testflight': 'birthday-development',
    'tellyouwhat-albums-development-albumgateway-1': 'albums-test',
}
OLD_PORTS = {'18787', '18788', '18789', '18092'}
API_BINDINGS = {'birthday-private-development': '18788', 'birthday-private-testflight': '18789',
                'tellyouwhat-albums-development-albumgateway-1': '18092'}


def read_state(runtime, name):
    return json.loads(platform.private_file(runtime, runtime.state / name, 16384))


def require_shared(runtime, source):
    for name in ('retire_test_apis.py', 'journal_development_artifacts.py'):
        installed = runtime.root / 'deploy/tencent' / name
        if (installed.is_symlink() or not installed.is_file()
                or installed.stat().st_uid != runtime.root.stat().st_uid or installed.stat().st_mode & 0o022
                or installed.read_bytes() != Path(__file__).with_name(name).read_bytes()):
            raise OperationError('install matched retirement and scheduled backup scripts through the production release')
    state = read_state(runtime, 'platform-test.json')
    owner = read_state(runtime, 'artifact-owners/platform-test.json')['current']
    folder, _, _ = platform.selection(runtime, source)
    if (state.get('source') != source or owner.get('source') != source
            or str(folder) not in owner.get('paths', [])
            or set(state.get('apps', [])) != set(test_routes.ROUTES)):
        raise OperationError('retirement requires the registered four-App shared test selection')
    platform.verify_running(runtime, folder, source, 'retirement-shared')
    platform.albums.require_resources(runtime, folder)
    stable = read_state(runtime, 'blue-green.json')
    if stable.get('phase') != 'STABLE':
        raise OperationError('wait for a stable production release')
    blue_green.assert_route(stable['current'])
    config = blue_green.proxy_config()
    hosts = blue_green.proxy_hosts(config)
    if (any(hosts.get(host) != {'127.0.0.1:18790'} for host, _ in test_routes.ROUTES.values())
            or any(dial.rsplit(':', 1)[-1] in OLD_PORTS for dial in blue_green.proxy_dials(config))):
        raise OperationError('a live proxy route still depends on a standalone test API')
    for app in test_routes.ROUTES:
        test_routes.ready(runtime, app)
    return owner


def containers(runtime):
    approved = {}
    for owner in set(TARGETS.values()):
        record = read_state(runtime, 'artifact-owners/' + owner + '.json')
        refs = sorted({ref for label in ('current', 'previous')
                       for ref in (record.get(label) or {}).get('images', [])})
        if record.get('owner') != owner or not refs:
            raise OperationError('standalone API has no registered recovery images')
        images = json.loads(runtime.execute('retirement-owned-images', ['docker', 'image', 'inspect', *refs]))
        approved[owner] = {image['Id'] for image in images}
    values = json.loads(runtime.execute('retirement-containers', ['docker', 'inspect', *TARGETS]))
    if len(values) != len(TARGETS) or {c['Name'].lstrip('/') for c in values} != set(TARGETS):
        raise OperationError('standalone API inventory differs from the reviewed targets')
    for container in values:
        name = container['Name'].lstrip('/')
        owner = TARGETS[name]
        if (not re.fullmatch('[a-f0-9]{64}', container.get('Id', ''))
                or container.get('Image') not in approved[owner]
                or container['State'].get('Status') not in ('running', 'exited')
                or container['State'].get('Paused') or container['State'].get('Restarting')):
            raise OperationError('standalone container differs from its registered API image')
        if name in API_BINDINGS and container.get('HostConfig', {}).get('PortBindings') != {
                '8080/tcp': [{'HostIp': '127.0.0.1', 'HostPort': API_BINDINGS[name]}]}:
            raise OperationError('standalone API no longer has its reviewed loopback binding')
        if name == 'tellyouwhat-albums-development-albumgateway-1':
            labels = container.get('Config', {}).get('Labels', {})
            if (labels.get('com.docker.compose.project') != 'tellyouwhat-albums-development'
                    or labels.get('com.docker.compose.service') != 'albumgateway'):
                raise OperationError('Albums retirement target is not its registered API service')
    return values


def checkpoint(runtime):
    result = create_backup(runtime)
    path = runtime.backups / result['filename']
    manifest = verify_backup(runtime, path)
    if any(not manifest.get('recovery_' + part) for part in (
            'platform_test', 'journal_development', 'albums_development', 'test_routing')):
        raise OperationError('retirement requires complete authenticated recovery selections')
    verify_recovery_bundle(runtime, path, manifest)
    return result['filename']


def retire(runtime, source):
    require_operator(runtime.root)
    for name in ('operations', 'deployment'):
        with (runtime.state / (name + '.lock')).open('r') as handle:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                continue
            raise OperationError('retirement must run through the registered producer with both locks')
    require_shared(runtime, source)
    before = containers(runtime)
    backup = checkpoint(runtime)
    require_shared(runtime, source)
    if [(c['Id'], c['Image'], c['State']['Running']) for c in containers(runtime)] != [
            (c['Id'], c['Image'], c['State']['Running']) for c in before]:
        raise OperationError('standalone API inventory changed during backup')
    running = [c['Id'] for c in before if c['State']['Running']]
    try:
        if running:
            runtime.execute('retirement-stop', ['docker', 'stop', '--time', '30', *running], timeout=180)
        after = containers(runtime)
        if ({c['Id'] for c in after} != {c['Id'] for c in before}
                or any(c['State']['Running'] for c in after)):
            raise OperationError('standalone APIs did not stop as selected')
        require_shared(runtime, source)
        retained_backup = checkpoint(runtime)
        return runtime.record('test-api-retirement', passed=True, shared_source=source,
                              stopped=[c['Name'].lstrip('/') for c in before],
                              retained_container_ids=[c['Id'] for c in before],
                              before_backup=backup, after_backup=retained_backup)
    except Exception:
        if running:
            runtime.execute('retirement-restore', ['docker', 'start', *running], timeout=180)
            restored = containers(runtime)
            if not set(running) <= {c['Id'] for c in restored if c['State']['Running']}:
                raise OperationError('standalone API recovery requires operator attention')
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', default='/opt/tellyouwhat/backend')
    parser.add_argument('--source', required=True, help='exact active shared test source')
    parser.add_argument('--retire', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    runtime = Runtime(args.backend_dir)
    if args.retire:
        retire(runtime, args.source)
        return
    owner = require_shared(runtime, args.source)
    result = development_deploy(runtime, 'platform-test', owner['paths'], owner['images'], args.source,
                               'retire-standalone-test-apis',
                               [sys.executable, str(Path(__file__).resolve()), '--backend-dir', str(runtime.root),
                                '--source', args.source, '--retire'],
                               'http://127.0.0.1:18790/readyz', verify_host=platform.HOST)
    print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (OperationError, OSError, ValueError, KeyError):
        print(json.dumps({'passed': False, 'error': 'test API retirement failed; inspect private operations diagnostics'}))
        sys.exit(1)
