"""Single-host release slots; all mutating callers hold the deployment and operations locks."""

import hashlib
import json
from pathlib import Path
import re
import shutil
import socket
import time
import urllib.request

from ops_common import OperationError, atomic_json, atomic_text, compose_command, read_environment

SERVICES = ("gateway", "worker", "admin")
PORTS = {"legacy": (18080, 18081, 18082), "blue": (18080, 18081, 18082), "green": (18180, 18181, 18182)}
STATE = "blue-green.json"
PROXY_FILE = Path("/etc/caddy/tellyouwhat-backend.caddy")
PROXY_ROOT = Path("/etc/caddy/Caddyfile")
LEGACY_ALBUM_PROXY = Path("/etc/caddy/albums-test.caddy")
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
    from ops_albums import verify_gateway
    root = slot_path(runtime, slot)
    verify_gateway(runtime, root, read_environment(root / ".env.production"), slot["slot"])
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
    configuration = proxy_config()
    found = set(proxy_ports(configuration))
    if found != {ports[0], ports[2]}:
        raise OperationError("proxy route does not match the recorded slot; recovery required")
    expected = slot.get('proxy', {}).get('hosts', {})
    if expected:
        actual = proxy_hosts(configuration)
        if any(actual.get(host) != {upstream} for host, upstream in expected.items()):
            raise OperationError('public App host does not route exclusively to the selected shared slot')


def proxy_hosts(value):
    found = {}
    def dials(item):
        found = set()
        if isinstance(item, dict):
            if 'dial' in item:
                found.add(item['dial'])
            for child in item.values():
                found.update(dials(child))
        elif isinstance(item, list):
            for child in item:
                found.update(dials(child))
        return found
    def walk(item):
        if isinstance(item, dict):
            for match in item.get('match', []):
                for host in match.get('host', []):
                    found.setdefault(host, set()).update(dials(item))
            for child in item.values():
                walk(child)
        elif isinstance(item, list):
            for child in item:
                walk(child)
    walk(value)
    return found


def capture_proxy(runtime):
    root = PROXY_ROOT.read_text()
    marker = 'import ' + str(LEGACY_ALBUM_PROXY)
    if root.count(str(LEGACY_ALBUM_PROXY)) != root.splitlines().count(marker) or root.splitlines().count(marker) > 1:
        raise OperationError('legacy Albums proxy import is not in the managed layout')
    legacy = None
    if marker in root.splitlines():
        if LEGACY_ALBUM_PROXY.is_symlink() or not LEGACY_ALBUM_PROXY.is_file():
            raise OperationError('legacy Albums proxy must be a regular file')
        adapted = json.loads(runtime.execute('proxy-legacy-inspect',
            ['caddy', 'adapt', '--config', str(LEGACY_ALBUM_PROXY), '--adapter', 'caddyfile']))
        if proxy_hosts(adapted) != {'api.albums.tellyouwhat.cn': {'127.0.0.1:18091'}}:
            raise OperationError('legacy Albums fragment contains unrecognized routes')
        legacy = LEGACY_ALBUM_PROXY.read_text()
    return {'fragment': PROXY_FILE.read_text(), 'legacyAlbums': legacy}


def shared_proxy(config, slot, legacy):
    from release import public_targets
    targets = public_targets(config)
    gateway, _, admin = PORTS[slot['slot']]
    email = config.get('ACME_EMAIL', '')
    if email and not re.fullmatch(r'[A-Za-z0-9_.+%-]+@[A-Za-z0-9.-]+', email):
        raise OperationError('invalid production certificate contact')
    tls = '\n\ttls ' + email if email else ''
    headers = '''header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        X-Content-Type-Options "nosniff"
        Referrer-Policy "no-referrer"
        -Server
    }'''
    fragment = f'''(tellyouwhat_public_api) {{
    {headers}
    @internal path /internal/* /_development/* /_testflight/*
    respond @internal 404
    reverse_proxy 127.0.0.1:{gateway} {{
        stream_close_delay 4h
        header_up X-Real-IP {{remote_host}}
        flush_interval -1
    }}
}}
'''
    hosts = {}
    for target in targets:
        host = target['domain']
        is_admin = host == config['ADMIN_DOMAIN']
        hosts[host] = '127.0.0.1:' + str(admin if is_admin else gateway)
        body = (headers + f'\n\treverse_proxy 127.0.0.1:{admin} {{\n\t\theader_up X-Real-IP {{remote_host}}\n\t}}'
                if is_admin else 'import tellyouwhat_public_api')
        fragment += f'\n{host} {{{tls}\n\t{body}\n}}\n'
    return {'fragment': fragment, 'hosts': hosts,
            'legacyAlbums': None if 'api.albums.tellyouwhat.cn' in hosts else legacy}


