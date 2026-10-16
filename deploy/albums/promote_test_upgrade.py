"""Promote the verified, still-empty Albums candidate through the operations wrapper.

Never roll back a database snapshot after the new public API may have accepted a
write. An uncertain Caddy reload leaves the candidate running for forward repair.
"""
import argparse
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import subprocess
import urllib.request

from stage_test_upgrade import (ROOT, PROJECT, Stage, OperationError, atomic_json,
                                atomic_text, ready, require_wrapper, candidate_environment)

ROUTE = Path('/etc/caddy/albums-test.caddy')


def verify_snapshot(folder):
    manifest = json.loads((folder / 'snapshot.json').read_text())
    ciphertext = (folder / 'database.sql.enc').read_bytes()
    key = bytes.fromhex((folder / 'snapshot.key').read_text().strip())
    if len(key) != 32 or not hmac.compare_digest(hashlib.sha256(ciphertext).hexdigest(), manifest['ciphertext_sha256']):
        raise OperationError('snapshot ciphertext changed')
    if not hmac.compare_digest(hmac.new(key, ciphertext, hashlib.sha256).hexdigest(), manifest['hmac_sha256']):
        raise OperationError('snapshot authentication failed')
    decrypted = subprocess.run(['openssl', 'enc', '-aes-256-cbc', '-pbkdf2', '-iter', '200000', '-md', 'sha256',
                                '-d', '-pass', 'env:ALBUM_UPGRADE_BACKUP_KEY', '-in', str(folder / 'database.sql.enc')],
                               env=dict(os.environ, ALBUM_UPGRADE_BACKUP_KEY=key.hex()), capture_output=True, timeout=60)
    if decrypted.returncode or hashlib.sha256(decrypted.stdout).hexdigest() != manifest['plaintext_sha256']:
        raise OperationError('snapshot plaintext digest differs')
    return manifest


def replacement_route(text):
    if text.count('api.albums.tellyouwhat.cn {') != 1 or text.count('reverse_proxy 127.0.0.1:18090') != 1:
        raise OperationError('active Albums route differs; inspect instead of overwriting')
    if '127.0.0.1:18091' in text:
        raise OperationError('candidate route already present')
    return text.replace('reverse_proxy 127.0.0.1:18090', 'reverse_proxy 127.0.0.1:18091')


def active_upstream():
    with urllib.request.urlopen('http://127.0.0.1:2019/config/', timeout=5) as response:
        config = json.load(response)
    def walk(value):
        if isinstance(value, dict):
            yield value
            for child in value.values(): yield from walk(child)
        elif isinstance(value, list):
            for child in value: yield from walk(child)
    routes = [v for v in walk(config) if any(m.get('host') == ['api.albums.tellyouwhat.cn'] for m in v.get('match', []) if isinstance(m, dict))]
    upstreams = [u['dial'] for route in routes for v in walk(route) for u in v.get('upstreams', [])]
    if upstreams != ['127.0.0.1:18091']:
        raise OperationError('loaded Caddy route does not point to candidate')


