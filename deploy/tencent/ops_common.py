"""Shared primitives for private, bounded server operations."""

import contextlib
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time


class OperationError(RuntimeError):
    pass


def read_environment(path):
    values = {}
    for number, source in enumerate(Path(path).read_text().splitlines(), 1):
        line = source.strip()
        if not line or line.startswith("#"):
            continue
        name, separator, value = line.partition("=")
        if not separator or not re.fullmatch(r"[A-Z][A-Z0-9_]*", name):
            raise OperationError(f"invalid environment assignment on line {number}")
        if name in values:
            raise OperationError(f"duplicate environment key: {name}")
        if len(value) >= 2 and value[0] in "\"'" and value[-1] == value[0]:
            value = value[1:-1]
        values[name] = value
    return values


def atomic_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor, temporary = tempfile.mkstemp(prefix=".pending-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w") as output:
            json.dump(value, output, sort_keys=True)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)


def compose_command(root, environment_file):
    root, environment_file = Path(root), Path(environment_file)
    config = read_environment(environment_file)
    command = ["docker", "compose", "--project-directory", str(root),
               "--env-file", str(environment_file), "-f", str(root / "compose.production.yaml")]
    credential = config.get("ARK_MANAGEMENT_CREDENTIAL_HOST_FILE", "")
    container_path = config.get("ARK_MANAGEMENT_CREDENTIAL_FILE", "")
    if credential:
        path = Path(credential)
        overlay = root / "compose.ark-management.yaml"
        if not path.is_absolute() or path.is_symlink() or not path.is_file():
            raise OperationError("Ark service credential must be an existing absolute regular file")
        info = path.stat()
        if info.st_uid != 65532 or info.st_mode & 0o077 or not 0 < info.st_size <= 8192:
            raise OperationError("Ark service credential must belong to UID 65532 with private permissions")
        if not overlay.is_file() or container_path != "/run/ark-management/credentials.json":
            raise OperationError("Ark service credential mount configuration is incomplete")
        command += ["-f", str(overlay)]
    elif container_path or config.get("AI_ENDPOINT_WRITES_ENABLED", "false").lower() == "true":
        raise OperationError("Ark endpoint writes require the isolated service credential mount")
    return command


class Runtime:
    def __init__(self, backend_dir, backup_dir=None, environment_file=None):
        self.root = Path(backend_dir).resolve()
        self.base_environment_file = Path(environment_file or self.root / ".env.production").resolve()
        self.state = self.root / ".operations"
        self.state.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.backups = Path(backup_dir or "/var/backups/tellyouwhat")
        self.refresh()

    def refresh(self):
        self.environment_file = self.base_environment_file
        self.compose_root = self.root
        state_path = self.state / "blue-green.json"
        if state_path.exists():
            current = json.loads(state_path.read_text())["current"]
            path = (self.root / current["path"]).resolve()
            if path != self.root and not path.is_relative_to(self.root / ".slots"):
                raise OperationError("invalid active slot path")
            self.compose_root = path
            self.environment_file = path / ".env.production"
        self.config = read_environment(self.environment_file)

    @contextlib.contextmanager
    def lock(self, name="operations"):
        descriptor = os.open(self.state / (name + ".lock"), os.O_CREAT | os.O_RDWR, 0o600)
        with os.fdopen(descriptor, "w") as handle:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise OperationError(f"another {name} operation is running") from error
            yield

    def execute(self, label, command, *, env=None, input=None, timeout=300):
        child_env = os.environ.copy()
        child_env.update(env or {})
        child_env["TELLYOUWHAT_ENV_FILE"] = str(self.environment_file)
        if "--env-file" in command:
            child_env["TELLYOUWHAT_ENV_FILE"] = command[command.index("--env-file") + 1]
        result = subprocess.run(command, input=input, capture_output=True, env=child_env, timeout=timeout)
        if result.returncode:
            # Tool errors can include SQL or credentials. Diagnostics stay on the server.
            diagnostic = self.state / (label + ".stderr")
            descriptor = os.open(diagnostic, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(descriptor, "wb") as output:
                output.write(result.stderr)
            raise OperationError(f"{label} failed (exit {result.returncode})")
        return result.stdout

    def compose(self, *arguments):
        return [*compose_command(self.compose_root, self.environment_file), *arguments]

    def record(self, name, **details):
        result = {"operation": name, "completed_at": int(time.time()), **details}
        atomic_json(self.state / (name + ".json"), result)
        return result
