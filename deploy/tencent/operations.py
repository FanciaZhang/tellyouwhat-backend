#!/usr/bin/env python3
"""Execute bounded maintenance operations for the shared backend."""

import argparse
import json
import os
import subprocess
import sys

from ops_backup import create_backup, restore_drill
from ops_common import OperationError, Runtime
from ops_health import health


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["health", "backup", "restore", "maintenance", "providers"])
    parser.add_argument("--backend-dir", default=os.getenv("TELLYOUWHAT_BACKEND_DIR", "/opt/tellyouwhat/backend"))
    parser.add_argument("--backup-dir", default=os.getenv("TELLYOUWHAT_BACKUP_DIR"))
    parser.add_argument("--environment-file", default=os.getenv("TELLYOUWHAT_ENV_FILE"))
    parser.add_argument("--backup", help="backup filename used only by an isolated restore drill")
    args = parser.parse_args()
    os.umask(0o077)
    try:
        runtime = Runtime(args.backend_dir, args.backup_dir, args.environment_file)
        if args.operation == "health":
            result = health(runtime)
        else:
            with runtime.lock():
                result = operate(runtime, args)
        print(json.dumps({"passed": True, **result}, sort_keys=True))
        return 0 if result.get("passed", True) else 1
    except subprocess.TimeoutExpired:
        print(json.dumps({"operation": args.operation, "passed": False, "error": "operation exceeded its timeout"}))
        return 1
    except (OperationError, OSError, TimeoutError, KeyError) as error:
        print(json.dumps({"operation": args.operation, "passed": False, "error": str(error)}))
        return 1


def operate(runtime, args):
    if args.operation == "backup":
        return create_backup(runtime)
    if args.operation == "restore":
        return restore_drill(runtime, args.backup)
    if args.operation == "providers":
        output = runtime.execute("providers", runtime.compose("run", "--rm", "--no-deps", "--entrypoint", "/servicecheck",
                                 "gateway", "--models"), timeout=600)
        checks = [json.loads(line) for line in output.splitlines() if line.strip()]
        return runtime.record("providers", checks=checks)
    runtime.execute("maintenance", runtime.compose("run", "--rm", "--no-deps", "maintenance"), timeout=900)
    return runtime.record("maintenance")


if __name__ == "__main__":
    sys.exit(main())
