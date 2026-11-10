"""Publish authenticated database/configuration pairs to a private backup bucket."""

import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import tarfile
import tempfile
import time

from ops_common import OperationError, read_environment


def config_path(runtime):
    return runtime.root.parent / "backup/cos.env"


def enabled(runtime):
    return config_path(runtime).exists() or (runtime.state / "offsite-required").exists()


def transport(runtime, action, paths=(), *, expected=None):
    interpreter = runtime.state / "backup-venv/bin/python"
    if not interpreter.is_file() or not config_path(runtime).is_file():
        raise OperationError("required offsite backup SDK or dedicated credentials are missing")
    request = {"config": str(config_path(runtime)), "action": action, "paths": [str(p) for p in paths]}
    if action == 'upload':
        request['expected'] = expected
    result = json.loads(runtime.execute("backup-cos-" + action,
        [str(interpreter), str(runtime.root / "deploy/tencent/ops_cos.py")],
        input=json.dumps(request).encode(), timeout=300))
    if result.get("passed") is not True:
        raise OperationError("offsite backup verification failed")
    return result


def create_recovery_bundle(runtime, filename, *, scope='host'):
    from ops_backup import crypt, file_digest
    from release import copy_runtime, RUNTIME_FILES, KEY_FILES
    destination = runtime.backups / ("recovery-" + filename.replace(".sql.gz.enc", ".tar.gz.enc"))
    state_path = runtime.state / "blue-green.json"
    state = json.loads(state_path.read_text()) if state_path.exists() else {}
    if state and state.get("phase") != "STABLE":
        raise OperationError("configuration backup requires a stable production release")
    slots = {k: v for k, v in state.items() if k in ("current", "previous") and v}
    if state and 'proxy' not in slots['current']:
        from blue_green import capture_proxy
        slots['current'] = dict(slots['current'], proxy=capture_proxy(runtime))
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
            from ops_albums import require_configuration
            require_configuration(runtime, source, values)
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
        from albums_artifacts import backup_configuration as backup_albums
        albums_production = backup_albums(runtime, bundle / 'albums-production', 'production')
        test_routing, apple_delivery, albums_development, platform_test, journal_development = {}, {}, {}, {}, {}
        if scope == 'host':
            from test_routes import backup_configuration
            test_routing = backup_configuration(runtime, bundle / 'test-routing')
            from apple_delivery_artifacts import backup_configuration as backup_apple_delivery
            apple_delivery = backup_apple_delivery(runtime, bundle / 'apple-delivery')
            albums_development = backup_albums(runtime, bundle / 'albums-development', 'development')
            from platform_test_artifacts import backup_configuration as backup_platform_test
            platform_test = backup_platform_test(runtime, bundle / 'platform-test')
            from journal_development_artifacts import backup_configuration as backup_journal_development
            journal_development = backup_journal_development(runtime, bundle / 'journal-development')
        compressed = temporary / "recovery.tar.gz"
        with tarfile.open(compressed, "w:gz") as archive:
            archive.add(bundle, arcname="recovery")
        encrypted = temporary / "recovery.enc"
        crypt(runtime, compressed, encrypted)
        os.replace(encrypted, destination)
    return {"recovery_filename": destination.name, "recovery_sha256": file_digest(destination),
            "recovery_images": images, "recovery_test_routing": test_routing,
            "recovery_apple_delivery": apple_delivery, "recovery_albums_development": albums_development,
            "recovery_albums_production": albums_production,
            "recovery_platform_test": platform_test, "recovery_journal_development": journal_development}


