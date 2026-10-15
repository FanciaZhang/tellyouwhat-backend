"""Reclaim owned host artifacts while retaining live mounts and recovery versions."""

import argparse
from datetime import datetime
import json
import os
from pathlib import Path
import re
import shutil
import time
import urllib.request
from urllib.parse import urlsplit

from ops_common import OperationError, Runtime, atomic_json


OWNERS = ("production", "journal-development", "birthday-development", "albums-test", "platform-test")
GRACE_SECONDS = 48 * 3600
IMAGE_PREFIXES = {
    "production": ("ghcr.io/fanciazhang/tellyouwhat-ai-", "ghcr.io/fanciazhang/health-ai-", "health-ai-"),
    "birthday-development": ("birthday-development:",),
    "albums-test": ("tellyouwhat-albums-test-",),
    "platform-test": ("tellyouwhat-platform-test-",),
}


def owned_image(tag, owner):
    return any(tag.startswith(prefix) for prefix in IMAGE_PREFIXES.get(owner, ()))


def timestamp(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def inspect_containers(runtime):
    ids = runtime.execute("artifacts-containers", ["docker", "ps", "-aq"]).decode().split()
    return json.loads(runtime.execute("artifacts-mounts", ["docker", "inspect", *ids])) if ids else []


def inspect_host(runtime):
    containers = inspect_containers(runtime)
    ids = sorted(set(runtime.execute("artifacts-images", ["docker", "image", "ls", "-q", "--no-trunc"]).decode().split()))
    images = []
    for start in range(0, len(ids), 100):
        images.extend(json.loads(runtime.execute("artifacts-image-details", ["docker", "image", "inspect", *ids[start:start + 100]])))
    return containers, images


def references(value):
    if isinstance(value, dict):
        for key, child in value.items():
            if key in ("path", "snapshot") and isinstance(child, str):
                yield "path", child
            if key == "images" and isinstance(child, dict):
                for image in child.values():
                    yield "image", image
            if key == "registry" and value.get("tag"):
                for service in ("gateway", "worker", "admin", "adminctl", "migrate", "maintenance"):
                    yield "image", child + "-" + service + ":" + value["tag"]
            yield from references(child)
    elif isinstance(value, list):
        for child in value:
            yield from references(child)


def directory_size(path):
    if path.is_file():
        return path.stat().st_size
    return sum(p.stat().st_size for p in path.rglob("*") if p.is_file() and not p.is_symlink())


def plan_cleanup(root, state, containers, images, owners=None, now=None):
    root = Path(root).resolve()
    owners = set(owners or OWNERS)
    now = time.time() if now is None else now
    mounts = {Path(m["Source"]).resolve() for c in containers for m in c.get("Mounts", []) if m.get("Type") == "bind"}
    protected_images = {c["Image"] for c in containers}
    protected_paths = set(mounts)
    for kind, value in references(state):
        if kind == "image":
            protected_images.add(value)
        else:
            path = (root / value).resolve()
            if not path.is_relative_to(root):
                raise OperationError("artifact recovery path escapes the backend")
            protected_paths.add(path)
    for path in (root / ".operations/artifact-owners").glob("*.json"):
        record = json.loads(path.read_text())
        if record.get("owner") not in OWNERS:
            raise OperationError("unknown artifact owner")
        for item in (record.get("current"), record.get("previous")):
            if not item:
                continue
            protected_paths.update(Path(p).resolve() for p in item.get("paths", []))
            protected_images.update(item.get("images", []))
        lease = record.get("lease")
        if lease and lease.get("expires_at", 0) > now:
            protected_paths.update(Path(p).resolve() for p in lease.get("paths", []))
            protected_images.update(lease.get("images", []))

    # Legacy development deployments predate owner records: retain two builds per image family.
    for owner in ("birthday-development", "albums-test", "platform-test"):
        if (root / ".operations/artifact-owners" / (owner + ".json")).exists():
            continue
        families = {}
        for image in images:
            for tag in image.get("RepoTags") or []:
                if owned_image(tag, owner):
                    families.setdefault(tag.rsplit(":", 1)[0], []).append(image)
        for versions in families.values():
            protected_images.update(i["Id"] for i in sorted(versions, key=lambda i: timestamp(i["Created"]), reverse=True)[:2])

    files = []
    if "journal-development" in owners:
        directory = root.parent / "journal-development/bin"
        if directory.is_symlink():
            raise OperationError("development artifact directory is a symlink")
        versions = sorted((p for p in directory.glob("journaldevserver-*") if re.fullmatch(r"journaldevserver-[0-9a-f]{16}", p.name)),
                          key=lambda p: p.lstat().st_mtime, reverse=True)
        if not (root / ".operations/artifact-owners/journal-development.json").exists():
            protected_paths.update(p.resolve() for p in versions[:2])
        files.extend(("journal-development", p) for p in versions)
    if "production" in owners:
        if (root / ".slots").is_symlink():
            raise OperationError("release slots directory is a symlink")
        for parent, pattern in ((root / ".releases", "*"), (root / ".incoming", "*"), (root, ".incoming-*")):
            if parent.is_symlink():
                raise OperationError("release artifact directory is a symlink")
            for p in parent.glob(pattern):
                if p.is_dir() and (p.name.startswith(("before-", "healthy-", "voice-enabled-", ".incoming-"))):
                    files.append(("production", p))
        for slot in ("blue", "green"):
            directory = root / ".slots" / slot
            if directory.is_symlink():
                raise OperationError("release slot directory is a symlink")
            files.extend(("production", p) for p in directory.glob("*") if p.is_dir() and re.fullmatch(r"[0-9a-f]{40}-[0-9]+", p.name))
    selected_files = []
    for owner, p in files:
        if p.is_symlink() or p.lstat().st_mtime > now - GRACE_SECONDS:
            continue
        path = p.resolve()
        if any(path == keep or path.is_relative_to(keep) or keep.is_relative_to(path) for keep in protected_paths):
            continue
        selected_files.append({"owner": owner, "path": str(p), "bytes": directory_size(p)})
    selected_images = []
    for image in images:
        tags = image.get("RepoTags") or []
        names = {image["Id"], *tags, *(image.get("RepoDigests") or [])}
        if names & protected_images or timestamp(image["Created"]) > now - GRACE_SECONDS:
            continue
        owner = next((o for o in owners if tags and all(owned_image(t, o) for t in tags)), None)
        if owner:
            selected_images.append({"owner": owner, "id": image["Id"], "tags": tags})
    return {"files": selected_files, "images": selected_images,
            "protected_paths": sorted(str(p) for p in protected_paths), "protected_images": sorted(protected_images)}


def read_state(runtime):
    state = {}
    for name in ("blue-green", "release"):
        path = runtime.state / (name + ".json")
        if path.exists():
            state[name] = json.loads(path.read_text())
    if not state or not (state.get("blue-green", {}).get("current") or state.get("release", {}).get("current")):
        raise OperationError("verified production recovery state is required before cleanup")
    return state


def cleanup(runtime, owner=None, *, apply=True, cache=False):
    before = shutil.disk_usage(runtime.root).free
    containers, images = inspect_host(runtime)
    plan = plan_cleanup(runtime.root, read_state(runtime), containers, images, [owner] if owner else None)
    if not apply:
        return {"dry_run": True, "free_bytes": before, **plan}
    removed_files, removed_images, errors = [], [], []
    for item in plan["files"]:
        # Recheck live mounts immediately before deletion, including newly created containers.
        latest = inspect_containers(runtime)
        fresh = plan_cleanup(runtime.root, read_state(runtime), latest, images, [owner] if owner else None)
        if item["path"] not in {p["path"] for p in fresh["files"]}:
            continue
        path = Path(item["path"])
        try:
            if path.is_dir():
                shutil.rmtree(path)
            else:
                path.unlink()
        except PermissionError:
            # Some retired snapshots were created by historical root deployments.
            try:
                runtime.execute("artifacts-remove-file", ["sudo", "-n", "rm", "-r", "--", str(path)])
            except OperationError as error:
                errors.append(str(error))
                continue
        except OSError as error:
            errors.append(type(error).__name__ + " removing an owned artifact")
            continue
        removed_files.append(item)
    for item in plan["images"]:
        try:
            runtime.execute("artifacts-remove-image", ["docker", "image", "rm", *item["tags"]], timeout=120)
            removed_images.append(item)
        except OperationError as error:
            errors.append(str(error))
    if cache:
        try:
            runtime.execute("artifacts-build-cache", ["docker", "builder", "prune", "-a", "-f", "--keep-storage", "2GB"], timeout=900)
        except OperationError as error:
            errors.append(str(error))
    after = shutil.disk_usage(runtime.root).free
    result = runtime.record("artifacts" + ("-" + owner if owner else ""), passed=not errors, errors=errors,
                          removed_files=removed_files, removed_images=removed_images,
                          free_bytes_before=before, free_bytes_after=after, reclaimed_bytes=after - before)
    if errors:
        raise OperationError("artifact cleanup incomplete: " + "; ".join(errors))
    return result


def require_operator(root):
    if os.geteuid() != Path(root).stat().st_uid:
        raise OperationError("artifact operations must run as the backend directory owner; do not use sudo root")


def register(runtime, owner, paths=(), images=(), source="", source_run=""):
    require_operator(runtime.root)
    if owner not in OWNERS or not source:
        raise OperationError("an artifact owner and source revision are required")
    path = runtime.state / "artifact-owners" / (owner + ".json")
    previous = json.loads(path.read_text()) if path.exists() else {}
    current = {"paths": [str(Path(p).resolve()) for p in paths], "images": list(images),
               "source": source, "source_run": source_run, "completed_at": int(time.time())}
    if any(not Path(p).exists() or Path(p).is_symlink() for p in current["paths"]):
        raise OperationError("registered artifact path must exist and cannot be a symlink")
    for p in paths:
        candidate = Path(p)
        allowed = (runtime.root.parent / "journal-development/bin") if owner == "journal-development" else runtime.root
        if candidate.is_symlink() or not candidate.resolve().is_relative_to(allowed):
            raise OperationError("registered artifact path escapes its owner scope")
    record = {"owner": owner, "current": current, "previous": previous.get("current")}
    if previous.get("current", {}).get("paths") == current["paths"] and previous.get("current", {}).get("images") == current["images"]:
        record["previous"] = previous.get("previous")
    atomic_json(path, record)
    return record


def development_deploy(runtime, owner, paths, images, source, source_run, command, verify_url):
    require_operator(runtime.root)
    url = urlsplit(verify_url)
    if (owner not in ("birthday-development", "albums-test", "platform-test") or not source or not images or not command
            or url.scheme != "http" or url.hostname != "127.0.0.1" or url.username or url.query
            or url.fragment or url.path not in ("/readyz", "/healthz")):
        raise OperationError("development deployment requires a scoped owner, revision, images and local readiness check")
    with runtime.lock(wait_seconds=600):
        with runtime.lock("deployment", wait_seconds=600):
            runtime.refresh()
            runtime.execute("artifact-producer-" + owner, command, timeout=900)
            # Albums validates Host and returns its API envelope for readiness.
            headers = {"Host": "api.albums.tellyouwhat.cn"} if owner == "albums-test" else {}
            readiness = urllib.request.Request(verify_url, headers=headers)
            for attempt in range(30):
                try:
                    with urllib.request.urlopen(readiness, timeout=2) as response:
                        if (response.geturl() == verify_url and response.status == 200
                                and (json.load(response).get("code") == "ok" if owner == "albums-test"
                                     else json.load(response).get("status") in ("ready", "ok"))):
                            break
                except (OSError, ValueError):
                    pass
                time.sleep(1)
            else:
                raise OperationError("development readiness failed; previous registered artifacts retained")
            for ref in images:
                runtime.execute("artifacts-register-image", ["docker", "image", "inspect", ref])
            register(runtime, owner, paths, images, source, source_run)
            return cleanup(runtime, owner)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--backend-dir", default="/opt/tellyouwhat/backend")
    parser.add_argument("--owner", choices=OWNERS)
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--cache", action="store_true")
    parser.add_argument("--register", action="store_true", help="register a verified deployment, then clean its retired outputs")
    parser.add_argument("--source", default="")
    parser.add_argument("--source-run", default="")
    parser.add_argument("--path", action="append", default=[])
    parser.add_argument("--image", action="append", default=[])
    parser.add_argument("--verify-url", default="")
    parser.add_argument("--run", nargs=argparse.REMAINDER, help="run an owned development producer under both locks, verify it, then register and clean")
    args = parser.parse_args()
    os.umask(0o077)
    require_operator(args.backend_dir)
    runtime = Runtime(args.backend_dir)
    if args.run:
        if not args.apply or not args.register:
            raise OperationError("owned development execution requires --apply --register")
        result = development_deploy(runtime, args.owner, args.path, args.image, args.source, args.source_run, args.run, args.verify_url)
        print(json.dumps(result, sort_keys=True))
        return
    with runtime.lock(wait_seconds=600):
        runtime.refresh()
        if args.register:
            if not args.owner or not args.apply or not (args.path or args.image):
                raise OperationError("registration requires --owner, --apply, --source and verified artifacts")
            for ref in args.image:
                runtime.execute("artifacts-register-image", ["docker", "image", "inspect", ref])
            register(runtime, args.owner, args.path, args.image, args.source, args.source_run)
        print(json.dumps(cleanup(runtime, args.owner, apply=args.apply, cache=args.cache), sort_keys=True))


if __name__ == "__main__":
    main()
