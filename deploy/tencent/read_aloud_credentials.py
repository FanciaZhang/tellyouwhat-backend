#!/usr/bin/env python3
"""Keep production Journal speech credentials private and bound to each release."""

import argparse
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess

from ops_common import OperationError, atomic_text, read_environment

CONTAINER_FILE = '/run/journal-read-aloud/api-key.txt'


def validate_key(raw):
    key = raw.strip()
    if not 1 <= len(key) <= 4096 or any(c < 33 or c > 126 for c in key):
        raise OperationError('invalid Journal TTS credential')
    return key


def materialize(output):
    raw = os.environ.get('JOURNAL_TTS_API_KEY', '').encode()
    if not raw:
        Path(output).unlink(missing_ok=True)
        return
    key = validate_key(raw)
    with os.fdopen(os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'wb') as handle:
        handle.write(key)


def validate_config(config):
    for name in ('JOURNAL_TTS_ENABLED', 'JOURNAL_TTS_WRITES_ENABLED'):
        if config.get(name, '') not in ('', 'true', 'false'):
            raise OperationError('invalid Journal TTS enable flag')
    required = any(config.get(k) == 'true' for k in ('JOURNAL_TTS_ENABLED', 'JOURNAL_TTS_WRITES_ENABLED'))
    host = config.get('JOURNAL_TTS_API_KEY_HOST_FILE', '')
    target = config.get('JOURNAL_TTS_API_KEY_FILE', '')
    if not host and not target and not required:
        return
    if config.get('APP_ENV', 'production') != 'production' or target != CONTAINER_FILE:
        raise OperationError('Journal TTS requires the production credential mount')
    path = Path(host)
    if not path.is_absolute() or path.parent.name != '.journal-read-aloud' or not re.fullmatch(r'[0-9a-f]{32}\.txt', path.name):
        raise OperationError('Journal TTS requires a versioned production credential')
    if any(parent.is_symlink() for parent in path.parents):
        raise OperationError('Journal TTS credential ancestors cannot be symlinks')
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 65532 or stat.S_IMODE(info.st_mode) != 0o600 or not 0 < info.st_size <= 4096:
        raise OperationError('Journal TTS credential must be a private regular file owned by UID 65532')
    fields = [('JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS', 1000, 1_000_000),
              ('JOURNAL_TTS_PROBE_DAILY_CHARACTERS', 33_333, 100_000_000)]
    if config.get('JOURNAL_TTS_ENABLED') == 'true':
        fields.append(('JOURNAL_TTS_MONTHLY_CHARACTERS', 0, 100_000_000))
    for name, default, maximum in fields:
        try:
            valid = 1 <= int(config.get(name, str(default))) <= maximum
        except ValueError:
            valid = False
        if not valid:
            raise OperationError('Journal TTS requires bounded character allowances')


def install(stage, backend_dir):
    stage, root = Path(stage).resolve(), Path(backend_dir).resolve()
    if stage.parent != root or not stage.name.startswith('.incoming-'):
        raise OperationError('Journal TTS credential stage must be an incoming release')
    source, environment = stage / 'journal-tts-api-key.txt', stage / '.env.production'
    try:
        config = read_environment(environment)
        if not source.exists():
            validate_config(config)
            return
        if (source.is_symlink() or not source.is_file() or source.stat().st_mode & 0o077
                or not 0 < source.stat().st_size <= 4096):
            raise OperationError('Journal TTS staging credential must be private')
        validate_key(source.read_bytes())
        directory = root / '.journal-read-aloud'
        if directory.is_symlink():
            raise OperationError('Journal TTS credential directory cannot be a symlink')
        target = directory / (secrets.token_hex(16) + '.txt')
        # Immutable credentials stay available to current and rollback configurations.
        subprocess.run(['sudo', '-n', 'install', '-d', '-m', '0711', '-o', 'root', '-g', 'root', str(directory)], check=True, capture_output=True)
        subprocess.run(['sudo', '-n', 'install', '-m', '0600', '-o', '65532', '-g', '65532', str(source), str(target)], check=True, capture_output=True)
        lines = [line for line in environment.read_text().splitlines()
                 if not line.startswith(('JOURNAL_TTS_API_KEY_HOST_FILE=', 'JOURNAL_TTS_API_KEY_FILE='))]
        atomic_text(environment, '\n'.join([*lines, 'JOURNAL_TTS_API_KEY_HOST_FILE=' + str(target),
                                         'JOURNAL_TTS_API_KEY_FILE=' + CONTAINER_FILE]) + '\n')
    finally:
        source.unlink(missing_ok=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='action', required=True)
    material = sub.add_parser('materialize')
    material.add_argument('--output', required=True)
    installer = sub.add_parser('install')
    installer.add_argument('--stage', required=True)
    installer.add_argument('--backend-dir', required=True)
    args = parser.parse_args()
    try:
        if args.action == 'materialize':
            materialize(args.output)
        else:
            install(args.stage, args.backend_dir)
    except (OperationError, OSError, subprocess.CalledProcessError):
        raise SystemExit('Journal TTS credential preparation failed; check its protected secret and production configuration.')
