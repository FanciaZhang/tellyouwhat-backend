"""Add ready test APIs to native Caddy without editing production routes."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import urllib.request
import uuid

from ops_common import OperationError, Runtime, atomic_text
from ops_artifacts import require_operator
from blue_green import assert_route

ROOT = Path('/etc/caddy/Caddyfile')
FRAGMENT = Path('/etc/caddy/tellyouwhat-test.caddy')
ROUTES = {'health': ('api-test.health.tellyouwhat.cn', 18790),
          'journal': ('api-test.journal.tellyouwhat.cn', 18790),
          'birthday': ('api-test.birthday.tellyouwhat.cn', 18790),
          'albums': ('api-test.albums.tellyouwhat.cn', 18790)}


def development_environment(runtime, app):
    container = json.loads(runtime.execute('test-route-identity',
        ['docker', 'inspect', 'tellyouwhat-platform-test-gateway-1']))[0]
    if not container.get('State', {}).get('Running') or container['State'].get('OOMKilled'):
        raise OperationError('shared test gateway is not running')
    env = dict(item.split('=', 1) for item in container['Config']['Env'] if '=' in item)
    if env.get('APP_ENV') != 'development' or app not in env.get('PLATFORM_APPS', '').split(','):
        raise OperationError('App is not enabled in the shared test gateway')
    prefix = 'ALBUM' if app == 'albums' else app.upper()
    bundle = {'health': 'healthapp', 'journal': 'journalapp', 'birthday': 'birthday', 'albums': 'albums'}[app]
    host_key = prefix + ('_API_HOST' if app == 'albums' else '_API_HOSTS')
    if (env.get(prefix + '_APPLE_BUNDLE_ID') != 'cn.tellyouwhat.' + bundle + '.dev'
            or env.get(host_key) != ROUTES[app][0]
            or (app == 'albums' and env.get('ALBUM_DEPLOYMENT_ENVIRONMENT') != 'test')):
        raise OperationError('shared test App is not bound to its development identity')
    return env


def render(apps):
    if not apps or not set(apps) <= ROUTES.keys():
        raise OperationError('select known test applications')
    return '\n'.join(f'''{ROUTES[app][0]} {{
    header {{
        Strict-Transport-Security "max-age=31536000"
        X-Content-Type-Options "nosniff"
        Referrer-Policy "no-referrer"
        -Server
    }}
    @internal path /internal/*
    respond @internal 404
    reverse_proxy 127.0.0.1:{ROUTES[app][1]} {{
        stream_close_delay 4h
        header_up X-Real-IP {{remote_host}}
        flush_interval -1
    }}
}}
''' for app in sorted(set(apps)))


class RejectRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def ready(runtime, app):
    development_environment(runtime, app)
    host, port = ROUTES[app]
    request = urllib.request.Request(f'http://127.0.0.1:{port}/readyz', headers={'Host': host})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), RejectRedirects)
    try:
        with opener.open(request, timeout=5) as response:
            if response.geturl() != request.full_url or response.status != 200:
                raise OperationError('shared test service readiness failed')
            body = json.load(response)
            if not isinstance(body, dict) or (body.get('code') != 'ok' if app == 'albums' else body.get('status') != 'ready'):
                raise OperationError('shared test service readiness failed')
    except (OSError, ValueError) as error:
        raise OperationError('shared test service readiness failed') from error


def install(runtime, source, destination):
    pending = destination.parent / ('.' + destination.name + '.pending')
    try:
        runtime.execute('test-route-stage', ['sudo', '-n', 'install', '-m', '644', str(source), str(pending)])
        runtime.execute('test-route-commit', ['sudo', '-n', 'mv', '--', str(pending), str(destination)])
    except Exception:
        if pending.exists() or pending.is_symlink():
            runtime.execute('test-route-discard-pending', ['sudo', '-n', 'unlink', str(pending)])
        raise


def backup_configuration(runtime, destination):
    state = runtime.state / 'test-routing.json'
    if not state.exists():
        if FRAGMENT.exists():
            raise OperationError('test routing exists without managed recovery state')
        return {}
    paths = {'Caddyfile': ROOT, 'test.caddy': FRAGMENT, 'state.json': state}
    if any(p.is_symlink() or not p.is_file() or p.stat().st_size > 256 * 1024 for p in paths.values()):
        raise OperationError('test routing recovery file is missing, oversized or a symlink')
    record = json.loads(state.read_text())
    content = FRAGMENT.read_text()
    render(record['apps'])  # Validate ownership; older pinned routes remain recoverable during migration.
    if (hashlib.sha256(content.encode()).hexdigest() != record['fragment_sha256']
            or ROOT.read_text().splitlines().count('import ' + str(FRAGMENT)) != 1):
        raise OperationError('test routing differs from its managed recovery state')
    destination.mkdir(mode=0o700)
    hashes = {}
    for name, source in paths.items():
        data = source.read_bytes()
        (destination / name).write_bytes(data)
        hashes[name] = hashlib.sha256(data).hexdigest()
    return hashes


def configure(runtime, enabled):
    require_operator(runtime.root)
    with runtime.lock(wait_seconds=600), runtime.lock('deployment', wait_seconds=600):
        runtime.refresh()
        state_path = runtime.state / 'test-routing.json'
        prior = json.loads(state_path.read_text()) if state_path.exists() else {}
        apps = sorted(set(prior.get('apps', [])) | set(enabled))
        text = render(apps)
        stable = json.loads((runtime.state / 'blue-green.json').read_text())
        if stable.get('phase') != 'STABLE':
            raise OperationError('wait for the production release to become stable')
        assert_route(stable['current'])
        for app in apps:
            ready(runtime, app)
        before = ROOT.read_text()
        existed = FRAGMENT.exists()
        previous = FRAGMENT.read_text() if existed else None
        marker = 'import ' + str(FRAGMENT)
        if (before.count(str(FRAGMENT)) not in (0, 1) or
                (str(FRAGMENT) in before and marker not in before.splitlines()) or
                (existed and (not prior or hashlib.sha256(previous.encode()).hexdigest() != prior.get('fragment_sha256'))) or
                (bool(prior) != existed)):
            raise OperationError('test routing differs from its managed state')
        after = before if marker in before.splitlines() else before.rstrip() + '\n\n' + marker + '\n'
        if previous == text and after == before:
            return {'passed': True, 'apps': apps, 'changed': False}
        folder = runtime.state / ('test-routing-' + uuid.uuid4().hex)
        folder.mkdir(mode=0o700)
        atomic_text(folder / 'root.before', before)
        atomic_text(folder / 'fragment.before', previous or '')
        atomic_text(folder / 'fragment.caddy', text)
        atomic_text(folder / 'root.caddy', after)
        # Validate before changing either active file; absolute existing imports stay intact.
        validation = after.replace(marker, 'import ' + str(folder / 'fragment.caddy'))
        atomic_text(folder / 'validation.caddy', validation)
        runtime.execute('test-route-validate', ['sudo', '-n', 'caddy', 'validate', '--config', str(folder / 'validation.caddy'), '--adapter', 'caddyfile'])
        if ROOT.read_text() != before or (FRAGMENT.read_text() if FRAGMENT.exists() else None) != previous:
            raise OperationError('Caddy configuration changed during preparation')
        try:
            install(runtime, folder / 'fragment.caddy', FRAGMENT)
            install(runtime, folder / 'root.caddy', ROOT)
            runtime.execute('test-route-reload', ['sudo', '-n', 'systemctl', 'reload', 'caddy'])
            assert_route(stable['current'])
        except Exception as error:
            install(runtime, folder / 'root.before', ROOT)
            if existed:
                install(runtime, folder / 'fragment.before', FRAGMENT)
            else:
                runtime.execute('test-route-remove', ['sudo', '-n', 'rm', '-f', '--', str(FRAGMENT)])
            try:
                runtime.execute('test-route-restore', ['sudo', '-n', 'systemctl', 'reload', 'caddy'])
            except Exception:
                raise OperationError('test route activation and Caddy recovery failed; operator recovery required') from error
            raise OperationError('test route activation failed; previous Caddy configuration restored') from error
        return runtime.record('test-routing', passed=True, apps=apps, changed=True, recovery=str(folder),
                              production_routes_verified=True,
                              script_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                              fragment_sha256=hashlib.sha256(text.encode()).hexdigest(),
                              root_sha256=hashlib.sha256(after.encode()).hexdigest())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', default='/opt/tellyouwhat/backend')
    parser.add_argument('--enable', nargs='+', choices=ROUTES, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    print(json.dumps(configure(Runtime(args.backend_dir), args.enable), sort_keys=True))


if __name__ == '__main__':
    main()
