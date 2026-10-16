"""Recover private Journal configuration and cost accounting without user content."""

import hashlib
import datetime
import hmac
import io
import json
import os
from pathlib import Path
import re
import secrets
import tarfile
import tempfile
import time
import uuid

from ops_common import OperationError, atomic_json, read_environment
from ops_backup import crypt, manifest_signature
from platform_test_deploy import private_file

FILES = {'selection.json', 'runtime.env', 'prompt.json', 'journaldevserver', 'cost-events.json'}
CHECKPOINT = 'checkpoint.tar.gz.enc'
ENVELOPE = 'checkpoint.json'


def digest(data):
    return hashlib.sha256(data).hexdigest()


def inner_runtime(runtime):
    settings = runtime.root.parent / 'journal-development/backup.env'
    private_file(runtime, settings, 256)
    config = read_environment(settings)
    if (set(config) != {'BACKUP_ENCRYPTION_KEY'}
            or not re.fullmatch('[a-f0-9]{64}', config['BACKUP_ENCRYPTION_KEY'])
            or config['BACKUP_ENCRYPTION_KEY'] == runtime.config.get('BACKUP_ENCRYPTION_KEY')):
        raise OperationError('Journal recovery requires its own escrowed encryption key')
    from types import SimpleNamespace
    return SimpleNamespace(config=config, execute=runtime.execute)


def validate_cost(raw):
    events = json.loads(raw)
    if events is None:
        return 0
    fields = {
        'reserve': {'ID', 'AppID', 'Operation', 'Meter', 'MonthStart', 'ReservedNanos', 'CreatedAt', 'LeaseExpiresAt'},
        'limits': {'MonthlyBudgetNanos', 'MaxConcurrent', 'LeaseDuration'},
        'budget': {'month_start', 'monthly_budget_nanos'},
    }
    if not isinstance(events, list):
        raise OperationError('invalid Journal cost ledger')
    for event in events:
        if not isinstance(event, dict) or set(event) - {'reserve', 'budget', 'limits', 'id', 'actual', 'known', 'now'}:
            raise OperationError('Journal recovery cannot include unknown cost payload fields')
        for key, allowed in fields.items():
            if key in event and (not isinstance(event[key], dict) or set(event[key]) - allowed):
                raise OperationError('Journal recovery cannot include unknown accounting fields')
        try:
            for field in ('reserve', 'limits', 'budget'):
                for key, value in event.get(field, {}).items():
                    if key in {'ID'}:
                        uuid.UUID(value)
                    elif key in {'AppID', 'Operation', 'Meter'}:
                        if not isinstance(value, str) or not re.fullmatch(r'[a-z][a-z0-9_.-]{0,63}', value):
                            raise ValueError()
                    elif key in {'MonthStart', 'CreatedAt', 'LeaseExpiresAt', 'month_start'}:
                        datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))
                    elif type(value) is not int or not 0 <= value < 2**63:
                        raise ValueError()
            if 'id' in event:
                uuid.UUID(event['id'])
            if 'now' in event:
                datetime.datetime.fromisoformat(event['now'].replace('Z', '+00:00'))
            if 'actual' in event and (type(event['actual']) is not int or not 0 <= event['actual'] < 2**63):
                raise ValueError()
            if 'known' in event and type(event['known']) is not bool:
                raise ValueError()
        except (ValueError, TypeError, AttributeError) as error:
            raise OperationError('Journal cost ledger contains invalid accounting values') from error
    return len(events)


def cost_snapshot(runtime, root):
    # Read the single atomic ledger; never traverse recordings, image jobs or consent stores.
    code = r'''
import os,pathlib,stat,sys
root=pathlib.Path(sys.argv[1]);path=root/'state/cost-events.json'
assert not root.is_symlink() and not path.parent.is_symlink() and not path.is_symlink()
assert path.parent.stat().st_uid==65532 and path.parent.stat().st_mode&0o077==0
fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
with os.fdopen(fd,'rb') as source:
 info=os.fstat(source.fileno())
 assert stat.S_ISREG(info.st_mode) and info.st_uid==65532 and info.st_mode&0o077==0 and info.st_size<=32*1024**2
 data=source.read(32*1024**2+1);assert len(data)<=32*1024**2
sys.stdout.buffer.write(data)
'''
    raw = runtime.execute('backup-journal-cost-ledger', ['sudo', '-n', 'python3', '-c', code, str(root)])
    validate_cost(raw)
    return raw


