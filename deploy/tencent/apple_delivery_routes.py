"""Install the private OTA HTTPS route without exposing capability URLs to proxy logs."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import urllib.request

import apple_delivery_artifacts as artifacts
from apple_delivery_deploy import ORIGIN, read_record, ready
from blue_green import assert_route
from ops_artifacts import development_deploy, require_operator
from ops_common import OperationError, Runtime, atomic_json, atomic_text
from test_routes import RejectRedirects, install


def assert_logging(configuration):
    logs = configuration.get('logging', {}).get('logs', {})
    # This host currently has no other named loggers or global logging overrides.
    if (set(logs) != {'default', 'apple_delivery'} or set(logs['default']) != {'exclude'}
            or set(logs['default']['exclude']) != {'http.log.access.apple_delivery', 'http.log.error.apple_delivery'}
            or logs['apple_delivery'] != {'writer': {'output': 'discard'}, 'include': ['http.log.access.apple_delivery']}):
        raise OperationError('OTA requires its isolated access/error log exclusion and unchanged default logging')
    host = ORIGIN.removeprefix('https://')
    servers = configuration.get('apps', {}).get('http', {}).get('servers', {})
    matched = []
    for server in servers.values():
        if server.get('trace'):
            raise OperationError('HTTP tracing may expose installation capabilities')
        settings = server.get('logs', {})
        names = settings.get('logger_names', {}).get(host)
        if names is not None:
            if names != ['apple_delivery'] or host in settings.get('skip_hosts', []):
                raise OperationError('OTA host must use its dedicated request logger')
            matched.append(server)
    if len(matched) != 1:
        raise OperationError('OTA host logger mapping is missing or ambiguous')
    site = [route for route in matched[0].get('routes', []) if route.get('match') == [{'host': [host]}]]
    if len(site) != 1:
        raise OperationError('OTA requires a dedicated host route')
    handlers = site[0].get('handle', [])
    pipeline = handlers[0].get('routes', []) if len(handlers) == 1 and handlers[0].get('handler') == 'subroute' else []
    if (not pipeline or set(pipeline[0]) != {'handle'} or not pipeline[0]['handle']
            or pipeline[0]['handle'][0] != {'handler': 'vars', 'access_logger_names': ['apple_delivery']}):
        raise OperationError('OTA must bind its logger before all request handlers, including mixed-case hosts')


def read_caddy(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 256 * 1024:
        raise OperationError('Caddy configuration is missing, oversized or a symlink')
    return path.read_text()


def active_configuration():
    target = 'http://127.0.0.1:2019/config/'
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), RejectRedirects)
    with opener.open(target, timeout=5) as response:
        data = response.read(1024 * 1024 + 1)
        if response.status != 200 or response.geturl() != target or len(data) > 1024 * 1024:
            raise OperationError('cannot verify the running Caddy configuration')
        return json.loads(data)


def difference_paths(expected, actual, path=''):
    if isinstance(expected, dict) and isinstance(actual, dict):
        result = []
        for key in sorted(set(expected) | set(actual)):
            child = path + '/' + key
            result.extend([child] if key not in expected or key not in actual
                          else difference_paths(expected[key], actual[key], child))
        return result
    if isinstance(expected, list) and isinstance(actual, list) and len(expected) == len(actual):
        return [child for index, (left, right) in enumerate(zip(expected, actual))
                for child in difference_paths(left, right, path + '/' + str(index))]
    return [] if expected == actual else [path]


def installed_file_hides(configuration, validation, installed):
    if isinstance(configuration, dict):
        # Caddy automatically hides its input Caddyfile; reload uses the installed input path.
        if configuration.get('handler') == 'file_server' and isinstance(configuration.get('hide'), list):
            configuration['hide'] = [installed if value == validation else value for value in configuration['hide']]
        for value in configuration.values():
            installed_file_hides(value, validation, installed)
    elif isinstance(configuration, list):
        for value in configuration:
            installed_file_hides(value, validation, installed)


def configure(runtime, source):
    require_operator(runtime.root)
    files = artifacts.recovery_files(runtime)
    if not files:
        raise OperationError('register the OTA service before configuring HTTPS')
    state = read_record(runtime, files['state.json'])
    if state.get('source') != source:
        raise OperationError('OTA service source changed before proxy activation')
    stable = read_record(runtime, runtime.state / 'blue-green.json')
    if stable.get('phase') != 'STABLE':
        raise OperationError('wait for a stable production release')
    ready()
    assert_route(stable['current'])
    root = artifacts.CADDY_ROOT
    before = read_caddy(root)
    active_before = active_configuration()
    baseline = runtime.execute('apple-route-baseline', ['sudo', '-n', 'caddy', 'adapt', '--config', str(root), '--adapter', 'caddyfile'])
    if json.loads(baseline) != active_before:
        raise OperationError('running Caddy differs from its managed configuration')
    route_file = runtime.state / 'apple-delivery-routing.json'
    prior = read_record(runtime, route_file) if route_file.exists() else None
    previous = {name: read_caddy(path) if path.exists() else None for name, path in artifacts.CADDY_FILES.items()}
    desired = {'delivery.caddy': files['current/apple-delivery.caddy'].read_text(),
               'logging.caddy': files['current/apple-delivery-logging.caddy'].read_text()}
    marker = 'import ' + str(artifacts.CADDY_FILES['delivery.caddy'])
    if prior:
        after = before
    else:
        if before.lstrip().startswith('{'):
            raise OperationError('existing global Caddy options require an explicit merge')
        after = artifacts.ROOT_PREFIX + before.rstrip() + '\n\n' + marker + '\n'
    folder = files['current/selection.json'].parent
    with tempfile.TemporaryDirectory(prefix='.routing-', dir=folder) as directory:
        temporary = Path(directory)
        atomic_text(temporary / 'root.before', before)
        for name in artifacts.CADDY_FILES:
            atomic_text(temporary / name, desired[name])
            atomic_text(temporary / (name + '.before'), previous[name] or '')
        staged = after
        for name, path in artifacts.CADDY_FILES.items():
            staged = staged.replace('import ' + str(path), 'import ' + str(temporary / name))
        atomic_text(temporary / 'validation.caddy', staged)
        adapted = runtime.execute('apple-route-adapt', ['sudo', '-n', 'caddy', 'adapt', '--config', str(temporary / 'validation.caddy'), '--adapter', 'caddyfile'])
        expected = json.loads(adapted)
        installed_file_hides(expected, str(temporary / 'validation.caddy'), str(root))
        assert_logging(expected)
        runtime.execute('apple-route-validate', ['sudo', '-n', 'caddy', 'validate', '--config', str(temporary / 'validation.caddy'), '--adapter', 'caddyfile'])
        if active_configuration() != active_before or read_caddy(root) != before or any((read_caddy(path) if path.exists() else None) != previous[name] for name, path in artifacts.CADDY_FILES.items()):
            raise OperationError('Caddy configuration changed during preparation')
        if before == after and desired == previous:
            if active_before != expected:
                raise OperationError('running Caddy does not match the validated proxy configuration')
            return {'passed': True, 'changed': False}
        atomic_text(temporary / 'root.caddy', after)
        try:
            for name, path in artifacts.CADDY_FILES.items():
                install(runtime, temporary / name, path)
            install(runtime, temporary / 'root.caddy', root)
            runtime.execute('apple-route-reload', ['sudo', '-n', 'systemctl', 'reload', 'caddy'])
            actual = active_configuration()
            if actual != expected:
                raise OperationError('running Caddy differs at ' + ', '.join(difference_paths(expected, actual)[:12]))
            assert_logging(actual)
            ready()
            assert_route(stable['current'])
            record = {name: hashlib.sha256(text.encode()).hexdigest() for name, text in desired.items()}
            record.update(source=source, installer_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest())
            atomic_json(route_file, record)
            artifacts.recovery_files(runtime)
        except Exception as error:
            try:
                install(runtime, temporary / 'root.before', root)
                for name, path in artifacts.CADDY_FILES.items():
                    if previous[name] is None:
                        if path.exists() or path.is_symlink():
                            runtime.execute('apple-route-remove', ['sudo', '-n', 'unlink', str(path)])
                    else:
                        install(runtime, temporary / (name + '.before'), path)
                if prior:
                    atomic_json(route_file, prior)
                else:
                    route_file.unlink(missing_ok=True)
                runtime.execute('apple-route-restore', ['sudo', '-n', 'systemctl', 'reload', 'caddy'])
                if active_configuration() != active_before:
                    raise OperationError('running Caddy did not recover its previous configuration')
                assert_route(stable['current'])
            except Exception as recovery:
                raise OperationError('OTA proxy activation and recovery failed; operator recovery required') from recovery
            raise OperationError('OTA proxy activation failed: ' + str(error) + '; previous configuration restored') from error
    return {'passed': True, 'changed': True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', default='/opt/tellyouwhat/backend')
    parser.add_argument('--source', required=True)
    parser.add_argument('--activate', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    runtime = Runtime(args.backend_dir)
    if args.activate:
        configure(runtime, args.source)
        return
    files = artifacts.recovery_files(runtime)
    if not files:
        raise OperationError('register the OTA service before configuring HTTPS')
    state = read_record(runtime, files['state.json'])
    if state['source'] != args.source:
        raise OperationError('select the actual deployed OTA service source')
    command = [sys.executable, str(Path(__file__).resolve()), '--backend-dir', str(runtime.root), '--source', args.source, '--activate']
    result = development_deploy(runtime, 'apple-development', [state['release']], [state['image']], args.source,
                               'ota-https', command, 'http://127.0.0.1:18791/readyz')
    print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (OperationError, OSError, ValueError) as error:
        print(json.dumps({'passed': False, 'error': str(error)}), file=sys.stderr)
        sys.exit(1)
