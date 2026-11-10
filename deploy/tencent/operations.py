#!/usr/bin/env python3
"""Execute bounded maintenance operations for the shared backend."""

import argparse
import json
import os
import re
import subprocess
import sys

from ops_backup import create_backup, prune_backups, restore_drill
from ops_artifacts import OWNERS, cleanup
from ops_common import OperationError, Runtime
from ops_health import health, report_health


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["health", "backup", "restore", "maintenance", "providers", "artifacts"])
    parser.add_argument("--backend-dir", default=os.getenv("TELLYOUWHAT_BACKEND_DIR", "/opt/tellyouwhat/backend"))
    parser.add_argument("--backup-dir", default=os.getenv("TELLYOUWHAT_BACKUP_DIR"))
    parser.add_argument("--environment-file", default=os.getenv("TELLYOUWHAT_ENV_FILE"))
    parser.add_argument("--backup", help="backup filename used only by an isolated restore drill")
    parser.add_argument('--albums-checkpoint', nargs=3, action='append', metavar=('ENVIRONMENT', 'FILE', 'SHA256'),
                        help='apply an independently pinned Albums deletion checkpoint during isolated restore')
    parser.add_argument('--verify-albums-checkpoint-source', action='store_true',
                        help='compare selected checkpoints around Albums replay and again after the shared SQL drill')
    parser.add_argument("--owner", choices=OWNERS)
    args = parser.parse_args()
    if args.albums_checkpoint and args.operation != 'restore':
        parser.error('--albums-checkpoint is only supported by restore')
    if args.verify_albums_checkpoint_source and (args.operation != 'restore' or not args.albums_checkpoint):
        parser.error('--verify-albums-checkpoint-source requires restore and --albums-checkpoint')
    os.umask(0o077)
    try:
        runtime = Runtime(args.backend_dir, args.backup_dir, args.environment_file)
        if args.operation == "health":
            with runtime.lock("health"):
                result = health(runtime)
                report_health(runtime, result)
        else:
            with runtime.lock(wait_seconds=600):
                runtime.refresh()
                try:
                    result = operate(runtime, args)
                except (OperationError, OSError, TimeoutError, KeyError, ValueError, subprocess.TimeoutExpired):
                    runtime.record(args.operation + "-failure", passed=False)
                    raise
                (runtime.state / (args.operation + "-failure.json")).unlink(missing_ok=True)
        print(json.dumps({"passed": True, **result}, sort_keys=True))
        return 0 if result.get("passed", True) else 1
    except subprocess.TimeoutExpired:
        print(json.dumps({"operation": args.operation, "passed": False, "error": "operation exceeded its timeout"}))
        return 1
    except (OperationError, OSError, TimeoutError, KeyError, ValueError) as error:
        print(json.dumps({"operation": args.operation, "passed": False, "error": str(error)}))
        return 1


def operate(runtime, args):
    if args.operation == "artifacts":
        return cleanup(runtime, args.owner, cache=not args.owner)
    if args.operation == "backup":
        return create_backup(runtime)
    if args.operation == "restore":
        checkpoints = {}
        for environment, path, digest in getattr(args, 'albums_checkpoint', None) or []:
            if (environment not in ('development', 'production') or environment in checkpoints
                    or not path or not re.fullmatch('[a-f0-9]{64}', digest)):
                raise OperationError('invalid or duplicate Albums checkpoint selection')
            checkpoints[environment] = (path, digest)
        return restore_drill(runtime, args.backup, albums_checkpoints=checkpoints or None,
                             verify_checkpoint_source=getattr(args, 'verify_albums_checkpoint_source', False))
    if args.operation == "providers":
        output = runtime.execute("providers", runtime.compose("run", "--rm", "--no-deps", "--entrypoint", "/servicecheck",
                                 "gateway", "--models"), timeout=600)
        checks = [json.loads(line) for line in output.splitlines() if line.strip()]
        return runtime.record("providers", checks=checks)
    errors = []
    removed = 0
    try:
        removed = prune_backups(runtime)
    except (OperationError, OSError) as error:
        errors.append(str(error))
    try:
        runtime.execute("maintenance", runtime.compose("run", "--rm", "--no-deps", "maintenance"), timeout=900)
    except (OperationError, OSError) as error:
        errors.append(str(error))
    from platform_test_maintenance import maintain
    platform_test = None
    try:
        platform_test = maintain(runtime)
    except (OperationError, OSError, KeyError, ValueError) as error:
        errors.append(str(error))
    try:
        cleanup(runtime, cache=True)
    except (OperationError, OSError) as error:
        errors.append(str(error))
    if errors:
        raise OperationError("; ".join(errors))
    return runtime.record("maintenance", expired_backups_removed=removed, platform_test=platform_test)


if __name__ == "__main__":
    sys.exit(main())