def bind_proxy(runtime, state, candidate):
    # Freeze the previous topology before pausing services or replacing routes.
    if 'proxy' not in state['current']:
        state['current']['proxy'] = capture_proxy(runtime)
    if 'proxy' not in candidate:
        config = read_environment(slot_path(runtime, candidate) / '.env.production')
        candidate['proxy'] = shared_proxy(config, candidate, state['current']['proxy']['legacyAlbums'])
    write_state(runtime, state, candidate=candidate)


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
    if root_before.count(str(PROXY_FILE)) != 1 or ('import ' + str(PROXY_FILE)) not in root_before.splitlines():
        raise OperationError("Caddy must import the dedicated backend fragment exactly once")
    selection = slot.get('proxy') or capture_proxy(runtime)
    legacy_marker = 'import ' + str(LEGACY_ALBUM_PROXY)
    if (root_before.count(str(LEGACY_ALBUM_PROXY)) != root_before.splitlines().count(legacy_marker)
            or root_before.splitlines().count(legacy_marker) > 1):
        raise OperationError('legacy Albums proxy import is not in the managed layout')
    root_after = root_before
    if selection['legacyAlbums']:
        if (LEGACY_ALBUM_PROXY.is_symlink() or not LEGACY_ALBUM_PROXY.is_file()
                or LEGACY_ALBUM_PROXY.read_text() != selection['legacyAlbums']):
            raise OperationError('legacy Albums proxy differs from the selected recovery snapshot')
        if legacy_marker not in root_after.splitlines():
            root_after = root_after.rstrip() + '\n\n' + legacy_marker + '\n'
    else:
        if legacy_marker in root_before.splitlines():
            capture_proxy(runtime)
        root_after = ''.join(line for line in root_before.splitlines(keepends=True) if line.rstrip('\r\n') != legacy_marker)
    candidate = runtime.state / "proxy-fragment.caddy"
    atomic_text(candidate, render_proxy(selection['fragment'], slot))
    # Keep the validation root next to the original to preserve relative imports.
    validation_root = PROXY_ROOT.parent / ".tellyouwhat-validation"
    temporary_root = runtime.state / "proxy-root.caddy"
    atomic_text(temporary_root, root_after.replace(str(PROXY_FILE), str(candidate)))
    runtime.execute("proxy-stage", ["sudo", "-n", "install", "-m", "600", str(temporary_root), str(validation_root)])
    runtime.execute("proxy-validate", ["sudo", "-n", "caddy", "validate", "--config", str(validation_root), "--adapter", "caddyfile"])
    if PROXY_FILE.read_text() != before or PROXY_ROOT.read_text() != root_before:
        raise OperationError("Caddy configuration changed during preparation")
    backup = runtime.state / "proxy-before.caddy"
    atomic_text(backup, before)
    root_backup = runtime.state / 'proxy-root-before.caddy'
    atomic_text(root_backup, root_before)
    root_candidate = runtime.state / 'proxy-root-after.caddy'
    atomic_text(root_candidate, root_after)
    pending = PROXY_FILE.parent / ".tellyouwhat-fragment.pending"
    permissions = PROXY_FILE.stat()
    install = ["sudo", "-n", "install", "-m", format(permissions.st_mode & 0o777, "o"),
               "-o", str(permissions.st_uid), "-g", str(permissions.st_gid)]
    def install_root(source, label):
        permissions = PROXY_ROOT.stat()
        pending_root = PROXY_ROOT.parent / '.tellyouwhat-root.pending'
        runtime.execute(label, ['sudo', '-n', 'install', '-m', format(permissions.st_mode & 0o777, 'o'),
                        '-o', str(permissions.st_uid), '-g', str(permissions.st_gid), str(source), str(pending_root)])
        runtime.execute(label + '-sync', ['sudo', '-n', 'sync', '-f', str(pending_root)])
        runtime.execute(label + '-commit', ['sudo', '-n', 'mv', '-f', str(pending_root), str(PROXY_ROOT)])
        runtime.execute(label + '-commit-sync', ['sudo', '-n', 'sync', '-f', str(PROXY_ROOT)])
    try:
        runtime.execute("proxy-install", [*install, str(candidate), str(pending)])
        runtime.execute("proxy-sync", ["sudo", "-n", "sync", "-f", str(pending)])
        runtime.execute("proxy-commit", ["sudo", "-n", "mv", "-f", str(pending), str(PROXY_FILE)])
        runtime.execute("proxy-commit-sync", ["sudo", "-n", "sync", "-f", str(PROXY_FILE)])
        if root_after != root_before:
            install_root(root_candidate, 'proxy-root-install')
        runtime.execute("proxy-reload", ["sudo", "-n", "caddy", "reload", "--config", str(PROXY_ROOT), "--adapter", "caddyfile"])
        assert_route(slot)
    except Exception:
        if root_after != root_before:
            install_root(root_backup, 'proxy-root-restore')
        runtime.execute("proxy-restore", [*install, str(backup), str(pending)])
        runtime.execute("proxy-restore-sync", ["sudo", "-n", "sync", "-f", str(pending)])
        runtime.execute("proxy-restore-commit", ["sudo", "-n", "mv", "-f", str(pending), str(PROXY_FILE)])
        runtime.execute("proxy-restore-commit-sync", ["sudo", "-n", "sync", "-f", str(PROXY_FILE)])
        runtime.execute("proxy-restore-reload", ["sudo", "-n", "caddy", "reload", "--config", str(PROXY_ROOT), "--adapter", "caddyfile"])
        raise
    return hashlib.sha256(candidate.read_bytes()).hexdigest()