def backup_configuration(runtime, destination):
    marker = runtime.state / 'journal-development.json'
    if not marker.exists():
        return {}
    state = json.loads(private_file(runtime, marker, 16384))
    owner = json.loads(private_file(runtime, runtime.state / 'artifact-owners/journal-development.json', 16384))['current']
    root = runtime.root.parent / 'journal-development'
    binary = Path(state['binary'])
    if (not re.fullmatch('[a-f0-9]{40}', state.get('source', '')) or state['source'] != owner.get('source')
            or str(binary) not in owner.get('paths', []) or binary.parent != root / 'bin' or binary.is_symlink()
            or not binary.is_file() or not 0 < binary.stat().st_size <= 64*1024**2):
        raise OperationError('Journal recovery does not match the registered binary')
    program = binary.read_bytes()
    if (binary.name != 'journaldevserver-' + digest(program)[:16]
            or ':binary-sha256:' + digest(program) not in owner.get('source_run', '')):
        raise OperationError('Journal recovery binary differs from its reviewed source')
    container = json.loads(runtime.execute('backup-journal-runtime', ['docker', 'inspect', 'journal-private-development']))[0]
    env = dict(value.split('=', 1) for value in container['Config']['Env'] if '=' in value)
    mounts = {m['Destination']: m for m in container['Mounts']}
    # Retired containers still hold the pinned configuration needed to preserve their recovery material.
    if (container['Image'] not in owner['images']
            or container['Config']['Image'] != state['image'] or container['Config']['User'] != '65532:65532'
            or any(env.get(k) for k in ('DATABASE_DSN', 'REDIS_URL', 'JOURNAL_PROMPT_DATABASE_DSN'))
            or env.get('APP_ENV') == 'production' or env.get('JOURNAL_PROMPT_FILE') != '/journal-config/prompt.json'
            or mounts.get('/journaldevserver', {}).get('Source') != str(binary) or mounts['/journaldevserver']['RW']
            or mounts.get('/journal-config', {}).get('Source') != str(root / 'config') or mounts['/journal-config']['RW']):
        raise OperationError('Journal recovery runtime crosses its isolated configuration boundary')
    settings = root / 'runtime.env'
    private_file(runtime, settings, 32768)
    config = read_environment(settings)
    if (not re.fullmatch(r'ghcr\.io/fanciazhang/tellyouwhat-ai-gateway@sha256:[a-f0-9]{64}', state['image'])
            or config.get('JOURNAL_DEVELOPMENT_RUNTIME_IMAGE') != state['image']
            or any(env.get(k) != v for k, v in config.items() if k != 'JOURNAL_DEVELOPMENT_RUNTIME_IMAGE')
            or 'JOURNAL_DEVELOPMENT_TOKEN' in config
            or any(not k.startswith('JOURNAL_') or 'DSN' in k or 'REDIS' in k for k in config)):
        raise OperationError('Journal recovery configuration differs from the running service')
    prompt = root / 'config/prompt.json'
    if (prompt.parent.is_symlink() or prompt.is_symlink() or not prompt.is_file()
            or prompt.stat().st_uid != runtime.root.stat().st_uid or prompt.stat().st_mode & 0o037
            or not 0 < prompt.stat().st_size <= 512*1024):
        raise OperationError('Journal recovery Prompt file must be private and bounded')
    policy = prompt.read_bytes()
    revision = json.loads(policy)
    if revision.get('scope') != 'journal' or not revision.get('publishedAt'):
        raise OperationError('Journal recovery requires a published Journal policy')
    ledger = cost_snapshot(runtime, root)
    selection = {**state, 'binary_sha256': digest(program), 'cost_events': validate_cost(ledger),
                 'new_connection_required_after_restore': True, 'user_content_and_consents_excluded': True}
    data = {'selection.json': json.dumps(selection, sort_keys=True).encode(), 'runtime.env': settings.read_bytes(),
            'prompt.json': policy, 'journaldevserver': program, 'cost-events.json': ledger}
    child = inner_runtime(runtime)
    key = child.config['BACKUP_ENCRYPTION_KEY']
    if any(key.encode() in value for value in data.values()):
        raise OperationError('Journal recovery cannot contain its decryption key')
    destination.mkdir(mode=0o700)
    with tempfile.TemporaryDirectory(prefix='.journal-checkpoint-', dir=destination.parent) as directory:
        archive = Path(directory) / 'checkpoint.tar.gz'
        with tarfile.open(archive, 'w:gz') as output:
            for name, content in data.items():
                member = tarfile.TarInfo(name)
                member.size, member.mode = len(content), 0o600
                output.addfile(member, io.BytesIO(content))
        crypt(child, archive, destination / CHECKPOINT)
    manifest = {'version': 1, 'created_at': int(time.time()), 'source': state['source'],
                'sha256': digest((destination / CHECKPOINT).read_bytes()), 'files': {n: digest(v) for n, v in data.items()}}
    atomic_json(destination / ENVELOPE, {'manifest': manifest, 'hmac_sha256': manifest_signature(key, manifest)})
    read_checkpoint(runtime, destination)
    return {name: digest((destination / name).read_bytes()) for name in (CHECKPOINT, ENVELOPE)}