def publish(runtime, source, manifest):
    from ops_backup import BACKUP_RETENTION_SECONDS, _verified_backup, _same_backup_file
    source = Path(source)
    with _verified_backup(runtime, source) as (verified, files):
        if verified != manifest or not manifest.get('recovery_filename'):
            raise OperationError('offsite publication requires the authenticated complete backup')
        manifest = verified
        if manifest['created_at'] <= time.time() - BACKUP_RETENTION_SECONDS:
            raise OperationError('backup is outside the retention period')
        paths = [source, runtime.backups / manifest['recovery_filename'], Path(str(source) + '.json')]
        envelope, _ = files[paths[-1]]
        envelope.seek(0)
        expected = {source.name: manifest['sha256'], paths[1].name: manifest['recovery_sha256'],
                    paths[-1].name: hashlib.sha256(envelope.read(1024 * 1024 + 1)).hexdigest()}
        if any(not _same_backup_file(p, handle, before) for p, (handle, before) in files.items()):
            raise OperationError('backup input changed before publication')
        # Each payload must read back correctly before the final inventory can be sent.
        result = transport(runtime, 'upload', paths, expected=expected)
        objects = result.get('objects')
        sizes = {p.name: files[p][1].st_size for p in paths}
        if (not isinstance(objects, list) or len(objects) != len(expected)
                or not all(isinstance(obj, dict) and isinstance(obj.get('filename'), str)
                           and obj['filename'] in expected and obj.get('sha256') == expected[obj['filename']]
                           and type(obj.get('bytes')) is int and obj['bytes'] == sizes[obj['filename']]
                           for obj in objects)
                or len({obj['filename'] for obj in objects}) != len(expected)
                or any(not _same_backup_file(p, handle, before) for p, (handle, before) in files.items())):
            raise OperationError('offsite readback differs from the encrypted local backup')
        operation = 'offsite-production' if manifest.get('recovery_scope') == 'production' else 'offsite'
        return runtime.record(operation, filename=source.name, sha256=manifest['sha256'], bucket=result['bucket'],
                              recovery_filename=manifest['recovery_filename'], verified_objects=len(expected), passed=True)


def download(runtime, filename, directory):
    from ops_backup import BACKUP_RETENTION_SECONDS, _backup_manifest, verify_backup
    from ops_cos import valid_filename
    if not valid_filename(filename) or not filename.endswith(".sql.gz.enc"):
        raise OperationError("invalid offsite backup filename")
    source = Path(directory) / filename
    envelope_path = Path(str(source) + ".json")
    transport(runtime, "download", [envelope_path])
    manifest = _backup_manifest(runtime, source, json.loads(envelope_path.read_text()))
    if manifest["created_at"] <= time.time() - BACKUP_RETENTION_SECONDS:
        raise OperationError("backup is outside the retention period")
    recovery = manifest.get("recovery_filename", "")
    if not valid_filename(recovery) or not recovery.startswith("recovery-"):
        raise OperationError("offsite configuration recovery file is missing or invalid")
    transport(runtime, "download", [source, Path(directory) / recovery])
    verify_backup(runtime, source)
    return source


