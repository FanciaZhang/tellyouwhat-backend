"""Explicit, version-bound interruption after a refused blue-green capacity check."""

import hashlib
import json
from pathlib import Path
import re
import shutil
import time

import blue_green as bg
from ops_common import OperationError, atomic_json, compose_command, read_environment

OFFER = "interruption-offer.json"


class ConfirmationRequired(OperationError):
    pass


def clear_offer(runtime):
    (runtime.state / OFFER).unlink(missing_ok=True)


def bundle_fingerprint(runtime, source):
    from release import RUNTIME_FILES, KEY_FILES
    digest = hashlib.sha256()
    for name in sorted([*RUNTIME_FILES, *["secrets/" + key for key in KEY_FILES]]):
        path = source / name
        if name == ".env.production":
            config = read_environment(path)
            for key in ["IMAGE_TAG", "IMAGE_REGISTRY_PREFIX", *[s.upper() + "_IMAGE" for s in (*bg.SERVICES, "adminctl", "migrate", "maintenance")]]:
                config.pop(key, None)
            credential = config.get("ARK_MANAGEMENT_CREDENTIAL_HOST_FILE")
            if credential:
                path = Path(credential)
                if path.is_symlink() or not path.resolve().is_relative_to(runtime.root / ".ark-management"):
                    raise OperationError("invalid release credential path")
                # Install uses a fresh private filename on every upload. Bind the
                # credential bytes, so identical re-staging remains eligible.
                output = runtime.execute("slot-credential-fingerprint", ["sudo", "-n", "sha256sum", "--", str(path)])
                checksum = output.decode().split()[0]
                if not re.fullmatch(r"[0-9a-f]{64}", checksum):
                    raise OperationError("credential fingerprint is unavailable")
                config["ARK_MANAGEMENT_CREDENTIAL_HOST_FILE"] = "sha256:" + checksum
            content = json.dumps(config, sort_keys=True).encode()
        else:
            content = path.read_bytes() if path.exists() else b""
        digest.update(name.encode() + b"\0" + content + b"\0")
    return digest.hexdigest()


def capacity_check(runtime, target, source, tag, registry, acceptance, source_run, previous):
    try:
        bg.resource_check(runtime, target)
    except bg.CapacityError as error:
        if not re.fullmatch(r"[0-9a-f]{40}", tag) or not re.fullmatch(r"[0-9]+", source_run):
            raise
        atomic_json(runtime.state / OFFER, {"tag": tag, "registry": registry, "acceptance": acceptance,
                    "source_run": source_run, "previous": previous, "bundle": bundle_fingerprint(runtime, source),
                    "created_at": int(time.time()), "reason": str(error)})
        raise ConfirmationRequired("Blue-green capacity check failed. Current services remain active. "
            "Run Backend Disruptive Deployment only after separately confirming interruption for this SHA and source run; "
            "active requests, voice sessions and tasks may be interrupted.") from error


def require_offer(runtime, tag, registry, acceptance, source, source_run, confirmation):
    if not re.fullmatch(r"[0-9a-f]{40}", tag) or confirmation != "interrupt:" + tag:
        raise OperationError("explicit interruption confirmation must match the full target SHA")
    path = runtime.state / OFFER
    if not path.exists():
        raise OperationError("no capacity refusal is available for manual interruption")
    offer = json.loads(path.read_text())
    state = bg.read_state(runtime)
    if (not state or state["phase"] != "STABLE" or state["current"] != offer["previous"]
            or any(offer[key] != value for key, value in {
                "tag": tag, "registry": registry, "acceptance": acceptance,
                "source_run": source_run, "bundle": bundle_fingerprint(runtime, source)}.items())
            or not 0 <= time.time() - offer["created_at"] <= 86400):
        raise OperationError("capacity refusal is stale or does not match the version, configuration and active slot")
    return state


def stop(runtime, slot):
    # This path is authorized to interrupt in-flight work after the grace period.
    bg.action(runtime, slot, "pause", partial=True)
    runtime.execute("slot-interrupt-stop", bg.compose(runtime, slot, "stop", "--timeout", "30", *bg.SERVICES), timeout=120)
    if bg.running(runtime, slot):
        raise OperationError("interrupted slot did not stop; another instance must not start")


