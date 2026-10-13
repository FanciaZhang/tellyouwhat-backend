"""Publish authenticated database/configuration pairs to a private backup bucket."""

import hmac
import json
import os
from pathlib import Path
import shutil
import tarfile
import tempfile

from ops_common import OperationError, read_environment


def config_path(runtime):
    return runtime.root.parent / "backup/cos.env"


def enabled(runtime):
    return config_path(runtime).exists() or (runtime.state / "offsite-required").exists()


def transport(runtime, action, paths=()):
    interpreter = runtime.state / "backup-venv/bin/python"
    if not interpreter.is_file() or not config_path(runtime).is_file():
        raise OperationError("required offsite backup SDK or dedicated credentials are missing")
    request = {"config": str(config_path(runtime)), "action": action, "paths": [str(p) for p in paths]}
    result = json.loads(runtime.execute("backup-cos-" + action,
        [str(interpreter), str(runtime.root / "deploy/tencent/ops_cos.py")],
        input=json.dumps(request).encode(), timeout=300))
    if result.get("passed") is not True:
        raise OperationError("offsite backup verification failed")
    return result


def create_recovery_bundle(runtime, filename):
    from ops_backup import crypt, file_digest
    from release import copy_runtime, RUNTIME_FILES, KEY_FILES
    destination = runtime.backups / ("recovery-" + filename.replace(".sql.gz.enc", ".tar.gz.enc"))
    state_path = runtime.state / "blue-green.json"
    state = json.loads(state_path.read_text()) if state_path.exists() else {}
    if state and state.get("phase") != "STABLE":
        raise OperationError("configuration backup requires a stable production release")
    slots = {k: v for k, v in state.items() if k in ("current", "previous") and v}
    if not slots:
        slots = {"current": {"path": str(runtime.compose_root)}}
    images = {k: v.get("images", {}) for k, v in slots.items()}
    with tempfile.TemporaryDirectory(prefix=".recovery-", dir=runtime.backups) as directory:
        temporary = Path(directory)
        bundle = temporary / "contents"
        bundle.mkdir(mode=0o700)
        for label, selection in slots.items():
            source = (runtime.root / selection["path"]).resolve()
            if not source.is_relative_to(runtime.root):
                raise OperationError("configuration recovery path escapes the backend")
            if any((source / p).is_symlink() for p in RUNTIME_FILES + ["secrets/" + k for k in KEY_FILES]):
                raise OperationError("configuration recovery symlinks are not permitted")
            target = bundle / label
            copy_runtime(source, target)
            env = target / ".env.production"
            values = read_environment(env)
            # The decryption key is preserved separately from its encrypted backup.
            env.write_text("\n".join(line for line in env.read_text().splitlines() if not line.lstrip().startswith("BACKUP_ENCRYPTION_KEY=")) + "\n")
            external = values.get("ARK_MANAGEMENT_CREDENTIAL_HOST_FILE")
            if external:
                path = Path(external)
                if not path.is_absolute() or path.is_symlink() or not path.is_file() or path.stat().st_size > 8192:
                    raise OperationError("invalid isolated service credential recovery file")
                data = runtime.execute("backup-isolated-credential", ["sudo", "-n", "cat", "--", str(path)])
                descriptor = os.open(target / "ark-management.json", os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
                with os.fdopen(descriptor, "wb") as output:
                    output.write(data)
        (bundle / "selection.json").write_text(json.dumps({"version": 1, "slots": slots}, sort_keys=True))
        scripts = bundle / "operations"
        scripts.mkdir(mode=0o700)
        for path in (runtime.root / "deploy/tencent").glob("*"):
            if path.is_file() and not path.is_symlink() and path.suffix in (".py", ".sh") and not path.name.endswith("_test.py"):
                shutil.copy2(path, scripts / path.name)
        compressed = temporary / "recovery.tar.gz"
        with tarfile.open(compressed, "w:gz") as archive:
            archive.add(bundle, arcname="recovery")
        encrypted = temporary / "recovery.enc"
        crypt(runtime, compressed, encrypted)
        os.replace(encrypted, destination)
    return {"recovery_filename": destination.name, "recovery_sha256": file_digest(destination), "recovery_images": images}


def publish(runtime, source, manifest):
    from ops_backup import file_digest
    paths = [source, runtime.backups / manifest["recovery_filename"], Path(str(source) + ".json")]
    # The authenticated manifest is the final object and commits the complete pair.
    result = transport(runtime, "upload", paths)
    expected = {p.name: file_digest(p) for p in paths}
    actual = {p["filename"]: p["sha256"] for p in result["objects"]}
    if expected != actual:
        raise OperationError("offsite readback differs from the encrypted local backup")
    return runtime.record("offsite", filename=source.name, sha256=manifest["sha256"], bucket=result["bucket"],
                          recovery_filename=manifest["recovery_filename"], verified_objects=len(expected), passed=True)


def download(runtime, filename, directory):
    from ops_backup import backup_secret, manifest_signature, verify_backup
    from ops_cos import valid_filename
    if not valid_filename(filename) or not filename.endswith(".sql.gz.enc"):
        raise OperationError("invalid offsite backup filename")
    source = Path(directory) / filename
    envelope_path = Path(str(source) + ".json")
    transport(runtime, "download", [envelope_path])
    envelope = json.loads(envelope_path.read_text())
    manifest = envelope["manifest"]
    signature = manifest_signature(backup_secret(runtime), manifest)
    if not hmac.compare_digest(signature, envelope["hmac_sha256"]) or manifest.get("filename") != filename:
        raise OperationError("downloaded backup manifest authentication failed")
    recovery = manifest.get("recovery_filename", "")
    if not valid_filename(recovery) or not recovery.startswith("recovery-"):
        raise OperationError("offsite configuration recovery file is missing or invalid")
    transport(runtime, "download", [source, Path(directory) / recovery])
    verify_backup(runtime, source)
    return source


def verify_recovery_bundle(runtime, source, manifest):
    from ops_backup import crypt
    from release import KEY_FILES
    encrypted = Path(source).parent / manifest["recovery_filename"]
    with tempfile.TemporaryDirectory(prefix="recovery-check-", dir=runtime.state) as directory:
        archive_path = Path(directory) / "recovery.tar.gz"
        crypt(runtime, encrypted, archive_path, decrypt=True)
        with tarfile.open(archive_path, "r:gz") as archive:
            members = archive.getmembers()
            if sum(m.size for m in members) > 64 * 1024**2 or any(
                Path(m.name).is_absolute() or ".." in Path(m.name).parts or not (m.isfile() or m.isdir()) for m in members):
                raise OperationError("unsafe or oversized configuration recovery archive")
            selection = json.load(archive.extractfile("recovery/selection.json"))
            images = {k: v.get("images", {}) for k, v in selection["slots"].items()}
            if images != manifest["recovery_images"]:
                raise OperationError("configuration recovery images differ from the authenticated manifest")
            for label in selection["slots"]:
                prefix = "recovery/" + label + "/"
                if label not in ("current", "previous") or not archive.getmember(prefix + ".env.production").isfile():
                    raise OperationError("configuration recovery selection is incomplete")
                content = archive.extractfile(prefix + ".env.production").read().decode()
                if any(line.lstrip().startswith("BACKUP_ENCRYPTION_KEY=") for line in content.splitlines()):
                    raise OperationError("backup decryption key must be preserved separately")
                for name in ["compose.production.yaml", *["secrets/" + key for key in KEY_FILES]]:
                    if not archive.getmember(prefix + name).isfile():
                        raise OperationError("configuration recovery signing or Compose files are incomplete")
    return True
