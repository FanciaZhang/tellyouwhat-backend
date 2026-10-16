"""Admit the existing Albums production store to the shared release slots."""

import ipaddress
import json
import os
from pathlib import Path
import re

from ops_common import OperationError, read_environment
from albums_artifacts import PRODUCTION_FILES, selected_files, production_keys

OVERLAY = 'compose.shared-albums-production.yaml'
PROJECT = 'tellyouwhat-albums-production'
NETWORK = PROJECT + '_albums-production'
SECRETS = Path('/opt/tellyouwhat/albums-production/secrets')
REGISTRY = {'ALBUM_API_HOST': 'api.albums.tellyouwhat.cn',
            'ALBUM_APPLE_BUNDLE_ID': 'cn.tellyouwhat.albums',
            'ALBUM_APPLE_TEAM_ID': 'T6DD96HY2B', 'ALBUM_DEPLOYMENT_ENVIRONMENT': 'production'}


def selected(config):
    return 'albums' in [s.strip() for s in config.get('PLATFORM_APPS', '').split(',')]


def private_file(path, owner, limit=32768):
    if (path.is_symlink() or not path.is_file() or path.stat().st_uid != owner
            or path.stat().st_mode & 0o077 or not 0 < path.stat().st_size <= limit):
        raise OperationError('Albums release configuration must be private and operator owned')
    return path.read_bytes()


def validate_registry(config):
    fields = {k for k in config if k.startswith('ALBUM_')}
    if not selected(config):
        if fields:
            raise OperationError('Albums settings require explicit production admission')
        return False
    if (config.get('APP_ENV') != 'production' or config.get('PUBLIC_PROXY_MODE') != 'external'
            or fields != set(REGISTRY) | {'ALBUM_RESOURCE_SOURCE'}
            or any(config.get(k) != v for k, v in REGISTRY.items())
            or not re.fullmatch('[a-f0-9]{40}', config.get('ALBUM_RESOURCE_SOURCE', ''))):
        raise OperationError('Albums production identity or credential boundary is invalid')
    # Joining a second network must not change mysql/redis alias resolution.
    try:
        for key in ('MYSQL_HOST', 'REDIS_HOST'):
            ipaddress.ip_address(config.get(key, ''))
    except ValueError as error:
        raise OperationError('shared data services require explicit IP addresses before Albums admission') from error
    return True


def validate(root, config):
    if not validate_registry(config):
        return
    root = Path(root)
    overlay = root / OVERLAY
    if overlay.is_symlink() or not overlay.is_file():
        raise OperationError('Albums production Compose overlay is missing')
    private_file(root / 'albums.env', root.stat().st_uid)
    app = read_environment(root / 'albums.env')
    production_keys(app)
    if (any(not k.startswith('ALBUM_') for k in app) or any(app.get(k) != v for k, v in REGISTRY.items())):
        raise OperationError('gateway Albums settings contain another environment or unrelated credentials')
    proxies = app.get('ALBUM_TRUSTED_PROXIES', '').split(',')
    try:
        if not 1 <= len(proxies) <= 3:
            raise ValueError()
        for proxy in proxies:
            network = ipaddress.ip_network(proxy, strict=True)
            if network.version != 4 or network.prefixlen != 32 or not network.is_private:
                raise ValueError()
    except ValueError as error:
        raise OperationError('Albums proxy trust must name individual private host gateways') from error


def compose_overlay(root, config):
    validate(root, config)
    return ['-f', str(Path(root) / OVERLAY)] if selected(config) else []


