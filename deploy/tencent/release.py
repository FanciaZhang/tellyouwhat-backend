#!/usr/bin/env python3
"""Deploy verified images and restore the previous runtime after activation failure."""

import argparse
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time

from ops_common import OperationError, Runtime, read_environment, compose_command


RUNTIME_FILES = [".env.production", "compose.production.yaml", "compose.ark-management.yaml", "deploy/single-server/Caddyfile",
                 "deploy/single-server/Caddyfile.external"]
KEY_FILES = ["health-subscription.p8", "journal-subscription.p8", "health-marketing.p8", "journal-marketing.p8"]


def copy_runtime(source, destination):
    source, destination = Path(source), Path(destination)
    for relative in RUNTIME_FILES + ["secrets/" + key for key in KEY_FILES]:
        origin = source / relative
        if origin.exists():
            target = destination / relative
            target.parent.mkdir(mode=0o750, parents=True, exist_ok=True)
            shutil.copy2(origin, target)
            if relative == ".env.production":
                target.chmod(0o600)


def snapshot(runtime, label):
    path = runtime.root / ".releases" / (label + "-" + secrets.token_hex(6))
    path.mkdir(mode=0o700, parents=True)
    copy_runtime(runtime.root, path)
    return str(path.relative_to(runtime.root))


def set_image(path, tag, registry):
    path = Path(path)
    lines = [line for line in path.read_text().splitlines() if not line.startswith(("IMAGE_TAG=", "IMAGE_REGISTRY_PREFIX="))]
    descriptor, temporary = tempfile.mkstemp(prefix=".release-env-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w") as output:
            output.write("\n".join([*lines, "IMAGE_TAG=" + tag, "IMAGE_REGISTRY_PREFIX=" + registry]) + "\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)


def validate(source, tag, registry, acceptance):
    if not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", tag) or not re.fullmatch(r"[A-Za-z0-9_./:-]{1,200}", registry):
        raise OperationError("invalid image tag or registry")
    if acceptance not in ("internal", "public"):
        raise OperationError("acceptance must be internal or public")
    for relative in [".env.production", *["secrets/" + key for key in KEY_FILES]]:
        path = source / relative
        if not path.is_file() or not path.stat().st_size:
            raise OperationError("required release configuration is missing")
    config = read_environment(source / ".env.production")
    if any(re.search(r"REPLACE_WITH_|ep-replace-|your-account/", value, re.I) for value in config.values()):
        raise OperationError("production environment contains placeholder values")
    for name in ("HEALTH_API_DOMAIN", "JOURNAL_API_DOMAIN", "ADMIN_DOMAIN"):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]", config.get(name, "")):
            raise OperationError("production application domains are required")
    if config.get("PUBLIC_PROXY_MODE", "docker") not in ("external", "docker"):
        raise OperationError("PUBLIC_PROXY_MODE must be docker or external")
    return config


def probe(url, host, status):
    with tempfile.TemporaryDirectory(prefix="tellyouwhat-readiness-") as directory:
        response = Path(directory) / "response.json"
        result = subprocess.run(["curl", "--fail", "--silent", "--show-error", "--connect-timeout", "2",
                                 "--max-time", "5", "--header", "Host: " + host, "--output", str(response),
                                 "--write-out", "%{http_code}", url], capture_output=True, timeout=10)
        if result.returncode or result.stdout.strip() != b"200":
            return False
        try:
            value = json.loads(response.read_text())
            return isinstance(value, dict) and value.get("status") == status
        except (ValueError, OSError):
            return False


def internally_ready(config, attempts, ports=(18080, 18081, 18082)):
    checks = [(f"http://127.0.0.1:{ports[0]}/readyz", config["HEALTH_API_DOMAIN"], "ready"),
              (f"http://127.0.0.1:{ports[0]}/readyz", config["JOURNAL_API_DOMAIN"], "ready"),
              (f"http://127.0.0.1:{ports[1]}/healthz", "localhost", "ok"),
              (f"http://127.0.0.1:{ports[2]}/readyz", config["ADMIN_DOMAIN"], "ready")]
    for attempt in range(attempts):
        if all(probe(*check) for check in checks):
            return True
        if attempt + 1 < attempts:
            time.sleep(5)
    return False