def read_checkpoint(runtime, directory):
    directory = Path(directory)
    child = inner_runtime(runtime)
    envelope = json.loads((directory / ENVELOPE).read_bytes())
    manifest = envelope['manifest']
    if (not hmac.compare_digest(envelope['hmac_sha256'], manifest_signature(child.config['BACKUP_ENCRYPTION_KEY'], manifest))
            or manifest.get('version') != 1 or set(manifest.get('files', {})) != FILES
            or manifest['sha256'] != digest((directory / CHECKPOINT).read_bytes())):
        raise OperationError('Journal checkpoint authentication failed')
    with tempfile.TemporaryDirectory(prefix='.journal-verify-', dir=directory.parent) as temporary:
        plain = Path(temporary) / 'checkpoint.tar.gz'
        crypt(child, directory / CHECKPOINT, plain, decrypt=True)
        with tarfile.open(plain, 'r:gz') as archive:
            members = archive.getmembers()
            if (len(members) != len(FILES) or {m.name for m in members} != FILES
                    or any(not m.isfile() for m in members) or sum(m.size for m in members) > 100*1024**2):
                raise OperationError('Journal checkpoint contains unexpected data')
            data = {m.name: archive.extractfile(m).read() for m in members}
    if any(digest(content) != manifest['files'][name] for name, content in data.items()):
        raise OperationError('Journal checkpoint content differs from its manifest')
    state = json.loads(data['selection.json'])
    if (state['source'] != manifest['source'] or state['binary_sha256'] != digest(data['journaldevserver'])
            or state['cost_events'] != validate_cost(data['cost-events.json'])):
        raise OperationError('Journal checkpoint selection or accounting is inconsistent')
    return data


def verify_recovery(archive, hashes):
    if set(hashes) != {CHECKPOINT, ENVELOPE}:
        raise OperationError('Journal recovery checkpoint is incomplete')
    for name, expected in hashes.items():
        try:
            member = archive.getmember('recovery/journal-development/' + name)
        except KeyError as error:
            raise OperationError('Journal recovery checkpoint is missing') from error
        if not member.isfile() or digest(archive.extractfile(member).read()) != expected:
            raise OperationError('Journal recovery differs from its authenticated manifest')


def verify_runtime_recovery(runtime, directory, filename):
    data = read_checkpoint(runtime, directory)
    selected = json.loads(data['selection.json'])
    original = json.loads(data['cost-events.json']) or []
    container = 'journal-recovery-' + secrets.token_hex(12)
    label = 'cn.tellyouwhat.journal-recovery=' + container
    with tempfile.TemporaryDirectory(prefix='journal-recovery-', dir=runtime.state) as temporary:
        root = Path(temporary)
        config, state = root / 'config', root / 'state'
        config.mkdir(mode=0o700)
        state.mkdir(mode=0o700)
        for path, content, mode in [(root / 'journaldevserver', data['journaldevserver'], 0o555),
                                    (config / 'prompt.json', data['prompt.json'], 0o600),
                                    (state / 'cost-events.json', data['cost-events.json'], 0o600)]:
            path.write_bytes(content)
            path.chmod(mode)
        try:
            runtime.execute('journal-recovery-ownership', ['sudo', '-n', 'chown', '-R', '65532:65532', str(config), str(state)])
            result = json.loads(runtime.execute('journal-recovery-run', [
                'docker', 'run', '--rm', '--name', container, '--label', label, '--network', 'none', '--read-only',
                '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true', '--memory', '192m', '--pids-limit', '64',
                '--user', '65532:65532', '--env', 'JOURNAL_PROMPT_FILE=/restore/config/prompt.json',
                '--env', 'JOURNAL_DEVELOPMENT_STATE_DIR=/restore/state',
                '--env', 'JOURNAL_DEVELOPMENT_MONTHLY_BUDGET_CNY=' + str(selected['monthly_budget_cny']),
                '--mount', 'type=bind,source=' + str(root / 'journaldevserver') + ',target=/journaldevserver,readonly',
                '--mount', 'type=bind,source=' + str(config) + ',target=/restore/config,readonly',
                '--mount', 'type=bind,source=' + str(state) + ',target=/restore/state',
                '--entrypoint', '/journaldevserver', selected['image'], 'verify-recovery'], timeout=90))
            if (result.get('passed') is not True or result.get('cost_store_replayed') is not True
                    or result.get('prompt_revision') != json.loads(data['prompt.json'])['id']):
                raise OperationError('Journal runtime did not verify the recovered policy and ledger')
            after = json.loads(cost_snapshot(runtime, root)) or []
            if after[:len(original)] != original:
                raise OperationError('Journal recovery discarded previous spending events')
        finally:
            try:
                remaining = runtime.execute('journal-recovery-containers', ['docker', 'ps', '-aq', '--filter', 'label=' + label]).decode().split()
                if remaining:
                    runtime.execute('journal-recovery-stop', ['docker', 'rm', '--force', *remaining], timeout=30)
            finally:
                runtime.execute('journal-recovery-cleanup', ['sudo', '-n', 'rm', '-rf', '--', str(config), str(state)])
    return runtime.record('journal-development-restore', filename=filename, source=selected['source'],
                          passed=True, cost_events=len(original), original_ledger_prefix_retained=True,
                          prompt_revision=result['prompt_revision'], container_removed=True,
                          network_disabled=True, connection_credentials_absent=True, user_content_and_consents_excluded=True)