def require_configuration(runtime, root, config):
    validate(root, config)
    if not selected(config):
        return
    owner = runtime.root.stat().st_uid
    source = config['ALBUM_RESOURCE_SOURCE']
    original = runtime.root / '.albums-production/releases' / source
    for path in (original, original.parent, original.parent.parent):
        if path.is_symlink() or not path.is_dir() or path.stat().st_mode & 0o077 or path.stat().st_uid != owner:
            raise OperationError('Albums production resource selection must remain private')
    state = json.loads(private_file(runtime.state / 'albums-production.json', owner, 16384))
    registry = json.loads(private_file(runtime.state / 'artifact-owners/albums-production.json', owner, 16384))
    if (state.get('source') != source or state.get('release') != str(original)
            or not any(s and s.get('source') == source and str(original) in s.get('paths', [])
                       for s in (registry.get('current'), registry.get('previous')))):
        raise OperationError('Albums resources must match their registered production selection')
    selected_files(runtime, original, source, PRODUCTION_FILES)
    compose = read_environment(original / 'compose.env')
    passwords = [compose.get('ALBUM_PRODUCTION_' + k, '') for k in ('MYSQL_PASSWORD', 'REDIS_PASSWORD')]
    if (compose.get('ALBUM_PRODUCTION_SECRET_DIRECTORY') != str(SECRETS)
            or compose.get('ALBUM_PRODUCTION_MYSQL_USER') != 'albums_production'
            or compose.get('ALBUM_PRODUCTION_NETWORK_CIDR') != '172.29.94.0/24'
            or any(not re.fullmatch('[a-f0-9]{64}', p) for p in [*passwords, compose.get('ALBUM_PRODUCTION_MYSQL_ROOT_PASSWORD', '')])
            or len(set([*passwords, compose.get('ALBUM_PRODUCTION_MYSQL_ROOT_PASSWORD')])) != 3
            or any(p == config.get(k) for p in passwords for k in ('MYSQL_PASSWORD', 'MYSQL_ROOT_PASSWORD', 'REDIS_PASSWORD'))):
        raise OperationError('Albums production database credentials are not isolated')
    app = read_environment(Path(root) / 'albums.env')
    expected = gateway_environment(read_environment(original / 'runtime.env'), compose, app['ALBUM_TRUSTED_PROXIES'])
    if app != expected:
        raise OperationError('shared gateway differs from the selected Albums production resources')
    if SECRETS.is_symlink() or not SECRETS.is_dir():
        raise OperationError('Albums production secret directory is unavailable')
    return compose


def require_resources(runtime, root, config):
    compose = require_configuration(runtime, root, config)
    if compose is None:
        return
    source = config['ALBUM_RESOURCE_SOURCE']
    passwords = [compose['ALBUM_PRODUCTION_' + k] for k in ('MYSQL_PASSWORD', 'REDIS_PASSWORD')]
    for service in ('mysql', 'redis', 'albumworker'):
        container = json.loads(runtime.execute('production-albums-' + service, ['docker', 'inspect', PROJECT + '-' + service + '-1']))[0]
        if (not container.get('State', {}).get('Running') or container['State'].get('OOMKilled')
                or set(container.get('NetworkSettings', {}).get('Networks', {})) != {NETWORK}
                or container.get('Config', {}).get('Labels', {}).get('com.docker.compose.project') != PROJECT):
            raise OperationError('Albums production data service or media worker is unavailable')
        env = dict(s.split('=', 1) for s in container['Config'].get('Env', []) if '=' in s)
        required = ({'MYSQL_DATABASE': 'albums_production', 'MYSQL_USER': 'albums_production', 'MYSQL_PASSWORD': passwords[0]}
                    if service == 'mysql' else {'REDIS_PASSWORD': passwords[1]} if service == 'redis' else {
                        **read_environment(runtime.root / '.albums-production/releases' / source / 'runtime.env'),
                        'ALBUM_DEPLOYMENT_ENVIRONMENT': 'production',
                        'ALBUM_DATABASE_DSN': 'albums_production:' + passwords[0] + '@tcp(mysql:3306)/albums_production?charset=utf8mb4&collation=utf8mb4_0900_ai_ci',
                        'ALBUM_REDIS_URL': 'redis://:' + passwords[1] + '@redis:6379/0'})
        if service == 'albumworker' and container.get('Mounts'):
            raise OperationError('Albums media worker must not mount gateway private keys or other host files')
        if (any(env.get(k) != v for k, v in required.items()) or (service == 'albumworker'
                and container['Config'].get('Image') != 'tellyouwhat-albums-production-albumworker:' + source)):
            raise OperationError('Albums running data resources differ from their selection')


