"""Single-host release slots; all mutating callers hold the deployment and operations locks."""

import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import socket
import time
import urllib.request

from ops_common import OperationError, atomic_json, compose_command, read_environment

SERVICES = ("gateway", "worker", "admin")
PORTS = {"legacy": (18080, 18081, 18082), "blue": (18080, 18081, 18082), "green": (18180, 18181, 18182)}
STATE = "blue-green.json"
PROXY_FILE = Path("/etc/caddy/tellyouwhat-backend.caddy")
PROXY_ROOT = Path("/etc/caddy/Caddyfile")
PROXY_API = "http://127.0.0.1:2019/config/"


def read_state(runtime):
    path = runtime.state / STATE
    return json.loads(path.read_text()) if path.exists() else None


def write_state(runtime, state, **changes):
    state.update(changes)
    state["updated_at"] = int(time.time())
    atomic_json(runtime.state / STATE, state)


def slot_path(runtime, slot):
    path = (runtime.root / slot["path"]).resolve()
    if slot["slot"] == "legacy" and path == runtime.root:
        return path
    if slot["slot"] not in ("blue", "green") or not path.is_relative_to(runtime.root / ".slots" / slot["slot"]):
        raise OperationError("invalid release slot path")
    return path


def compose(runtime, slot, *args):
    path = slot_path(runtime, slot)
    return [*compose_command(path, path / ".env.production"), *args]


def control(runtime, slot, service, action="status", boot=None):
    result = runtime.execute("slot-" + action, compose(runtime, slot, "exec", "-T", service, "/servicecheck",
                             "--lifecycle=" + action, "--role=" + service, "--boot-id=" + (boot or "")), timeout=10)
    value = json.loads(result)
    expected = slot["slot"]
    if value.get("protocol") != 1 or value.get("slot") != expected or not re.fullmatch(r"[a-f0-9]{32}", value.get("bootID", "")):
        raise OperationError("release requires the lifecycle-capable baseline")
    return value


def running(runtime, slot):
    output = runtime.execute("slot-running", compose(runtime, slot, "ps", "--status", "running", "--services"), timeout=10)
    return set(output.decode().splitlines()) & set(SERVICES)


def statuses(runtime, slot, partial=False):
    services = running(runtime, slot) if partial else SERVICES
    return {service: control(runtime, slot, service) for service in services}


def lifecycle_capable(runtime, tag, registry):
    output = runtime.execute("release-lifecycle", ["docker", "image", "inspect", "--format",
                             '{{index .Config.Labels "cn.tellyouwhat.lifecycle"}}',
                             *[registry + "-" + s + ":" + tag for s in SERVICES]])
    return output.decode().splitlines() == ["1"] * len(SERVICES)


def action(runtime, slot, verb, partial=False):
    values = statuses(runtime, slot, partial)
    for service in ("worker", "admin", "gateway"):
        if service not in values:
            continue
        value = values[service]
        if ((verb == "serve" and value["httpEnabled"]) or (verb == "resume" and value["backgroundEnabled"])
                or (verb == "pause" and not value["backgroundEnabled"])):
            continue
        control(runtime, slot, service, verb, value["bootID"])
    return values


def ready(runtime, slot, attempts):
    from release import internally_ready
    config = read_environment(slot_path(runtime, slot) / ".env.production")
    return internally_ready(config, attempts, ports=PORTS[slot["slot"]])


def require_ready(runtime, slot, attempts):
    if not ready(runtime, slot, attempts):
        raise OperationError("slot dependencies are not ready")


def proxy_config():
    # Ignore ambient proxy configuration for the host-only administration API.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(PROXY_API, timeout=5) as response:
        return json.load(response)


def proxy_ports(value):
    found = []
    if isinstance(value, dict):
        for key, item in value.items():
            if key == "dial" and isinstance(item, str) and re.fullmatch(r"127\.0\.0\.1:(18080|18082|18180|18182)", item):
                found.append(int(item.rsplit(":", 1)[1]))
            else:
                found.extend(proxy_ports(item))
    elif isinstance(value, list):
        for item in value:
            found.extend(proxy_ports(item))
    return found


def assert_route(slot):
    ports = PORTS[slot["slot"]]
    found = set(proxy_ports(proxy_config()))
    if found != {ports[0], ports[2]}:
        raise OperationError("proxy route does not match the recorded slot; recovery required")