class CapacityError(OperationError):
    """Only CPU/RAM pressure may offer a separately confirmed interruption."""


def cpu_busy_percent():
    def sample():
        values = [int(v) for v in Path("/proc/stat").read_text().splitlines()[0].split()[1:9]]
        return sum(values), values[3] + values[4]
    total, idle = sample()
    time.sleep(5)
    end_total, end_idle = sample()
    elapsed = end_total - total
    if elapsed <= 0:
        raise OperationError("CPU capacity sample is unavailable")
    return 100 * (1 - (end_idle - idle) / elapsed)


def storage_check(runtime):
    disk = shutil.disk_usage(runtime.root)
    if disk.free < max(2 * 1024**3, disk.total * 0.15):
        raise OperationError("insufficient disk headroom")


def require_memory_capacity(dual=True, gateway_mib=384):
    memory = dict(re.findall(r"^(MemTotal|MemAvailable):\s+(\d+)", Path("/proc/meminfo").read_text(), re.M))
    slot_mib = gateway_mib + 768 + 192
    if int(memory["MemAvailable"]) < (slot_mib + 512) * 1024 or (dual and int(memory["MemTotal"]) < (2 * slot_mib + 192 + 512) * 1024):
        raise CapacityError("insufficient memory headroom for release startup")


def resource_check(runtime, slot, gateway_mib=384):
    require_memory_capacity(gateway_mib=gateway_mib)
    if cpu_busy_percent() >= 85:
        raise CapacityError("CPU usage is at least 85 percent; active services were retained")
    storage_check(runtime)
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
    atomic_text(path, content)


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
    from ops_albums import selected, verify_compose
    selected_config = read_environment(slot_path(runtime, slot) / ".env.production")
    verify_compose(selected_config, config)
    for service, port, limit in zip(SERVICES, PORTS[slot["slot"]], (512 if selected(selected_config) else 384, 768, 192)):
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
    current['proxy'] = capture_proxy(runtime)
    # The first proxy reload establishes stream protection for future reloads.
    # Existing legacy WebSockets may reconnect during this explicit bootstrap.
    revision = switch_proxy(runtime, current)
    state = {"version": 1, "phase": "STABLE", "current": current, "previous": None,
             "proxyRevision": revision, "boots": {s: v["bootID"] for s, v in values.items()}}
    write_state(runtime, state)
    return {"passed": True, "bootstrap": True, **state}


def activate(runtime, state, candidate, attempts):
    previous = state["current"]
    bind_proxy(runtime, state, candidate)
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


def deploy(runtime, tag, registry, acceptance, bundle, attempts, source_run=""):
    from disruptive_release import capacity_check, clear_offer
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
    clear_offer(runtime)
    capacity_check(runtime, target, source, tag, registry, acceptance, source_run, previous)
    # Strip slot settings from the protected bundle; only the host assigns them.
    environment_update(source / ".env.production", {"IMAGE_TAG": tag, "IMAGE_REGISTRY_PREFIX": registry,
                       **{s.upper() + "_IMAGE": "" for s in (*SERVICES, "adminctl", "migrate", "maintenance")}})
    runtime.execute("slot-pull", [*compose_command(source, source / ".env.production"), "pull", *SERVICES, "adminctl", "migrate", "maintenance"], timeout=900)
    capacity_check(runtime, target, source, tag, registry, acceptance, source_run, previous)
    candidate = prepare(runtime, source, target, tag, registry)
    write_state(runtime, state, phase="PREPARING", candidate=candidate)
    try:
        validate_slot_config(runtime, candidate)
        runtime.execute("slot-migrate", compose(runtime, candidate, "run", "--rm", "--no-deps", "migrate"), timeout=300)
        runtime.execute("slot-start", compose(runtime, candidate, "up", "-d", "--no-build", *SERVICES), timeout=180)
        require_ready(runtime, candidate, attempts)
        values = statuses(runtime, candidate)
        if any(v["backgroundEnabled"] or v["httpEnabled"] for v in values.values()):
            raise OperationError("candidate did not start in standby")
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
    if state.get("strategy") == "disruptive":
        from disruptive_release import recover as recover_disruptive
        return recover_disruptive(runtime, state, attempts)
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
    from ops_recording import require_continuity
    require_continuity(read_environment(slot_path(runtime, state['current']) / '.env.production'),
                       read_environment(slot_path(runtime, candidate) / '.env.production'))
    from ops_albums import require_resources
    candidate_root = slot_path(runtime, candidate)
    require_resources(runtime, candidate_root, read_environment(candidate_root / '.env.production'))
    if not running(runtime, candidate):
        from ops_albums import selected
        resource_check(runtime, candidate["slot"], 512 if selected(runtime.config) or selected(read_environment(candidate_root / ".env.production")) else 384)
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
