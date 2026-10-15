"""Reclaim only expired Apple development installation artifacts."""

import json
import os
from pathlib import Path
import re

from ops_common import OperationError


def cleanup_packages(runtime):
    state_file = runtime.state / "apple-delivery.json"
    owner_file = runtime.state / "artifact-owners/apple-development.json"
    if not state_file.exists():
        if owner_file.exists():
            raise OperationError("Apple delivery recovery state is missing")
        return {"configured": False}
    if state_file.is_symlink() or state_file.stat().st_size > 16 * 1024:
        raise OperationError("invalid Apple delivery recovery state")
    state = json.loads(state_file.read_text())
    if not isinstance(state, dict):
        raise OperationError("invalid Apple delivery recovery state")
    image = state.get("image", "")
    if not re.fullmatch(r"tellyouwhat-apple-development:[0-9a-f]{40}", image):
        raise OperationError("Apple delivery image is outside its owner scope")
    if owner_file.is_symlink() or not owner_file.is_file() or owner_file.stat().st_size > 16 * 1024:
        raise OperationError("Apple delivery owner registration is missing")
    owner = json.loads(owner_file.read_text())
    if not isinstance(owner, dict) or not isinstance(owner.get("current"), dict) or image not in owner["current"].get("images", []):
        raise OperationError("Apple delivery cleanup is not bound to the current registration")
    data = runtime.root / ".apple-delivery/data"
    operator = runtime.root.stat()
    if ((runtime.root / ".apple-delivery").is_symlink() or data.is_symlink() or not data.is_dir()
            or not data.resolve().is_relative_to(runtime.root) or data.stat().st_uid != operator.st_uid
            or data.stat().st_mode & 0o077 or os.geteuid() != operator.st_uid):
        raise OperationError("Apple delivery store must be private and operator owned")
    output = runtime.execute("apple-delivery-cleanup", [
        "docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges:true", "--memory", "64m", "--pids-limit", "64",
        "--user", f"{operator.st_uid}:{operator.st_gid}",
        "--mount", f"type=bind,source={data},target=/data", image, "cleanup", "--data", "/data"
    ], timeout=120)
    result = json.loads(output)
    if (not isinstance(result, dict) or set(result) != {"grants_removed", "packages_removed"}
            or any(type(n) is not int or n < 0 for n in result.values())):
        raise OperationError("invalid Apple delivery cleanup result")
    return {"configured": True, **result}