def render_proxy(source, slot):
    gateway, _, admin = PORTS[slot["slot"]]
    source, g = re.subn(r"reverse_proxy 127\.0\.0\.1:(?:18080|18180) \{", "reverse_proxy 127.0.0.1:" + str(gateway) + " {", source)
    source, a = re.subn(r"reverse_proxy 127\.0\.0\.1:(?:18082|18182) \{", "reverse_proxy 127.0.0.1:" + str(admin) + " {", source)
    if g != 1 or a != 1:
        raise OperationError("managed Caddy upstreams are not in the supported layout")
    # Only the production Gateway block is edited; development and other sites remain intact.
    # Header placeholders contain braces, so insert at the beginning of the block.
    marker = "reverse_proxy 127.0.0.1:" + str(gateway) + " {"
    block = source[source.index(marker):]
    body = block.split("\n\t}", 1)[0]
    if "stream_close_delay" not in body:
        source = source.replace(marker, marker + "\n\t\tstream_close_delay 4h", 1)
    elif not re.search(r"(?m)^\s*stream_close_delay 4h\s*$", body):
        raise OperationError("production streaming protection must use stream_close_delay 4h")
    return source


def switch_proxy(runtime, slot):
    before = PROXY_FILE.read_text()
    root_before = PROXY_ROOT.read_text()
    if root_before.count(str(PROXY_FILE)) != 1:
        raise OperationError("Caddy must import the dedicated backend fragment exactly once")
    candidate = runtime.state / "proxy-fragment.caddy"
    candidate.write_text(render_proxy(before, slot))
    candidate.chmod(0o600)
    # Keep the validation root next to the original to preserve relative imports.
    validation_root = PROXY_ROOT.parent / ".tellyouwhat-validation"
    temporary_root = runtime.state / "proxy-root.caddy"
    temporary_root.write_text(root_before.replace(str(PROXY_FILE), str(candidate)))
    runtime.execute("proxy-stage", ["sudo", "-n", "install", "-m", "600", str(temporary_root), str(validation_root)])
    runtime.execute("proxy-validate", ["sudo", "-n", "caddy", "validate", "--config", str(validation_root), "--adapter", "caddyfile"])
    if PROXY_FILE.read_text() != before or PROXY_ROOT.read_text() != root_before:
        raise OperationError("Caddy configuration changed during preparation")
    backup = runtime.state / "proxy-before.caddy"
    backup.write_text(before)
    try:
        runtime.execute("proxy-install", ["sudo", "-n", "install", "-m", "644", str(candidate), str(PROXY_FILE)])
        runtime.execute("proxy-reload", ["sudo", "-n", "caddy", "reload", "--config", str(PROXY_ROOT), "--adapter", "caddyfile"])
        assert_route(slot)
    except Exception:
        runtime.execute("proxy-restore", ["sudo", "-n", "install", "-m", "644", str(backup), str(PROXY_FILE)])
        runtime.execute("proxy-restore-reload", ["sudo", "-n", "caddy", "reload", "--config", str(PROXY_ROOT), "--adapter", "caddyfile"])
        raise
    return hashlib.sha256(candidate.read_bytes()).hexdigest()


def resource_check(runtime, slot):
    memory = dict(re.findall(r"^(MemTotal|MemAvailable):\s+(\d+)", Path("/proc/meminfo").read_text(), re.M))
    if int(memory["MemAvailable"]) < (1344 + 512) * 1024 or int(memory["MemTotal"]) < (2688 + 192 + 512) * 1024:
        raise OperationError("insufficient memory for two release slots; active services were retained")
    disk = shutil.disk_usage(runtime.root)
    if disk.free < max(2 * 1024**3, disk.total * 0.15):
        raise OperationError("insufficient disk headroom")
    for port in PORTS[slot]:
        with socket.socket() as listener:
            try:
                listener.bind(("127.0.0.1", port))
            except OSError as error:
                raise OperationError("candidate slot port is occupied") from error