def activate(runtime, tag, registry):
    runtime.execute("release-activate", runtime.compose("up", "-d", "--no-build", "gateway", "worker", "admin"),
                    env={"IMAGE_TAG": tag, "IMAGE_REGISTRY_PREFIX": registry}, timeout=180)
    from blue_green import action, lifecycle_capable
    if lifecycle_capable(runtime, tag, registry):
        slot = {"slot": "legacy", "path": "."}
        for attempt in range(18):
            try:
                action(runtime, slot, "serve")
                action(runtime, slot, "resume")
                break
            except OperationError:
                if attempt == 17:
                    raise
                time.sleep(1)


def recover(runtime, previous, attempts, verify=True):
    path = (runtime.root / previous["snapshot"]).resolve()
    if not path.is_relative_to(runtime.root / ".releases"):
        raise OperationError("invalid rollback snapshot")
    config = validate(path, previous["tag"], previous["registry"], "internal")
    copy_runtime(path, runtime.root)
    set_image(runtime.environment_file, previous["tag"], previous["registry"])
    activate(runtime, previous["tag"], previous["registry"])
    if verify and not internally_ready(config, attempts):
        raise OperationError("restored version failed readiness verification")


def automation_version(runtime, tag, registry):
    services = ("gateway", "worker", "admin", "adminctl")
    images = [registry + "-" + service + ":" + tag for service in services]
    result = runtime.execute("release-capabilities", ["docker", "image", "inspect", "--format",
                             '{{ index .Config.Labels "cn.tellyouwhat.operations-automation" }}', *images])
    return 1 if result.decode().splitlines() == ["1"] * len(services) else 0


def require_automation_compatibility(current, target):
    if current.get("automationVersion", 0) > target.get("automationVersion", 0):
        raise OperationError("target release does not support the active automatic protection; use a compatible recovery image")


def deploy(runtime, tag, registry, acceptance, bundle, attempts):
    from blue_green import read_state, deploy as slot_deploy
    if read_state(runtime):
        return slot_deploy(runtime, tag, registry, acceptance, bundle, attempts)
    source = Path(bundle).resolve() if bundle else runtime.root
    if bundle and source != runtime.root / (".incoming-" + tag):
        raise OperationError("release bundle must match the requested image tag")
    config = validate(source, tag, registry, acceptance)
    env = {"IMAGE_TAG": tag, "IMAGE_REGISTRY_PREFIX": registry}
    candidate = compose_command(source, source / ".env.production")
    runtime.execute("release-config", candidate + ["config", "--quiet"], env=env)
    runtime.execute("release-pull", candidate + ["pull", "gateway", "worker", "admin", "adminctl", "migrate", "maintenance"],
                    env=env, timeout=900)
    version = automation_version(runtime, tag, registry)
    release_record = runtime.state / "release.json"
    if release_record.exists():
        require_automation_compatibility(json.loads(release_record.read_text()).get("current", {}),
                                         {"automationVersion": version})
    if acceptance == "public" and config.get("PUBLIC_PROXY_MODE", "docker") == "docker":
        runtime.execute("release-proxy-pull", candidate + ["pull", "caddy"], env=env, timeout=180)
    previous = None
    old = read_environment(runtime.environment_file) if runtime.environment_file.exists() else {}
    if old.get("IMAGE_TAG") and old.get("IMAGE_REGISTRY_PREFIX"):
        previous = {"tag": old["IMAGE_TAG"], "registry": old["IMAGE_REGISTRY_PREFIX"],
                    "snapshot": snapshot(runtime, "before-" + tag), "healthy": internally_ready(old, 1),
                    "automationVersion": automation_version(runtime, old["IMAGE_TAG"], old["IMAGE_REGISTRY_PREFIX"])}
    runtime.execute("release-migrate", candidate + ["run", "--rm", "--no-deps", "migrate"], env=env, timeout=300)
    activated = False
    try:
        activated = True
        if source != runtime.root:
            copy_runtime(source, runtime.root)
            target = runtime.root / "deploy/tencent"
            target.mkdir(parents=True, exist_ok=True)
            for script in (source / "deploy/tencent").iterdir():
                if script.is_file() and script.suffix in (".py", ".sh"):
                    shutil.copy2(script, target / script.name)
        set_image(runtime.environment_file, tag, registry)
        activate(runtime, tag, registry)
        if not internally_ready(config, attempts):
            raise OperationError("internal gateway, worker, or admin readiness failed")
        if acceptance == "public" and config.get("PUBLIC_PROXY_MODE", "docker") == "docker":
            runtime.execute("release-proxy", runtime.compose("up", "-d", "--no-build", "caddy"), env=env)
        current = {"tag": tag, "registry": registry, "snapshot": snapshot(runtime, "healthy-" + tag), "healthy": True, "automationVersion": version}
        result = runtime.record("release", current=current, previous=previous if previous and previous["healthy"] else None,
                                acceptance=acceptance)
        if bundle:
            shutil.rmtree(source)
        return {"passed": True, **result}
    except Exception as error:
        recovered = False
        if activated and previous:
            recover(runtime, previous, attempts, verify=previous["healthy"])
            recovered = previous["healthy"]
        runtime.record("release_failure", tag=tag, previous_restored=recovered)
        raise OperationError("deployment failed; " + ("previous healthy version restored" if recovered else "inspect private release diagnostics")) from error