def verify_gateway(runtime, root, config, slot):
    if not selected(config):
        return
    app = read_environment(Path(root) / 'albums.env')
    container = json.loads(runtime.execute('production-albums-gateway', ['docker', 'inspect', 'tellyouwhat-' + slot + '-gateway-1']))[0]
    env = dict(s.split('=', 1) for s in container.get('Config', {}).get('Env', []) if '=' in s)
    networks = container.get('NetworkSettings', {}).get('Networks', {})
    primary = 'tellyouwhat-' + slot + '_tellyouwhat-backend'
    mounts = [m for m in container.get('Mounts', []) if m.get('Destination') == '/album-secrets']
    if (any(env.get(k) != v for k, v in app.items()) or set(networks) != {primary, NETWORK}
            or networks.get(primary, {}).get('Gateway', '') + '/32' not in app['ALBUM_TRUSTED_PROXIES'].split(',')
            or len(mounts) != 1 or mounts[0].get('Source') != str(SECRETS)
            or mounts[0].get('Type') != 'bind' or mounts[0].get('RW') is not False):
        raise OperationError('shared production gateway Albums network, proxy trust or key mount is invalid')


def verify_compose(config, rendered):
    if not selected(config):
        return
    for name, service in rendered['services'].items():
        if name == 'gateway':
            continue
        env = service.get('environment', {})
        if (any(k.startswith('ALBUM_') and k not in set(REGISTRY) | {'ALBUM_RESOURCE_SOURCE'} for k in env)
                or 'albums-production' in service.get('networks', {})
                or any(v.get('target') == '/album-secrets' for v in service.get('volumes', []))):
            raise OperationError('Albums credentials, network or key mount reached another shared service')


def gateway_environment(app, compose, proxies):
    return {**app, 'ALBUM_DEPLOYMENT_ENVIRONMENT': 'production',
            'ALBUM_DATABASE_DSN': 'albums_production:' + compose.get('ALBUM_PRODUCTION_MYSQL_PASSWORD', '') + '@tcp(' + PROJECT + '-mysql-1:3306)/albums_production?charset=utf8mb4&collation=utf8mb4_0900_ai_ci',
            'ALBUM_REDIS_URL': 'redis://:' + compose.get('ALBUM_PRODUCTION_REDIS_PASSWORD', '') + '@' + PROJECT + '-redis-1:6379/0',
            'ALBUM_TRUSTED_PROXIES': proxies}


def stage(runtime, root):
    root = Path(root)
    config = read_environment(root / '.env.production')
    if not validate_registry(config):
        return
    source = config['ALBUM_RESOURCE_SOURCE']
    original = runtime.root / '.albums-production/releases' / source
    selected_files(runtime, original, source, PRODUCTION_FILES)
    compose = read_environment(original / 'compose.env')
    proxies = []
    for slot in ('blue', 'green'):
        name = 'tellyouwhat-' + slot + '_tellyouwhat-backend'
        network = json.loads(runtime.execute('production-albums-proxy-' + slot, ['docker', 'network', 'inspect', name]))[0]
        if network.get('Name') != name:
            raise OperationError('production release network does not match its slot')
        proxies.extend(c['Gateway'] + '/32' for c in network.get('IPAM', {}).get('Config', []) if c.get('Gateway'))
    values = gateway_environment(read_environment(original / 'runtime.env'), compose, ','.join(sorted(set(proxies))))
    target = root / 'albums.env'
    body = ''.join(k + '=' + v + '\n' for k, v in values.items()).encode()
    created = False
    try:
        if target.exists() or target.is_symlink():
            if private_file(target, runtime.root.stat().st_uid) != body:
                raise OperationError('staged Albums credentials changed; use a new matched release bundle')
        else:
            descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, 'wb') as output:
                output.write(body)
            created = True
        require_resources(runtime, root, config)
    except Exception:
        if created:
            target.unlink()
        raise


if __name__ == '__main__':
    import argparse
    from ops_common import Runtime
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend-dir', required=True)
    parser.add_argument('--bundle', required=True)
    args = parser.parse_args()
    runtime = Runtime(args.backend_dir)
    from ops_artifacts import require_operator
    require_operator(runtime)
    stage(runtime, args.bundle)
