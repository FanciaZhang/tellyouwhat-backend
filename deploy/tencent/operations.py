#!/usr/bin/env python3
"""Execute bounded maintenance operations for the shared backend."""

import argparse
import json
import os
import subprocess
import sys

from ops_backup import create_backup, restore_drill
from ops_common import OperationError, Runtime


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["backup", "restore", "maintenance"])
    parser.add_argument("--backend-dir", default=os.getenv("TELLYOUWHAT_BACKEND_DIR", "/opt/tellyouwhat/backend"))
    parser.add_argument("--backup-dir", default=os.getenv("TELLYOUWHAT_BACKUP_DIR"))
    parser.add_argument("--environment-file", default=os.getenv("TELLYOUWHAT_ENV_FILE"))
    parser.add_argument("--backup", help="backup filename used only by an isolated restore drill")
    args = parser.parse_args()
    os.umask(0o077)
    try:
        runtime = Runtime(args.backend_dir, args.backup_dir, args.environment_file)
        with runtime.lock():
            if args.operation == "backup":
                result = create_backup(runtime)
            elif args.operation == "restore":
                result = restore_drill(runtime, args.backup)
            else:
                runtime.execute("maintenance", runtime.compose("run", "--rm", "--no-deps", "maintenance"), timeout=900)
                result = runtime.record("maintenance")
        print(json.dumps({"passed": True, **result}, sort_keys=True))
    except subprocess.TimeoutExpired:
        print(json.dumps({"operation": args.operation, "passed": False, "error": "operation exceeded its timeout"}))
        return 1
    except (OperationError, OSError, TimeoutError) as error:
        print(json.dumps({"operation": args.operation, "passed": False, "error": str(error)}))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
