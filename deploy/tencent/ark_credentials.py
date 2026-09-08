#!/usr/bin/env python3
"""Stage an admin-only Ark credential without exposing it to shared app mounts."""

import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile

from ops_common import OperationError, read_environment


def validate_credential(raw):
    try:
        value = json.loads(raw)
    except (ValueError, UnicodeError):
        raise OperationError("invalid Ark service credential JSON") from None
    if (len(raw) > 8192 or not isinstance(value, dict)
            or set(value) != {"accessKey", "secretKey"}
            or any(not isinstance(item, str) or not item.strip() for item in value.values())):
        raise OperationError("invalid Ark service credential fields")


def materialize(output):
    raw = os.environ.get("ARK_SERVICE_CREDENTIALS", "").encode()
    output = Path(output)
    if not raw:
        output.unlink(missing_ok=True)
        return
    validate_credential(raw)
    descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as handle:
        handle.write(raw)


def install(stage, backend_dir):
    stage, root = Path(stage).resolve(), Path(backend_dir).resolve()
    if stage.parent != root or not stage.name.startswith(".incoming-"):
        raise OperationError("Ark credential stage must be an incoming release")
    source = stage / "ark-management.json"
    environment = stage / ".env.production"
    config = read_environment(environment)
    if not source.exists():
        if (config.get("ARK_MANAGEMENT_CREDENTIAL_HOST_FILE") or config.get("ARK_MANAGEMENT_CREDENTIAL_FILE")
                or config.get("AI_ENDPOINT_WRITES_ENABLED", "false").lower() == "true"):
            raise OperationError("ARK_MANAGEMENT_CREDENTIALS_JSON is required for this release")
        return
    if source.is_symlink() or source.stat().st_mode & 0o077:
        raise OperationError("Ark credential staging file must have private permissions")
    try:
        validate_credential(source.read_bytes())
        directory = root / ".ark-management"
        target = directory / (secrets.token_hex(16) + ".json")
        # Versioned files are retained for release rollback. No shared /secrets mount.
        subprocess.run(["sudo", "-n", "install", "-d", "-m", "0711", "-o", "root", "-g", "root", str(directory)], check=True, capture_output=True)
        subprocess.run(["sudo", "-n", "install", "-m", "0600", "-o", "65532", "-g", "65532", str(source), str(target)], check=True, capture_output=True)
        lines = [line for line in environment.read_text().splitlines()
                 if not line.startswith(("ARK_MANAGEMENT_CREDENTIAL_FILE=", "ARK_MANAGEMENT_CREDENTIAL_HOST_FILE="))]
        lines += ["ARK_MANAGEMENT_CREDENTIAL_HOST_FILE=" + str(target),
                  "ARK_MANAGEMENT_CREDENTIAL_FILE=/run/ark-management/credentials.json"]
        descriptor, temporary = tempfile.mkstemp(prefix=".ark-env-", dir=stage)
        try:
            with os.fdopen(descriptor, "w") as output:
                output.write("\n".join(lines) + "\n")
                output.flush()
                os.fsync(output.fileno())
            os.replace(temporary, environment)
        finally:
            Path(temporary).unlink(missing_ok=True)
    finally:
        source.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    material = sub.add_parser("materialize")
    material.add_argument("--output", required=True)
    installer = sub.add_parser("install")
    installer.add_argument("--stage", required=True)
    installer.add_argument("--backend-dir", required=True)
    args = parser.parse_args()
    try:
        if args.action == "materialize":
            materialize(args.output)
        else:
            install(args.stage, args.backend_dir)
        return 0
    except (OperationError, OSError, subprocess.CalledProcessError):
        print("Ark credential preparation failed; verify the protected secret, file permissions, and deployment configuration.")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
