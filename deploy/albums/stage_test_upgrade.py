"""Rehearse an empty Albums test-stack upgrade without changing the public route.

Invoke through ops_artifacts.py --run as ubuntu. The existing stack remains the
active recovery selection; the candidate has its own database, Redis and network.
"""
import argparse
import fcntl
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tencent"))
from ops_common import OperationError, atomic_json, atomic_text, read_environment

ROOT = Path('/opt/tellyouwhat/albums-test')
OPERATIONS = Path('/opt/tellyouwhat/backend/.operations')
PROJECT = 'tellyouwhat-albums-test-next'
SERVICES = ('albumgateway', 'albumworker', 'migrate')
DUMP = ('mysqldump --single-transaction --quick --hex-blob --no-tablespaces '
        '--set-gtid-purged=OFF --column-statistics=0 --skip-extended-insert '
        '--complete-insert --order-by-primary --skip-comments --default-character-set=utf8mb4 '
        '--user="$MYSQL_USER" albums_test')


def require_wrapper():
    if os.geteuid() == 0 or ROOT.stat().st_uid != os.geteuid():
        raise OperationError('run as the Albums directory owner, not root')
    for name in ('operations', 'deployment'):
        with (OPERATIONS / (name + '.lock')).open('r') as handle:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                continue
            raise OperationError('both operations locks must be held by the deployment wrapper')


def candidate_environment(before, source):
    values = read_environment(before)
    if (not re.fullmatch('[a-f0-9]{40}', source)
            or not re.fullmatch('[a-f0-9]{40}', values.get('ALBUM_TEST_IMAGE_TAG', ''))
            or values.get('ALBUM_TEST_API_PORT', '18090') != '18090'
            or values.get('ALBUM_TEST_IMAGE_PREFIX') != 'tellyouwhat-albums-test'
            or values.get('ALBUM_TEST_ENV_FILE') != str(ROOT / '.env.albums-test')
            or values.get('ALBUM_TEST_SECRET_DIRECTORY') != str(ROOT / 'secrets')):
        raise OperationError('unexpected active test-stack configuration')
    values.update(ALBUM_TEST_IMAGE_TAG=source, ALBUM_TEST_API_PORT='18091',
                  ALBUM_TEST_NETWORK_CIDR='172.29.91.0/24', ALBUM_TEST_PROXY_CIDR='172.29.91.1/32')
    return ''.join(k + '=' + v + '\n' for k, v in values.items())


def check_empty(state):
    # shortcut: this first cutover only handles the still-empty test environment.
    # Once accounts exist, use a write-fenced cutover with subscription reconciliation.
    if state['accounts'] or state['uploads']:
        raise OperationError('test stack contains accounts or uploads; do not clone live user data')


def signing_files(runtime):
    names = ['ALBUM_APPLE_SIGN_IN_KEY_FILE', 'ALBUM_CREDENTIAL_KEY_FILE', 'ALBUM_LOGIN_LIMIT_KEY_FILE']
    for enabled, fields in (
        ('ALBUM_SUBSCRIPTIONS_ENABLED', ['ALBUM_APP_STORE_KEY_FILE', 'ALBUM_APP_STORE_ROOTS_FILE']),
        ('ALBUM_RETENTION_PUSH_ENABLED', ['ALBUM_APNS_KEY_FILE']),
        ('ALBUM_SHARING_ENABLED', ['ALBUM_SHARE_KEY_FILE']),
    ):
        if runtime.get(enabled) == 'true': names.extend(fields)
    if runtime.get('ALBUM_SHARING_ENABLED') == 'true' and runtime.get('ALBUM_SHARE_PREVIOUS_KEY_FILE'):
        names.append('ALBUM_SHARE_PREVIOUS_KEY_FILE')
    values = []
    for field in names:
        value = runtime.get(field, '')
        if not re.fullmatch('/album-secrets/[a-zA-Z0-9_.-]+', value):
            raise OperationError('missing or invalid active signing material reference')
        values.append(value.removeprefix('/album-secrets/'))
    return sorted(set(values))


def ready(port):
    request = urllib.request.Request(f'http://127.0.0.1:{port}/readyz',
                                     headers={'Host': 'api.albums.tellyouwhat.cn'})
    for _ in range(30):
        try:
            with urllib.request.urlopen(request, timeout=2) as response:
                if response.geturl() == request.full_url and response.status == 200 and json.load(response) == {'code': 'ok'}:
                    return
        except (OSError, ValueError):
            pass
        time.sleep(1)
    raise OperationError('Albums readiness did not pass')


