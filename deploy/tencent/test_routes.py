"""Add ready test APIs to native Caddy without editing production routes."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import urllib.request
import uuid
import time

from ops_common import OperationError, Runtime, atomic_text
from ops_artifacts import require_operator
from blue_green import assert_route

ROOT = Path('/etc/caddy/Caddyfile')
FRAGMENT = Path('/etc/caddy/tellyouwhat-test.caddy')
ROUTES = {'health': ('api-test.health.tellyouwhat.cn', 18790),
          'journal': ('api-test.journal.tellyouwhat.cn', 18787),
          'birthday': ('api-test.birthday.tellyouwhat.cn', 18790),
          'albums': ('api-test.albums.tellyouwhat.cn', 18092)}


def development_environment(runtime, app):
    name = ('journal-private-development' if app == 'journal' else
            'tellyouwhat-albums-development-albumgateway-1' if app == 'albums' else
            'tellyouwhat-platform-test-gateway-1')
    container = json.loads(runtime.execute('test-route-identity', ['docker', 'inspect', name]))[0]
    if not container.get('State', {}).get('Running'):
        raise OperationError('test container is not running')
    env = dict(item.split('=', 1) for item in container['Config']['Env'] if '=' in item)
    if app == 'journal':
        if env.get('DATABASE_DSN') or env.get('REDIS_URL') or env.get('APP_ENV') == 'production':
            raise OperationError('Journal development service contains production runtime configuration')
    elif app == 'albums':
        if (env.get('ALBUM_DEPLOYMENT_ENVIRONMENT') != 'test'
                or env.get('ALBUM_APPLE_BUNDLE_ID') != 'cn.tellyouwhat.albums.dev'
                or env.get('ALBUM_API_HOST') != ROUTES[app][0]):
            raise OperationError('Albums service is not bound to its development identity')
    elif (env.get('APP_ENV') != 'development'
          or env.get(app.upper() + '_APPLE_BUNDLE_ID') != 'cn.tellyouwhat.' + ('healthapp' if app == 'health' else app) + '.dev'
          or env.get(app.upper() + '_API_HOSTS') != ROUTES[app][0]):
        raise OperationError('platform service is not bound to its development identity')
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


def ready(runtime, app, voice_protocol):
    env = development_environment(runtime, app)
    host, port = ROUTES[app]
    headers = {'Host': host}
    path = '/readyz'
    if app == 'journal':
        if not re.fullmatch(r'journal-voice-v[0-9]+', voice_protocol or ''):
            raise OperationError('Journal requires the current client voice protocol')
        token = env.get('JOURNAL_DEVELOPMENT_TOKEN', '')
        if not token:
            raise OperationError('Journal development credential is missing')
        headers.update({'Authorization': 'Bearer ' + token, 'X-Tellyouwhat-Request-ID': str(uuid.uuid4()),
                        'X-Journal-Development-Installation': str(uuid.uuid4()),
                        'X-Journal-Development-Mode': 'forced',
                        'X-Journal-Development-Started-At': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())})
        path = '/v1/ai/quota'
    request = urllib.request.Request(f'http://127.0.0.1:{port}{path}', headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), RejectRedirects)
    try:
        with opener.open(request, timeout=5) as response:
            if response.geturl() != request.full_url or response.status != 200:
                raise OperationError('test service readiness failed')
            if app == 'journal':
                if (response.headers.get('X-Journal-Environment') != 'development'
                        or response.headers.get('X-Journal-Development-Protocol') != 'subscription-v2'
                        or response.headers.get('X-Journal-Voice-Protocol') != voice_protocol):
                    raise OperationError('Journal development contract does not match the client')
            else:
                body = json.load(response)
                if (body.get('code') != 'ok' if app == 'albums' else body.get('status') != 'ready'):
                    raise OperationError('test service readiness failed')
    except (OSError, ValueError) as error:
        raise OperationError('test service readiness failed') from error


def install(runtime, source, destination):
    pending = destination.parent / ('.' + destination.name + '.pending')
    runtime.execute('test-route-stage', ['sudo', '-n', 'install', '-m', '644', str(source), str(pending)])
    runtime.execute('test-route-commit', ['sudo', '-n', 'mv', '--', str(pending), str(destination)])


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
    if (content != render(record['apps']) or hashlib.sha256(content.encode()).hexdigest() != record['fragment_sha256']
            or ROOT.read_text().splitlines().count('import ' + str(FRAGMENT)) != 1):
        raise OperationError('test routing differs from its managed recovery state')
    destination.mkdir(mode=0o700)
    hashes = {}
    for name, source in paths.items():
        data = source.read_bytes()
        (destination / name).write_bytes(data)
        hashes[name] = hashlib.sha256(data).hexdigest()
    return hashes


def configure(runtime, enabled, voice_protocol):
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
            ready(runtime, app, voice_protocol)
        before = ROOT.read_text()
        existed = FRAGMENT.exists()
        previous = FRAGMENT.read_text() if existed else None
        marker = 'import ' + str(FRAGMENT)
        if (before.count(str(FRAGMENT)) not in (0, 1) or
                (str(FRAGMENT) in before and marker not in before.splitlines()) or
                (existed and (not prior or previous != render(prior['apps']))) or
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
    parser.add_argument('--journal-voice-protocol')
    args = parser.parse_args()
    os.umask(0o077)
    print(json.dumps(configure(Runtime(args.backend_dir), args.enable, args.journal_voice_protocol), sort_keys=True))


if __name__ == '__main__':
    main()
