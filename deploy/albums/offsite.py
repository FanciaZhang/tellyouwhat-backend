"""Encrypted, version-pinned COS recovery bundles; no latest-checkpoint or cutover claim."""
import copy
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import tarfile
import tempfile
import time

from backup import configure, verify, RETENTION
from deletion_ledger import verify as verify_ledger
from ops_backup import backup_secret, crypt, file_digest, manifest_signature
from ops_common import OperationError, atomic_json

SCOPE = "albums-offsite-bundle-v1"
MAX_BYTES = 1 << 30


def move(runtime, executable, credentials, operation, path, key, digest, size, **pinned):
    if not executable or not credentials:
        raise OperationError("offsite transfer requires an executable and dedicated private COS configuration")
    request = dict(Operation=operation, Path=str(path), Key=key, SHA256=digest, Bytes=size, **pinned)
    result = json.loads(runtime.execute("albums-backup-transfer", [str(executable), str(credentials)], input=json.dumps(request).encode(), timeout=960))
    if (result.get("Key") != key or result.get("SHA256") != digest or result.get("Bytes") != size
            or not isinstance(result.get("Version"), str) or result["Version"] == "null"
            or not re.fullmatch(r"[A-Za-z0-9_.~-]{1,1024}", result["Version"])
            or any(result.get(k) != v for k, v in pinned.items())):
        raise OperationError("offsite transfer did not return the pinned content")
    return result


def publish(runtime, executable, credentials, filename, ledger, ledger_digest):
    configure(runtime)
    m = verify(runtime, runtime.backups / filename)
    if m["created_at"] <= time.time() - RETENTION:
        raise OperationError("Albums backup is outside retention")
    verify_ledger(runtime, ledger, ledger_digest, m["created_at"])
    ledger_name = Path(ledger).name
    if not re.fullmatch(r"albums-deletions-[0-9a-f]{32}\.json", ledger_name):
        raise OperationError("invalid deletion ledger filename")
    names = (filename, filename + ".json", ledger_name)
    nonce = secrets.token_hex(16)
    with tempfile.TemporaryDirectory(prefix=".albums-offsite-", dir=runtime.backups) as directory:
        folder = Path(directory)
        stage = copy.copy(runtime)
        stage.backups = folder
        for name, source in zip(names, (runtime.backups / filename, runtime.backups / (filename + ".json"), Path(ledger))):
            shutil.copyfile(source, folder / name)
            (folder / name).chmod(0o600)
        # Authenticate the copied inputs, not files that may have changed while copying.
        verify(stage, folder / filename)
        verify_ledger(stage, folder / ledger_name, ledger_digest, m["created_at"])
        if sum((folder / name).stat().st_size for name in names) > MAX_BYTES - 65536:
            raise OperationError("encrypted offsite bundle exceeds the 1 GiB transfer limit")
        archive, encrypted = folder / "bundle.tar", folder / "bundle.enc"
        with tarfile.open(archive, "w", format=tarfile.USTAR_FORMAT) as output:
            for name in names:
                output.add(folder / name, arcname=name, recursive=False)
        crypt(runtime, archive, encrypted)
        digest, size = file_digest(encrypted), encrypted.stat().st_size
        if size > MAX_BYTES:
            raise OperationError("encrypted offsite bundle exceeds the 1 GiB transfer limit")
        remote = move(runtime, executable, credentials, "put", encrypted, "albums/recovery/" + nonce + ".enc", digest, size)
    receipt = {"scope": SCOPE, "database": runtime.config["MYSQL_DATABASE"], "published_at": int(time.time()),
               "snapshot": filename, "snapshot_created_at": m["created_at"], "ledger": ledger_name,
               "ledger_sha256": ledger_digest, "remote": remote}
    path = runtime.backups / ("albums-offsite-" + nonce + ".json")
    atomic_json(path, {"receipt": receipt, "hmac_sha256": manifest_signature(backup_secret(runtime), receipt)})
    return publish_receipt(runtime, executable, credentials, path, file_digest(path))