def rollback(runtime, attempts):
    from blue_green import read_state, rollback as slot_rollback
    if read_state(runtime):
        return slot_rollback(runtime, attempts)
    record = json.loads((runtime.state / "release.json").read_text())
    previous = record.get("previous")
    if not previous:
        raise OperationError("no verified previous release is available")
    current = record["current"]
    require_automation_compatibility(current, previous)
    try:
        recover(runtime, previous, attempts)
    except Exception:
        recover(runtime, current, attempts)
        raise
    return {"passed": True, **runtime.record("release", current=previous, previous=current, acceptance="internal", rolled_back=True)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["deploy", "rollback", "bootstrap", "maintain", "recover"])
    parser.add_argument("--backend-dir", default=os.getenv("TELLYOUWHAT_BACKEND_DIR", "/opt/tellyouwhat/backend"))
    parser.add_argument("--tag")
    parser.add_argument("--registry")
    parser.add_argument("--acceptance", default="internal")
    parser.add_argument("--bundle")
    args = parser.parse_args()
    os.umask(0o077)
    try:
        attempts = int(os.getenv("READINESS_ATTEMPTS", "18"))
        if attempts < 1:
            raise OperationError("READINESS_ATTEMPTS must be positive")
        runtime = Runtime(args.backend_dir)
        with runtime.lock("deployment"), runtime.lock():
            runtime.refresh()
            if args.action == "deploy":
                if not args.tag or not args.registry:
                    raise OperationError("image tag and registry are required")
                result = deploy(runtime, args.tag, args.registry, args.acceptance, args.bundle, attempts)
            elif args.action == "rollback":
                result = rollback(runtime, attempts)
            else:
                import blue_green
                result = blue_green.adopt(runtime) if args.action == "bootstrap" else getattr(blue_green, args.action)(runtime, attempts)
        print(json.dumps(result, sort_keys=True))
        return 0
    except OperationError as error:
        if args.action == "maintain" and str(error).startswith("another "):
            print(json.dumps({"passed": True, "action": "maintain", "skipped": "operation lock held"}))
            return 0
        print(json.dumps({"passed": False, "action": args.action, "error": str(error)}))
        return 1
    except (OSError, ValueError, subprocess.TimeoutExpired) as error:
        print(json.dumps({"passed": False, "action": args.action, "error": str(error)}))
        return 1
    finally:
        if args.action == "deploy":
            subprocess.run(["docker", "logout", "ghcr.io"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    sys.exit(main())