class Stage:
    def __init__(self, source, run):
        self.source = source
        self.folder = ROOT / run
        self.selection = OPERATIONS / run
        self.source_path = ROOT / ('source-' + source)
        self.active_env = ROOT / '.env.compose-albums-test'
        self.before = read_environment(self.active_env)
        self.old_source = ROOT / ('source-' + self.before['ALBUM_TEST_IMAGE_TAG'])
        self.new_env = self.folder / 'candidate.env'
        self.calls = 0

    def execute(self, label, command, *, data=None, env=None, timeout=180):
        self.calls += 1
        result = subprocess.run(command, input=data, capture_output=True,
                                env=dict(os.environ, **(env or {})), timeout=timeout)
        if result.returncode:
            path = self.folder / f'{self.calls}-{label}.stderr'
            path.write_bytes(result.stderr)
            path.chmod(0o600)
            raise OperationError(f'{label} failed; diagnostics retained privately')
        return result.stdout

    def compose(self, new, *args):
        project = PROJECT if new else 'tellyouwhat-albums-test'
        path = self.source_path if new else self.old_source
        env = self.new_env if new else self.active_env
        return ['docker', 'compose', '--project-name', project, '--env-file', str(env),
                '-f', str(path / 'compose.albums-test.yaml'), *args]

    def sql(self, new, query):
        name = (PROJECT if new else 'tellyouwhat-albums-test') + '-mysql-1'
        return self.execute('sql', ['docker', 'exec', name, 'sh', '-c',
            'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql --user="$MYSQL_USER" '
            '--database=albums_test --batch --skip-column-names -e "$1"', 'sh', query]).decode().splitlines()

    def state(self, new):
        rows = self.sql(new, 'SELECT COUNT(*),MAX(name) FROM schema_migrations; '
                        'SELECT COUNT(*) FROM album_accounts; SELECT COUNT(*) FROM album_uploads; '
                        'SELECT COUNT(*) FROM album_subscription_notifications;')
        count, latest = rows[0].split('\t')
        return dict(migrations=int(count), latest=latest, accounts=int(rows[1]),
                    uploads=int(rows[2]), notifications=int(rows[3]))

    def dump(self, new):
        name = (PROJECT if new else 'tellyouwhat-albums-test') + '-mysql-1'
        return self.execute('dump', ['docker', 'exec', name, 'sh', '-c', 'MYSQL_PWD="$MYSQL_PASSWORD" exec ' + DUMP])

    def snapshot(self):
        try:
            self.execute('drain', self.compose(False, 'stop', '-t', '40', 'albumgateway', 'albumworker'))
            for service in ('albumgateway', 'albumworker'):
                result = json.loads(self.execute('drained', ['docker', 'inspect', '--format', '{{json .State}}',
                                    'tellyouwhat-albums-test-' + service + '-1']))
                if result['Running'] or result['OOMKilled'] or result['ExitCode'] not in (0, 143):
                    raise OperationError('old API/worker did not drain cleanly')
            baseline = self.state(False)
            check_empty(baseline)
            data = self.dump(False)
            raw = self.folder / 'database.sql'
            raw.write_bytes(data)
            raw.chmod(0o600)
            secret = secrets.token_hex(32)
            key_file = self.folder / 'snapshot.key'
            atomic_text(key_file, secret + '\n')
            encrypted = self.folder / 'database.sql.enc'
            crypto = ['openssl', 'enc', '-aes-256-cbc', '-pbkdf2', '-iter', '200000', '-md', 'sha256',
                      '-pass', 'env:ALBUM_UPGRADE_BACKUP_KEY']
            self.execute('encrypt', crypto + ['-in', str(raw), '-out', str(encrypted)],
                         env={'ALBUM_UPGRADE_BACKUP_KEY': secret})
            content = encrypted.read_bytes()
            auth = hmac.new(bytes.fromhex(secret), content, hashlib.sha256).hexdigest()
            recovered = self.execute('decrypt', crypto + ['-d', '-in', str(encrypted)],
                                     env={'ALBUM_UPGRADE_BACKUP_KEY': secret})
            if recovered != data:
                raise OperationError('snapshot decryption differs')
            atomic_json(self.folder / 'snapshot.json', dict(ciphertext_sha256=hashlib.sha256(content).hexdigest(),
                         hmac_sha256=auth, plaintext_sha256=hashlib.sha256(data).hexdigest(), state=baseline))
            return baseline, data
        finally:
            # Always recover the untouched original database and exact old binaries.
            (self.folder / 'database.sql').unlink(missing_ok=True)
            self.execute('resume-active', self.compose(False, 'start', 'albumgateway', 'albumworker'))
            ready(18090)

    def run(self):
        candidate = candidate_environment(self.active_env, self.source)
        ready(18090)
        check_empty(self.state(False))
        if self.execute('existing-candidate', ['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + PROJECT]).strip():
            raise OperationError('candidate already exists; inspect it rather than repeat staging')
        if self.execute('existing-volume', ['docker', 'volume', 'ls', '-q', '--filter', 'name=' + PROJECT + '_albums-test-mysql']).strip():
            raise OperationError('candidate database volume already exists; preserve it for inspection')
        atomic_text(self.new_env, candidate)
        atomic_text(self.folder / 'active-compose.env', self.active_env.read_text())
        atomic_text(self.folder / 'active-runtime.env', (ROOT / '.env.albums-test').read_text())
        runtime = read_environment(ROOT / '.env.albums-test')
        signing = {}
        for name in signing_files(runtime):
            # Disabled features can retain paths to keys that have not been created.
            digest = self.execute('signing-digest', ['sudo', '-n', 'sha256sum', str(ROOT / 'secrets' / name)]).decode().split()[0]
            signing[name] = digest
        recovery = dict(compose_sha256=hashlib.sha256((self.old_source / 'compose.albums-test.yaml').read_bytes()).hexdigest(),
                        runtime_sha256=hashlib.sha256((ROOT / '.env.albums-test').read_bytes()).hexdigest(),
                        signing_sha256=signing,
                        database_mounts=json.loads(self.execute('active-volume', ['docker', 'inspect', '--format', '{{json .Mounts}}',
                                                             'tellyouwhat-albums-test-mysql-1'])))
        self.execute('validate', self.compose(True, 'config', '--quiet'))
        self.execute('build', self.compose(True, 'build', '--build-arg', 'GOPROXY=https://goproxy.cn,direct', *SERVICES), timeout=700)
        baseline, data = self.snapshot()
        try:
            self.execute('candidate-databases', self.compose(True, 'up', '-d', '--wait', '--wait-timeout', '120', 'mysql', 'redis'))
            self.execute('restore', ['docker', 'exec', '-i', PROJECT + '-mysql-1', 'sh', '-c',
                         'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql --user="$MYSQL_USER" --database=albums_test'], data=data)
            if self.dump(True) != data or self.state(True) != baseline:
                raise OperationError('candidate restore differs from exact baseline snapshot')
            self.execute('migrate', self.compose(True, 'run', '--rm', '--no-deps', 'migrate'))
            expected = sorted(p.name for p in (self.source_path / 'migrations').glob('*.sql'))
            after = self.state(True)
            if after != dict(baseline, migrations=len(expected), latest=expected[-1]):
                raise OperationError('migration changed test data or omitted a schema step')
            self.execute('candidate-services', self.compose(True, 'up', '-d', '--no-deps', 'albumgateway', 'albumworker'))
            ready(18091)
            time.sleep(5)
            states = {}
            for service in ('albumgateway', 'albumworker'):
                item = json.loads(self.execute('candidate-state', ['docker', 'inspect', PROJECT + '-' + service + '-1']))[0]
                if not item['State']['Running'] or item['RestartCount'] or item['State']['OOMKilled']:
                    raise OperationError('candidate service did not remain running')
                states[service] = dict(id=item['Id'], image=item['Image'], started_at=item['State']['StartedAt'])
            ready(18090)
            if self.state(False) != baseline:
                raise OperationError('active test data changed; staged clone is stale')
            result = dict(source=self.source, baseline_source=self.before['ALBUM_TEST_IMAGE_TAG'],
                          before=baseline, after=after, restore_identical=True, snapshot_encrypted=True,
                          candidate_project=PROJECT, candidate_port=18091, active_port=18090,
                          public_route_changed=False, candidate_services=states,
                          recovery=recovery,
                          completed_at=int(time.time()))
            self.selection.mkdir(mode=0o700)
            atomic_json(self.selection / 'selection.json', dict(result, recovery_path=str(self.folder)))
            atomic_json(self.folder / 'evidence.json', result)
            return result
        except Exception:
            self.execute('stop-candidate', self.compose(True, 'stop', '-t', '40', 'albumgateway', 'albumworker'))
            raise
        finally:
            (self.folder / 'database.sql').unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', required=True)
    parser.add_argument('--run', required=True)
    args = parser.parse_args()
    if not re.fullmatch('[a-f0-9]{40}', args.source) or not re.fullmatch('albums-upgrade-[a-z0-9-]{1,40}', args.run):
        raise OperationError('invalid source or run identifier')
    os.umask(0o077)
    require_wrapper()
    stage = Stage(args.source, args.run)
    stage.folder.mkdir(mode=0o700)
    print(json.dumps(stage.run(), sort_keys=True))


if __name__ == '__main__':
    main()