def start(runtime, slot, attempts):
    runtime.execute("slot-start", bg.compose(runtime, slot, "up", "-d", "--no-build", *bg.SERVICES), timeout=180)
    bg.require_ready(runtime, slot, attempts)
    bg.statuses(runtime, slot)


def recover(runtime, state, attempts):
    target, candidate = state["recovery_target"], state["candidate"]
    stop(runtime, candidate)
    start(runtime, target, attempts)
    bg.action(runtime, target, "serve")
    revision = bg.switch_proxy(runtime, target)
    bg.action(runtime, target, "resume")
    bg.require_ready(runtime, target, attempts)
    bg.write_state(runtime, state, phase="STABLE", current=target, previous=candidate,
                   candidate=None, recovery_target=None, strategy=None, proxyRevision=revision)
    bg.publish(runtime, state)
    return {"passed": True, "deployment": "STABLE", "previous_restored": True}


def deploy(runtime, tag, registry, acceptance, bundle, attempts, source_run, confirmation):
    from release import validate
    source = Path(bundle).resolve() if bundle else None
    if source != runtime.root / (".incoming-" + tag):
        raise OperationError("manual release requires its immutable staged bundle")
    state = require_offer(runtime, tag, registry, acceptance, source, source_run, confirmation)
    config = validate(source, tag, registry, acceptance)
    if config.get("PUBLIC_PROXY_MODE") != "external":
        raise OperationError("native Caddy is required")
    previous = state["current"]
    bg.assert_route(previous)
    bg.require_ready(runtime, previous, 1)
    bg.storage_check(runtime)
    bg.environment_update(source / ".env.production", {"IMAGE_TAG": tag, "IMAGE_REGISTRY_PREFIX": registry,
                          **{s.upper() + "_IMAGE": "" for s in (*bg.SERVICES, "adminctl", "migrate", "maintenance")}})
    runtime.execute("slot-pull", [*compose_command(source, source / ".env.production"), "pull",
                    *bg.SERVICES, "adminctl", "migrate", "maintenance"], timeout=900)
    bg.storage_check(runtime)
    target = "green" if previous["slot"] in ("legacy", "blue") else "blue"
    candidate = bg.prepare(runtime, source, target, tag, registry)
    bg.validate_slot_config(runtime, candidate)
    # Reject occupied target ports before stopping any active service.
    for port in bg.PORTS[target]:
        with bg.socket.socket() as listener:
            try:
                listener.bind(("127.0.0.1", port))
            except OSError as error:
                raise OperationError("candidate slot port is occupied") from error
    clear_offer(runtime)
    bg.write_state(runtime, state, phase="INTERRUPTING", strategy="disruptive",
                   recovery_target=previous, candidate=candidate)
    try:
        stop(runtime, previous)
        bg.require_memory_capacity(dual=False)
        runtime.execute("slot-migrate", bg.compose(runtime, candidate, "run", "--rm", "--no-deps", "migrate"), timeout=300)
        start(runtime, candidate, attempts)
        values = bg.statuses(runtime, candidate)
        if any(v["backgroundEnabled"] or v["httpEnabled"] for v in values.values()):
            raise OperationError("candidate did not start in standby")
        bg.action(runtime, candidate, "serve")
        revision = bg.switch_proxy(runtime, candidate)
        bg.action(runtime, candidate, "resume")
        bg.require_ready(runtime, candidate, attempts)
        candidate["healthy"] = True
        bg.write_state(runtime, state, phase="STABLE", current=candidate, previous=previous,
                       candidate=None, recovery_target=None, strategy=None, proxyRevision=revision)
        bg.publish(runtime, state)
    except Exception as error:
        bg.write_state(runtime, state, phase="RECOVERY_REQUIRED", strategy="disruptive",
                       recovery_target=previous, candidate=candidate)
        try:
            recover(runtime, state, attempts)
        except Exception:
            raise OperationError("interrupted deployment failed; explicit recovery is required") from error
        raise OperationError("interrupted deployment failed; previous version restarted without restoring the database") from error
    for script in (source / "deploy/tencent").iterdir():
        if script.is_file() and script.suffix in (".py", ".sh"):
            shutil.copy2(script, runtime.root / "deploy/tencent" / script.name)
    shutil.rmtree(source)
    return {"passed": True, "deployment": "STABLE", "strategy": "disruptive", "current": candidate}