def image_metadata(runtime, tag, registry):
    result = {}
    for service in (*SERVICES, "adminctl", "migrate", "maintenance"):
        image = registry + "-" + service + ":" + tag
        output = runtime.execute("slot-image", ["docker", "image", "inspect", "--format",
                                 '{{json .RepoDigests}}|{{index .Config.Labels "cn.tellyouwhat.lifecycle"}}|{{index .Config.Labels "cn.tellyouwhat.operations-automation"}}', image])
        digests, lifecycle, automation = output.decode().strip().split("|")
        matches = [d for d in json.loads(digests) if d.startswith(registry + "-" + service + "@sha256:")]
        if len(matches) != 1 or lifecycle != "1" or automation != "1":
            raise OperationError("release images require immutable digests and lifecycle/automation support")
        result[service] = matches[0]
    return result


def environment_update(path, values):
    lines = [line for line in path.read_text().splitlines() if line.partition("=")[0] not in values]
    content = "\n".join([*lines, *[key + "=" + value for key, value in values.items()]]) + "\n"
    temporary = path.with_name(".slot-env.pending")
    temporary.write_text(content)
    temporary.chmod(0o600)
    os.replace(temporary, path)


def prepare(runtime, source, slot, tag, registry):
    from release import copy_runtime
    path = runtime.root / ".slots" / slot / (tag + "-" + str(time.time_ns()))
    copy_runtime(source, path)
    path.chmod(0o700)
    # Docker bind mounts retain directory/file ownership needed by UID 65532.
    runtime.execute("slot-secrets", ["sudo", "-n", "chgrp", "-R", "65532", str(path / "secrets")])
    (path / "secrets").chmod(0o750)
    for key in (path / "secrets").iterdir():
        key.chmod(0o640)
    images = image_metadata(runtime, tag, registry)
    ports = PORTS[slot]
    values = {"COMPOSE_PROJECT_NAME": "tellyouwhat-" + slot, "TELLYOUWHAT_DEPLOYMENT_SLOT": slot,
              "GATEWAY_HOST_PORT": str(ports[0]), "WORKER_HOST_PORT": str(ports[1]), "ADMIN_HOST_PORT": str(ports[2]),
              "IMAGE_TAG": tag, "IMAGE_REGISTRY_PREFIX": registry}
    values.update({service.upper() + "_IMAGE": image for service, image in images.items()})
    environment_update(path / ".env.production", values)
    return {"slot": slot, "path": str(path.relative_to(runtime.root)), "tag": tag, "registry": registry,
            "images": images, "automationVersion": 1, "healthy": False, "snapshot": str(path.relative_to(runtime.root))}


def validate_slot_config(runtime, slot):
    output = runtime.execute("slot-config", compose(runtime, slot, "config", "--format", "json"))
    config = json.loads(output)
    if config.get("name") != "tellyouwhat-" + slot["slot"]:
        raise OperationError("candidate Compose project is not isolated")
    for service, port, limit in zip(SERVICES, PORTS[slot["slot"]], (384, 768, 192)):
        value = config["services"][service]
        bindings = value.get("ports", [])
        if (value.get("image") != slot["images"][service]
                or value.get("environment", {}).get("TELLYOUWHAT_DEPLOYMENT_SLOT") != slot["slot"]
                or not 0 < int(value.get("mem_limit", 0)) <= limit * 1024**2
                or len(bindings) != 1 or bindings[0].get("host_ip") != "127.0.0.1"
                or str(bindings[0].get("published")) != str(port)):
            raise OperationError("candidate image, admission, memory or port configuration is unsafe")


def adopt(runtime):
    if read_state(runtime):
        raise OperationError("blue-green deployment is already initialized")
    if runtime.config.get("PUBLIC_PROXY_MODE") != "external":
        raise OperationError("blue-green deployment requires native Caddy")
    record = json.loads((runtime.state / "release.json").read_text())
    current = {**record["current"], "slot": "legacy", "path": "."}
    require_ready(runtime, current, 1)
    values = statuses(runtime, current)
    assert_route(current)
    # The first proxy reload establishes stream protection for future reloads.
    # Existing legacy WebSockets may reconnect during this explicit bootstrap.
    revision = switch_proxy(runtime, current)
    state = {"version": 1, "phase": "STABLE", "current": current, "previous": None,
             "proxyRevision": revision, "boots": {s: v["bootID"] for s, v in values.items()}}
    write_state(runtime, state)
    return {"passed": True, "bootstrap": True, **state}