def read_receipt(runtime, path, digest):
    try:
        path = Path(path)
        if path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077 or path.stat().st_size > 65536:
            raise OperationError("invalid private offsite receipt")
        with path.open("rb") as source:
            data = source.read(65537)
        if len(data) > 65536 or not isinstance(digest, str) or not hmac.compare_digest(hashlib.sha256(data).hexdigest(), digest):
            raise OperationError("offsite receipt digest mismatch")
        envelope = json.loads(data)
        r = envelope["receipt"]
        if not hmac.compare_digest(manifest_signature(backup_secret(runtime), r), envelope["hmac_sha256"]):
            raise OperationError("offsite receipt authentication failed")
        if (r["scope"] != SCOPE or r["database"] != runtime.config["MYSQL_DATABASE"]
                or not re.fullmatch(r"albums-[0-9a-f]{32}\.sql\.gz\.enc", r["snapshot"])
                or not re.fullmatch(r"albums-deletions-[0-9a-f]{32}\.json", r["ledger"])
                or type(r["snapshot_created_at"]) is not int or type(r["published_at"]) is not int
                or not time.time() - RETENTION < r["snapshot_created_at"] <= r["published_at"] <= time.time() + 300):
            raise OperationError("offsite receipt scope, age or filenames rejected")
        return r
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise OperationError("offsite receipt missing or invalid") from error


def publish_receipt(runtime, executable, credentials, path, digest):
    configure(runtime)
    r = read_receipt(runtime, path, digest)
    key = r["remote"]["Key"]
    if not re.fullmatch(r"albums/recovery/[0-9a-f]{32}\.enc", key):
        raise OperationError("invalid bundle key in publication receipt")
    # Retrying only the receipt must not re-upload the encrypted bundle. Keep
    # the local signed receipt even when the provider response is uncertain.
    with tempfile.TemporaryDirectory(prefix=".albums-receipt-", dir=runtime.backups) as directory:
        copied = Path(directory) / "receipt.json"
        shutil.copyfile(path, copied)
        copied.chmod(0o600)
        read_receipt(runtime, copied, digest)
        remote = move(runtime, executable, credentials, "put", copied, key.removesuffix(".enc") + ".receipt.json", digest, copied.stat().st_size, Bucket=r["remote"]["Bucket"], Region=r["remote"]["Region"])
        if remote["Bucket"] != r["remote"]["Bucket"] or remote["Region"] != r["remote"]["Region"]:
            raise OperationError("receipt and bundle destinations differ")
    return runtime.record("albums-offsite-publish", receipt=str(path), receipt_sha256=digest,
                          remote=r["remote"], receipt_remote=remote, remotely_read_verified=True,
                          latest_checkpoint_proven=False)


def fetch(runtime, executable, credentials, receipt, receipt_digest):
    configure(runtime)
    r = read_receipt(runtime, receipt, receipt_digest)
    remote = r["remote"]
    # Never overwrite local snapshots or use archive-selected paths.
    folder = Path(tempfile.mkdtemp(prefix="albums-recovered-", dir=runtime.backups))
    try:
        encrypted, raw = folder / "bundle.enc", folder / "bundle.tar"
        move(runtime, executable, credentials, "get", encrypted, remote["Key"], remote["SHA256"], remote["Bytes"],
             Version=remote["Version"], Bucket=remote["Bucket"], Region=remote["Region"])
        if encrypted.stat().st_size != remote["Bytes"] or file_digest(encrypted) != remote["SHA256"]:
            raise OperationError("downloaded offsite content mismatch")
        crypt(runtime, encrypted, raw, decrypt=True)
        expected = {r["snapshot"], r["snapshot"] + ".json", r["ledger"]}
        seen = set()
        total = 0
        with tarfile.open(raw, "r|") as archive:
            for member in archive:
                if (member.name not in expected or member.name in seen or member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE) or member.sparse is not None
                        or member.size < 0 or total + member.size > MAX_BYTES):
                    raise OperationError("unexpected recovery archive member")
                with archive.extractfile(member) as source, (folder / member.name).open("xb") as output:
                    os.chmod(output.name, 0o600)
                    shutil.copyfileobj(source, output)
                seen.add(member.name)
                total += member.size
        if seen != expected:
            raise OperationError("incomplete recovery bundle")
        stage = copy.copy(runtime)
        stage.backups = folder
        m = verify(stage, folder / r["snapshot"])
        if m["created_at"] != r["snapshot_created_at"]:
            raise OperationError("snapshot differs from publication receipt")
        verify_ledger(stage, folder / r["ledger"], r["ledger_sha256"], m["created_at"])
        encrypted.unlink()
        raw.unlink()
        return runtime.record("albums-offsite-fetch", directory=str(folder), snapshot=r["snapshot"],
                              deletion_ledger=str(folder / r["ledger"]), deletion_ledger_sha256=r["ledger_sha256"],
                              latest_checkpoint_proven=False, recovery_ready=False)
    except BaseException:
        shutil.rmtree(folder)
        raise