def promote(stage):
    evidence = json.loads((stage.folder / 'evidence.json').read_text())
    snapshot = verify_snapshot(stage.folder)
    if (evidence['source'] != stage.source or evidence['public_route_changed']
            or stage.state(False) != evidence['before'] or stage.state(True) != evidence['after']
            or evidence['before']['accounts'] or evidence['before']['uploads']
            or (stage.new_env.read_text() != candidate_environment(stage.active_env, stage.source))):
        raise OperationError('candidate or active state differs from verified staging')
    for original, saved in ((stage.active_env, 'active-compose.env'), (ROOT / '.env.albums-test', 'active-runtime.env')):
        if original.read_bytes() != (stage.folder / saved).read_bytes():
            raise OperationError('configuration changed after staging')
    for name, expected in evidence['recovery']['signing_sha256'].items():
        if not re.fullmatch('[a-zA-Z0-9_.-]+', name): raise OperationError('invalid signing reference')
        actual = stage.execute('signing-digest', ['sudo', '-n', 'sha256sum', str(ROOT / 'secrets' / name)]).decode().split()[0]
        if actual != expected: raise OperationError('signing material changed after staging')
    for service, expected in evidence['candidate_services'].items():
        item = json.loads(stage.execute('candidate-inspect', ['docker', 'inspect', PROJECT + '-' + service + '-1']))[0]
        if item['Id'] != expected['id'] or item['Image'] != expected['image'] or not item['State']['Running'] or item['RestartCount']:
            raise OperationError('candidate service changed or restarted')
    ready(18090)
    ready(18091)
    route = ROUTE
    before = route.read_text()
    after = replacement_route(before)
    atomic_text(stage.folder / 'caddy-before', before)
    atomic_text(stage.folder / 'caddy-after', after)
    # A marker distinguishes an unknown switch from a safe new attempt.
    with (stage.folder / 'promotion-started').open('x') as handle: handle.write(stage.source + '\n')
    switch_started = False
    route_written = False
    try:
        stage.execute('drain-public', stage.compose(False, 'stop', '-t', '40', 'albumgateway', 'albumworker'))
        for service in ('albumgateway', 'albumworker'):
            state = json.loads(stage.execute('drain-state', ['docker', 'inspect', '--format', '{{json .State}}',
                                           'tellyouwhat-albums-test-' + service + '-1']))
            if state['Running'] or state['OOMKilled'] or state['ExitCode'] not in (0, 143):
                raise OperationError('old writers did not drain')
        if stage.state(False) != evidence['before'] or hashlib.sha256(stage.dump(False)).hexdigest() != snapshot['plaintext_sha256']:
            raise OperationError('active data changed after staging; new clone required')
        if stage.state(True) != evidence['after']:
            raise OperationError('candidate acquired data before promotion')
        route_written = True
        stage.execute('install-route', ['sudo', '-n', 'install', '-o', 'root', '-g', 'root', '-m', '0644',
                                       str(stage.folder / 'caddy-after'), str(route)])
        stage.execute('validate-caddy', ['sudo', '-n', 'caddy', 'validate', '--config', '/etc/caddy/Caddyfile'])
        switch_started = True
        atomic_json(stage.folder / 'promotion-state.json', dict(state='reload-started', source=stage.source))
        stage.execute('reload-caddy', ['sudo', '-n', 'systemctl', 'reload', 'caddy'])
        active_upstream()
        ready(18091)
        with urllib.request.urlopen('https://api.albums.tellyouwhat.cn/readyz', timeout=15) as response:
            if response.status != 200 or json.load(response) != {'code': 'ok'}:
                raise OperationError('public readiness failed')
        evidence.update(public_route_changed=True, active_port=18091, previous_port=18090,
                        original_writers_stopped=True, loaded_caddy_upstream='127.0.0.1:18091')
        atomic_json(stage.folder / 'promotion-state.json', dict(state='promoted', source=stage.source))
        atomic_json(stage.folder / 'promoted-evidence.json', evidence)
        atomic_json(stage.selection / 'selection.json', dict(evidence, recovery_path=str(stage.folder)))
        return evidence
    except Exception:
        if switch_started:
            atomic_json(stage.folder / 'promotion-state.json', dict(state='inspect-forward-recovery', source=stage.source))
            # New writes may already exist. Preserve the live candidate and both DBs.
        else:
            if route_written:
                stage.execute('restore-route', ['sudo', '-n', 'install', '-o', 'root', '-g', 'root', '-m', '0644',
                                                str(stage.folder / 'caddy-before'), str(route)])
            stage.execute('resume-public', stage.compose(False, 'start', 'albumgateway', 'albumworker'))
            ready(18090)
            atomic_json(stage.folder / 'promotion-state.json', dict(state='old-service-restored', source=stage.source))
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', required=True)
    parser.add_argument('--run', required=True)
    args = parser.parse_args()
    if not re.fullmatch('[a-f0-9]{40}', args.source) or not re.fullmatch('albums-upgrade-[a-z0-9-]{1,40}', args.run):
        raise OperationError('invalid stage reference')
    os.umask(0o077)
    require_wrapper()
    print(json.dumps(promote(Stage(args.source, args.run)), sort_keys=True))


if __name__ == '__main__': main()