def activate(runtime, state, candidate, attempts):
    previous = state["current"]
    # Pausing acknowledges every process before any candidate can claim work.
    action(runtime, previous, "pause")
    action(runtime, candidate, "serve")
    write_state(runtime, state, phase="SWITCHING", candidate=candidate)
    revision = switch_proxy(runtime, candidate)
    write_state(runtime, state, phase="DRAINING", current=candidate, previous=previous,
                candidate=None, proxyRevision=revision, switched_at=int(time.time()), quiet_since=None)
    action(runtime, candidate, "resume")
    require_ready(runtime, candidate, attempts)
    publish(runtime, state)


def publish(runtime, state):
    runtime.record("release", current=state["current"], previous=state.get("previous"),
                   acceptance="internal", deployment=state["phase"], proxyRevision=state.get("proxyRevision"))


def deploy(runtime, tag, registry, acceptance, bundle, attempts):
    from release import validate
    state = read_state(runtime)
    if state["phase"] != "STABLE":
        raise OperationError("previous release is draining or requires recovery")
    previous = state["current"]
    assert_route(previous)
    require_ready(runtime, previous, 1)
    source = Path(bundle).resolve() if bundle else None
    if source != runtime.root / (".incoming-" + tag):
        raise OperationError("blue-green release requires its immutable staged bundle")
    config = validate(source, tag, registry, acceptance)
    if config.get("PUBLIC_PROXY_MODE") != "external":
        raise OperationError("native Caddy is required")
    active_config = read_environment(slot_path(runtime, previous) / ".env.production")
    # Shared data/identity migrations require a separate coordinated operation.
    shared = ("MYSQL_HOST", "MYSQL_PORT", "MYSQL_DATABASE", "REDIS_HOST", "REDIS_PORT", "PAYLOAD_ENCRYPTION_KEY",
              "HEALTH_API_DOMAIN", "JOURNAL_API_DOMAIN", "ADMIN_DOMAIN")
    for key in shared:
        if config.get(key) != active_config.get(key):
            raise OperationError("shared data or domain configuration changed; coordinated migration required")
    target = "green" if previous["slot"] in ("legacy", "blue") else "blue"
    resource_check(runtime, target)
    # Strip slot settings from the protected bundle; only the host assigns them.
    environment_update(source / ".env.production", {"IMAGE_TAG": tag, "IMAGE_REGISTRY_PREFIX": registry,
                       **{s.upper() + "_IMAGE": "" for s in (*SERVICES, "adminctl", "migrate", "maintenance")}})
    runtime.execute("slot-pull", [*compose_command(source, source / ".env.production"), "pull", *SERVICES, "adminctl", "migrate", "maintenance"], timeout=900)
    resource_check(runtime, target)
    candidate = prepare(runtime, source, target, tag, registry)
    write_state(runtime, state, phase="PREPARING", candidate=candidate)
    try:
        validate_slot_config(runtime, candidate)
        runtime.execute("slot-migrate", compose(runtime, candidate, "run", "--rm", "--no-deps", "migrate"), timeout=300)
        runtime.execute("slot-start", compose(runtime, candidate, "up", "-d", "--no-build", *SERVICES), timeout=180)
        values = statuses(runtime, candidate)
        if any(v["backgroundEnabled"] or v["httpEnabled"] for v in values.values()):
            raise OperationError("candidate did not start in standby")
        require_ready(runtime, candidate, attempts)
        candidate["healthy"] = True
        write_state(runtime, state, phase="READY")
        activate(runtime, state, candidate, attempts)
    except Exception as error:
        write_state(runtime, state, phase="RECOVERY_REQUIRED", recovery_target=previous, candidate=candidate)
        try:
            recover(runtime, attempts)
        except Exception:
            raise OperationError("release failed; explicit recovery is required") from error
        raise OperationError("release failed; previous route restored and candidate retained for draining") from error
    # Keep code and release data separate; live slots retain immutable app files.
    for script in (source / "deploy/tencent").iterdir():
        if script.is_file() and script.suffix in (".py", ".sh"):
            shutil.copy2(script, runtime.root / "deploy/tencent" / script.name)
    shutil.rmtree(source)
    return {"passed": True, "deployment": "DRAINING", "current": state["current"]}


