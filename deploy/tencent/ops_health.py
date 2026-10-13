"""Check live dependencies and the freshness of scheduled operations."""

import json
from datetime import datetime, timezone
from pathlib import Path
import shutil
import time

from release import probe
from ops_common import OperationError


def backup_valid(runtime, record):
    from ops_backup import verify_backup
    from ops_offsite import config_path, enabled
    filename = record["filename"]
    if Path(filename).name != filename:
        return False
    manifest = verify_backup(runtime, runtime.backups / filename)
    if manifest["sha256"] != record["sha256"]:
        return False
    if enabled(runtime):
        if not config_path(runtime).is_file():
            return False
        remote = json.loads((runtime.state / "offsite.json").read_text())
        return (remote.get("passed") is True and remote.get("filename") == filename
                and remote.get("sha256") == manifest["sha256"] and remote.get("verified_objects") == 3
                and remote.get("recovery_filename") == manifest.get("recovery_filename"))
    return True


def health(runtime):
    checks = []
    ports = (int(runtime.config.get("GATEWAY_HOST_PORT", "18080")),
             int(runtime.config.get("WORKER_HOST_PORT", "18081")),
             int(runtime.config.get("ADMIN_HOST_PORT", "18082")))
    from blue_green import read_state
    deployment = read_state(runtime)
    if deployment:
        checks.append({"name": "deployment_state", "passed": deployment["phase"] in ("STABLE", "DRAINING")})
        try:
            record = json.loads((runtime.state / "deployment_health.json").read_text())
            fresh = record.get("passed") is True and 0 <= time.time() - record["completed_at"] <= 90
        except (OSError, ValueError, KeyError):
            fresh = False
        checks.append({"name": "deployment_controller", "passed": fresh})
    for name, url, host, status in [
        ("health_gateway", f"http://127.0.0.1:{ports[0]}/readyz", runtime.config["HEALTH_API_DOMAIN"], "ready"),
        ("journal_gateway", f"http://127.0.0.1:{ports[0]}/readyz", runtime.config["JOURNAL_API_DOMAIN"], "ready"),
        ("worker", f"http://127.0.0.1:{ports[1]}/healthz", "localhost", "ok"),
        ("admin", f"http://127.0.0.1:{ports[2]}/readyz", runtime.config["ADMIN_DOMAIN"], "ready"),
    ]:
        checks.append({"name": name, "passed": probe(url, host, status)})
    disk = shutil.disk_usage(runtime.root)
    checks.append({"name": "disk_space", "passed": disk.free >= max(2 * 1024**3, disk.total * 0.15),
                   "free_megabytes": disk.free // 1024**2})
    now = time.time()
    for name, maximum_age in [("backup", 36 * 3600), ("maintenance", 30 * 3600), ("restore", 8 * 86400)]:
        valid, age = False, None
        try:
            record = json.loads((runtime.state / (name + ".json")).read_text())
            age = int(now - record["completed_at"])
            valid = 0 <= age <= maximum_age and record.get("passed", True) is True
            failure = runtime.state / (name + "-failure.json")
            if failure.exists():
                valid = valid and json.loads(failure.read_text())["completed_at"] < record["completed_at"]
            if name == "backup":
                valid = valid and backup_valid(runtime, record)
        except (OperationError, OSError, ValueError, TypeError, KeyError):
            valid = False
        checks.append({"name": name + "_freshness", "passed": valid, "age_seconds": age})
    return runtime.record("health", passed=all(check["passed"] for check in checks), checks=checks)


def report_health(runtime, result):
    payload = {"checkedAt": datetime.fromtimestamp(result["completed_at"], timezone.utc).isoformat(),
               "checks": result["checks"]}
    runtime.execute("health-report", runtime.compose("run", "--rm", "--no-deps", "-T", "adminctl", "operations-health"),
                    input=json.dumps(payload).encode(), timeout=60)