def verify_recovery_bundle(runtime, source, manifest, *, albums_checkpoints=None, verify_checkpoint_source=False):
    from ops_backup import crypt
    from release import KEY_FILES
    if albums_checkpoints is None:
        albums_checkpoints = {}
    if verify_checkpoint_source and not albums_checkpoints:
        raise OperationError('source verification requires an independent Albums checkpoint')
    if (not isinstance(albums_checkpoints, dict)
            or any(environment not in ('development', 'production')
                   or not manifest.get('recovery_albums_' + environment)
                   or not isinstance(selection, (tuple, list)) or len(selection) != 2
                   or not isinstance(selection[0], (str, Path)) or not str(selection[0])
                   or not isinstance(selection[1], str) or not re.fullmatch('[a-f0-9]{64}', selection[1])
                   for environment, selection in albums_checkpoints.items())):
        raise OperationError('independent Albums checkpoints must match the recovered environments and pinned digests')
    encrypted = Path(source).parent / manifest["recovery_filename"]
    with tempfile.TemporaryDirectory(prefix="recovery-check-", dir=runtime.state) as directory:
        archive_path = Path(directory) / "recovery.tar.gz"
        crypt(runtime, encrypted, archive_path, decrypt=True)
        with tarfile.open(archive_path, "r:gz") as archive:
            members = archive.getmembers()
            # Independent app sources, rollback selections and encrypted SQL share this bundle.
            if sum(m.size for m in members) > 512 * 1024**2 or any(
                Path(m.name).is_absolute() or ".." in Path(m.name).parts or not (m.isfile() or m.isdir()) for m in members):
                raise OperationError("unsafe or oversized configuration recovery archive")
            selection = json.load(archive.extractfile("recovery/selection.json"))
            images = {k: v.get("images", {}) for k, v in selection["slots"].items()}
            if images != manifest["recovery_images"]:
                raise OperationError("configuration recovery images differ from the authenticated manifest")
            apple_delivery = manifest.get('recovery_apple_delivery', {})
            if apple_delivery:
                from apple_delivery_artifacts import verify_recovery
                verify_recovery(archive, apple_delivery)
            from albums_artifacts import verify_recovery as verify_albums
            for environment in ('development', 'production'):
                if manifest.get('recovery_albums_' + environment):
                    verify_albums(archive, manifest['recovery_albums_' + environment], environment)
            if manifest.get('recovery_platform_test'):
                if ('current/albums.env' in manifest['recovery_platform_test']
                        and not manifest.get('recovery_albums_development')):
                    raise OperationError('shared test Albums recovery is missing its independent data and keys')
                from platform_test_artifacts import verify_recovery as verify_platform_test
                verify_platform_test(archive, manifest['recovery_platform_test'])
            if manifest.get('recovery_journal_development'):
                from journal_development_artifacts import verify_recovery as verify_journal_development
                verify_journal_development(archive, manifest['recovery_journal_development'])
                from journal_development_artifacts import verify_runtime_recovery
                checkpoint = Path(directory) / 'journal-checkpoint'
                checkpoint.mkdir(mode=0o700)
                for name in manifest['recovery_journal_development']:
                    (checkpoint / name).write_bytes(archive.extractfile('recovery/journal-development/' + name).read())
                    (checkpoint / name).chmod(0o600)
                verify_runtime_recovery(runtime, checkpoint, manifest['filename'])
            test_routing = manifest.get('recovery_test_routing', {})
            if test_routing:
                import hashlib
                if set(test_routing) != {'Caddyfile', 'test.caddy', 'state.json'}:
                    raise OperationError('test routing recovery selection is incomplete')
                for name, digest in test_routing.items():
                    try:
                        member = archive.getmember('recovery/test-routing/' + name)
                    except KeyError as error:
                        raise OperationError('test routing recovery file is missing') from error
                    if not member.isfile() or hashlib.sha256(archive.extractfile(member).read()).hexdigest() != digest:
                        raise OperationError('test routing recovery differs from the authenticated manifest')
            for label in selection["slots"]:
                prefix = "recovery/" + label + "/"
                if label not in ("current", "previous") or not archive.getmember(prefix + ".env.production").isfile():
                    raise OperationError("configuration recovery selection is incomplete")
                content = archive.extractfile(prefix + ".env.production").read().decode()
                if any(line.lstrip().startswith("BACKUP_ENCRYPTION_KEY=") for line in content.splitlines()):
                    raise OperationError("backup decryption key must be preserved separately")
                from ops_recording import configured_root
                recovered_env = Path(directory) / (label + '.env')
                recovered_env.touch(mode=0o600)
                recovered_env.write_text(content)
                if configured_root(read_environment(recovered_env), 'production') is not None:
                    try:
                        overlay = archive.getmember(prefix + 'compose.journal-recording.yaml')
                    except KeyError as error:
                        raise OperationError('recording recovery mount configuration is missing') from error
                    if not overlay.isfile():
                        raise OperationError('recording recovery mount configuration is invalid')
                from ops_albums import selected as albums_selected, OVERLAY as albums_overlay
                values = read_environment(recovered_env)
                if albums_selected(values):
                    if not manifest.get('recovery_albums_production'):
                        raise OperationError('shared Albums API recovery requires its full production checkpoint')
                    for name in ('albums.env', albums_overlay):
                        try:
                            item = archive.getmember(prefix + name)
                        except KeyError as error:
                            raise OperationError('shared Albums API recovery configuration is missing') from error
                        if not item.isfile() or not 0 < item.size <= 32768:
                            raise OperationError('shared Albums API recovery configuration is invalid')
                    state = json.loads(archive.extractfile('recovery/albums-production/state.json').read())
                    if values.get('ALBUM_RESOURCE_SOURCE') != state.get('source'):
                        raise OperationError('shared Albums API recovery differs from its data selection')
                for name in ["compose.production.yaml", *["secrets/" + key for key in KEY_FILES]]:
                    if not archive.getmember(prefix + name).isfile():
                        raise OperationError("configuration recovery signing or Compose files are incomplete")
            for environment in ('development', 'production'):
                checkpoint = manifest.get('recovery_albums_' + environment)
                if checkpoint:
                    from albums_artifacts import verify_runtime_recovery as restore_albums
                    independent = albums_checkpoints.get(environment)
                    restore_albums(runtime, archive, checkpoint, environment=environment,
                                   image=manifest['image'], backup_filename=manifest['filename'],
                                   independent_checkpoint=independent,
                                   verify_checkpoint_source=verify_checkpoint_source and independent is not None)
    return True