def recover(runtime, attempts):
    state = read_state(runtime)
    target = state.get("recovery_target") or state["current"]
    candidate = state.get("candidate")
    if not candidate:
        raise OperationError("no interrupted release to recover")
    require_ready(runtime, target, attempts)
    # Never grant old authority before a potentially active candidate is paused.
    action(runtime, candidate, "pause", partial=True)
    action(runtime, target, "serve")
    revision = switch_proxy(runtime, target)
    write_state(runtime, state, phase="DRAINING", current=target, previous=candidate, candidate=None,
                recovery_target=None, proxyRevision=revision, switched_at=int(time.time()), quiet_since=None)
    action(runtime, target, "resume")
    publish(runtime, state)
    return {"passed": True, "deployment": "DRAINING", "current": target}


def rollback(runtime, attempts):
    state = read_state(runtime)
    if state["phase"] not in ("STABLE", "DRAINING") or not state.get("previous"):
        raise OperationError("no compatible slot is available for rollback")
    assert_route(state["current"])
    candidate = state["previous"]
    if not candidate.get("healthy"):
        raise OperationError("previous slot never passed readiness verification")
    if not running(runtime, candidate):
        resource_check(runtime, candidate["slot"])
    # A retired legacy slot occupies blue ports and cannot be restarted after
    # blue is adopted. Use a lifecycle-capable slot snapshot instead.
    if candidate["slot"] == "legacy" and state["current"]["slot"] == "blue":
        raise OperationError("legacy rollback slot conflicts with active blue ports")
    runtime.execute("slot-rollback-start", compose(runtime, candidate, "up", "-d", "--no-build", *SERVICES), timeout=180)
    require_ready(runtime, candidate, attempts)
    previous = state["current"]
    write_state(runtime, state, phase="READY", candidate=candidate)
    try:
        activate(runtime, state, candidate, attempts)
    except Exception:
        write_state(runtime, state, phase="RECOVERY_REQUIRED", recovery_target=previous, candidate=candidate)
        raise
    return {"passed": True, "deployment": "DRAINING", "current": candidate}


def maintain(runtime, attempts=1):
    state = read_state(runtime)
    if not state:
        if lifecycle_capable(runtime, runtime.config.get("IMAGE_TAG", ""), runtime.config.get("IMAGE_REGISTRY_PREFIX", "")):
            slot = {"slot": "legacy", "path": "."}
            require_ready(runtime, slot, attempts)
            action(runtime, slot, "serve")
            action(runtime, slot, "resume")
        return {"passed": True, "deployment": "legacy"}
    if state["phase"] not in ("STABLE", "DRAINING"):
        raise OperationError("interrupted release requires recovery")
    current = state["current"]
    assert_route(current)
    require_ready(runtime, current, attempts)
    # Managed processes always restart in standby. Only the recorded, verified
    # active slot may receive fresh authority from this host controller.
    if state["phase"] == "DRAINING":
        old = state["previous"]
        action(runtime, old, "pause", partial=True)
    action(runtime, current, "serve")
    action(runtime, current, "resume")
    if state["phase"] == "DRAINING":
        old = state["previous"]
        values = statuses(runtime, old, partial=True)
        if any(v["backgroundEnabled"] for v in values.values()):
            action(runtime, old, "pause")
            raise OperationError("inactive slot had background admission enabled")
        busy = any(v["http"] or v["background"] for v in values.values())
        now = int(time.time())
        if busy:
            write_state(runtime, state, quiet_since=None)
        elif state.get("quiet_since") is None:
            write_state(runtime, state, quiet_since=now)
        elif now - state["quiet_since"] >= 60 and now - state["switched_at"] >= 120:
            # Gateway is sealed first, so it cannot produce a new Worker request.
            for service in SERVICES:
                if service in values:
                    control(runtime, old, service, "seal", values[service]["bootID"])
            runtime.execute("slot-stop", compose(runtime, old, "stop", *SERVICES), timeout=120)
            write_state(runtime, state, phase="STABLE", quiet_since=None)
            publish(runtime, state)
    if state["phase"] == "DRAINING" and time.time() - state["switched_at"] > 4 * 3600:
        raise OperationError("old slot drain exceeded four hours; retained for investigation")
    runtime.record("deployment_health", passed=True, phase=state["phase"], current_slot=current["slot"])
    return {"passed": True, "deployment": state["phase"], "current": current}
