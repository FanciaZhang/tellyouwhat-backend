"""Check live dependencies and the freshness of scheduled operations."""

import json
from pathlib import Path
import shutil
import time

from release import probe


def health(runtime):
    checks = []
    for name, url, host, status in [
        ("health_gateway", "http://127.0.0.1:18080/readyz", runtime.config["HEALTH_API_DOMAIN"], "ready"),
        ("journal_gateway", "http://127.0.0.1:18080/readyz", runtime.config["JOURNAL_API_DOMAIN"], "ready"),
        ("worker", "http://127.0.0.1:18081/healthz", "localhost", "ok"),
        ("admin", "http://127.0.0.1:18082/readyz", runtime.config["ADMIN_DOMAIN"], "ready"),
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
            valid = 0 <= age <= maximum_age
            if name == "backup":
                filename = record["filename"]
                valid = valid and Path(filename).name == filename and (runtime.backups / filename).is_file()
        except (OSError, ValueError, TypeError, KeyError):
            pass
        checks.append({"name": name + "_freshness", "passed": valid, "age_seconds": age})
    return runtime.record("health", passed=all(check["passed"] for check in checks), checks=checks)
