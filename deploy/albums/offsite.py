"""Encrypted, version-pinned COS recovery bundles; no latest-checkpoint or cutover claim."""
import copy
import hashlib
import hmac
from itertools import islice
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import stat
import tarfile
import time

from backup import configure, verify, RETENTION, staging, cleanup_staging, _remove_recovered_owner
from deletion_ledger import verify as verify_ledger
from ops_backup import backup_secret, crypt, file_digest, manifest_signature
from ops_common import OperationError, atomic_json

SCOPE = "albums-offsite-bundle-v1"
EXPIRY_SCOPE = "albums-offsite-expiry-v1"
MAX_BYTES = 1 << 30
MULTIPART_PROTOCOL = "cos-multipart-v1"

def destination(credentials):
    try:
        with os.fdopen(os.open(credentials, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as source:
            info = os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > 65536:
                raise OperationError("offsite destination requires private configuration")
            config = json.loads(source.read(65537))
        if (not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,48}-[0-9]{5,20}", config["Bucket"])
                or not re.fullmatch(r"[a-z]{2}-[a-z]+(?:-[a-z]+)?", config["Region"])):
            raise OperationError("invalid offsite destination")
        return {k: config[k] for k in ("Bucket", "Region")}
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise OperationError("invalid private offsite configuration") from error


def move(runtime, executable, credentials, operation, path, key, digest, size, **pinned):
    if not executable or not credentials:
        raise OperationError("offsite transfer requires an executable and dedicated private COS configuration")
    request = dict(Operation=operation, Path=str(path), Key=key, SHA256=digest, Bytes=size, **pinned)
    result = json.loads(runtime.execute("albums-backup-transfer", [str(executable), str(credentials)], input=json.dumps(request).encode(), timeout=960))
    if (not isinstance(result, dict) or result.get("Key") != key or result.get("SHA256") != digest or result.get("Bytes") != size
            or not isinstance(result.get("Version"), str) or result["Version"] == "null"
            or not re.fullmatch(r"[A-Za-z0-9_.~-]{1,1024}", result["Version"])
            or any(result.get(k) != v for k, v in pinned.items())):
        raise OperationError("offsite transfer did not return the pinned content")
    return result


def initiate(runtime, executable, credentials, key, target):
    if not executable or not credentials:
        raise OperationError("offsite initiation requires a transfer executable and private COS configuration")
    request = dict(Operation="initiate", Key=key, **target)
    result = json.loads(runtime.execute("albums-backup-initiate", [str(executable), str(credentials)],
                                       input=json.dumps(request).encode(), timeout=960))
    if (not isinstance(result, dict) or set(result) != {"Key", "Bucket", "Region", "UploadID"}
            or result["Key"] != key or any(result[k] != v for k, v in target.items())
            or not isinstance(result["UploadID"], str) or result["UploadID"] == "null" or not re.fullmatch(r"[A-Za-z0-9_-]{1,1024}", result["UploadID"])):
        raise OperationError("offsite initiation did not return a bound upload session")
    return result["UploadID"]


def publish(runtime, executable, credentials, filename, ledger, ledger_digest):
    configure(runtime)
    cleanup_staging(runtime)
    m = verify(runtime, runtime.backups / filename)
    if m["created_at"] <= time.time() - RETENTION:
        raise OperationError("Albums backup is outside retention")
    verify_ledger(runtime, ledger, ledger_digest, m["created_at"])
    ledger_name = Path(ledger).name
    if not re.fullmatch(r"albums-deletions-[0-9a-f]{32}\.json", ledger_name):
        raise OperationError("invalid deletion ledger filename")
    target = destination(credentials)
    names = (filename, filename + ".json", ledger_name)
    nonce = secrets.token_hex(16)
    with staging(runtime, "offsite", (*names, "bundle.tar", "bundle.enc")) as folder:
        stage = copy.copy(runtime)
        stage.backups = folder
        for name, source in zip(names, (runtime.backups / filename, runtime.backups / (filename + ".json"), Path(ledger))):
            (folder / name).touch(mode=0o600, exist_ok=False)
            shutil.copyfile(source, folder / name)
            (folder / name).chmod(0o600)
        # Authenticate the copied inputs, not files that may have changed while copying.
        verify(stage, folder / filename)
        verify_ledger(stage, folder / ledger_name, ledger_digest, m["created_at"])
        if sum((folder / name).stat().st_size for name in names) > MAX_BYTES - 65536:
            raise OperationError("encrypted offsite bundle exceeds the 1 GiB transfer limit")
        archive, encrypted = folder / "bundle.tar", folder / "bundle.enc"
        archive.touch(mode=0o600, exist_ok=False)
        encrypted.touch(mode=0o600, exist_ok=False)
        with tarfile.open(archive, "w", format=tarfile.USTAR_FORMAT) as output:
            for name in names:
                output.add(folder / name, arcname=name, recursive=False)
        crypt(runtime, archive, encrypted)
        digest, size = file_digest(encrypted), encrypted.stat().st_size
        if size > MAX_BYTES:
            raise OperationError("encrypted offsite bundle exceeds the 1 GiB transfer limit")
        key = "albums/recovery/" + nonce + ".enc"
        receipt = {"scope": SCOPE, "database": runtime.config["MYSQL_DATABASE"], "published_at": int(time.time()),
                   "snapshot": filename, "snapshot_created_at": m["created_at"], "ledger": ledger_name,
                   "ledger_sha256": ledger_digest, "publication_confirmed": False,
                   "upload_protocol": MULTIPART_PROTOCOL, "receipt_upload_id": "",
                   "remote": dict(Key=key, Version="", UploadID="", SHA256=digest, Bytes=size, **target)}
        path = runtime.backups / ("albums-offsite-" + nonce + ".json")
        # Persist intent before initiation, then the session before any part can be sent.
        atomic_json(path, {"receipt": receipt, "hmac_sha256": manifest_signature(backup_secret(runtime), receipt)})
        receipt["remote"]["UploadID"] = initiate(runtime, executable, credentials, key, target)
        atomic_json(path, {"receipt": receipt, "hmac_sha256": manifest_signature(backup_secret(runtime), receipt)})
        remote = move(runtime, executable, credentials, "put", encrypted, key, digest, size, UploadID=receipt["remote"]["UploadID"], **target)
        receipt.update(remote=remote, publication_confirmed=True, published_at=int(time.time()))
        atomic_json(path, {"receipt": receipt, "hmac_sha256": manifest_signature(backup_secret(runtime), receipt)})
    return publish_receipt(runtime, executable, credentials, path, file_digest(path))


def read_receipt(runtime, path, digest, *, expired=False):
    if not isinstance(digest, str) or type(expired) is not bool:
        raise OperationError("offsite receipt requires an explicit digest")
    receipt = _read_receipt(runtime, path, digest, expired=expired)[0]
    confirmation = _expiry_path(runtime, receipt)
    if not expired and (confirmation.exists() or confirmation.is_symlink()):
        raise OperationError("expired offsite publication cannot be restored or resumed")
    return receipt


def _read_receipt(runtime, path, digest, *, expired):
    try:
        path = Path(path)
        with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as source:
            info = os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > 65536:
                raise OperationError("invalid private offsite receipt")
            data = source.read(65537)
        if len(data) > 65536 or digest is not None and not hmac.compare_digest(hashlib.sha256(data).hexdigest(), digest):
            raise OperationError("offsite receipt digest mismatch")
        envelope = json.loads(data)
        r = envelope["receipt"]
        if not hmac.compare_digest(manifest_signature(backup_secret(runtime), r), envelope["hmac_sha256"]):
            raise OperationError("offsite receipt authentication failed")
        if (r["scope"] != SCOPE or r["database"] != runtime.config["MYSQL_DATABASE"]
                or not re.fullmatch(r"albums-[0-9a-f]{32}\.sql\.gz\.enc", r["snapshot"])
                or not re.fullmatch(r"albums-deletions-[0-9a-f]{32}\.json", r["ledger"])
                or type(r["snapshot_created_at"]) is not int or type(r["published_at"]) is not int
                or not 0 < r["snapshot_created_at"] <= r["published_at"] <= time.time() + 300
                or expired is not None and (r["snapshot_created_at"] <= time.time() - RETENTION) != expired):
            raise OperationError("offsite receipt scope, age or filenames rejected")
        remote = r["remote"]
        confirmed = r.get("publication_confirmed", True)
        if (type(confirmed) is not bool or not confirmed and expired is False
                or not re.fullmatch(r"albums/recovery/[0-9a-f]{32}\.enc", remote["Key"])
                or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,48}-[0-9]{5,20}", remote["Bucket"])
                or not re.fullmatch(r"[a-z]{2}-[a-z]+(?:-[a-z]+)?", remote["Region"])
                or not re.fullmatch(r"[0-9a-f]{64}", remote["SHA256"])
                or type(remote["Bytes"]) is not int or not 0 < remote["Bytes"] <= MAX_BYTES
                or confirmed and (remote["Version"] == "null" or not re.fullmatch(r"[A-Za-z0-9_.~-]{1,1024}", remote["Version"]))
                or not confirmed and remote["Version"] != ""):
            raise OperationError("offsite receipt destination or confirmation rejected")
        protocol = r.get("upload_protocol")
        if protocol not in (None, MULTIPART_PROTOCOL):
            raise OperationError("unknown offsite upload protocol")
        if protocol == MULTIPART_PROTOCOL:
            upload_id, receipt_id = remote.get("UploadID"), r.get("receipt_upload_id")
            if (not isinstance(upload_id, str) or not isinstance(receipt_id, str)
                    or upload_id == "null" or receipt_id == "null"
                    or (confirmed or upload_id) and not re.fullmatch(r"[A-Za-z0-9_-]{1,1024}", upload_id)
                    or receipt_id and not re.fullmatch(r"[A-Za-z0-9_-]{1,1024}", receipt_id)):
                raise OperationError("invalid offsite upload session binding")
        return r, hashlib.sha256(data).hexdigest()
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise OperationError("offsite receipt missing or invalid") from error


def _expiry_path(runtime, receipt):
    return runtime.backups / ("albums-expired-" + receipt["remote"]["Key"].split("/")[-1].removesuffix(".enc") + ".json")


def _read_expiry_confirmation(runtime, path, receipt, digest):
    try:
        with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as source:
            info = os.fstat(source.fileno())
            if (not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_nlink != 1
                    or info.st_uid != runtime.backups.stat().st_uid or info.st_size > 65536):
                raise OperationError("offsite expiry confirmation is not private or bounded")
            envelope = json.loads(source.read(65537))
        proof = envelope["expiry"]
        if (set(envelope) != {"expiry", "hmac_sha256"}
                or not hmac.compare_digest(manifest_signature(backup_secret(runtime), proof), envelope["hmac_sha256"])):
            raise OperationError("offsite expiry confirmation authentication failed")
        if (set(proof) != {"scope", "database", "receipt_sha256", "confirmed_at", "remote"}
                or proof["scope"] != EXPIRY_SCOPE or proof["database"] != runtime.config["MYSQL_DATABASE"]
                or proof["receipt_sha256"] != digest or receipt.get("upload_protocol") != MULTIPART_PROTOCOL
                or type(proof["confirmed_at"]) is not int
                or not receipt["snapshot_created_at"] + RETENTION <= proof["confirmed_at"] <= time.time() + 300):
            raise OperationError("offsite expiry confirmation scope, pin or age rejected")
        remote = proof["remote"]
        if (set(remote) != {"Key", "Bucket", "Region", "VersionsRemoved", "SessionsClosed", "Empty", "UploadsEmpty"}
                or any(remote[k] != receipt["remote"][k] for k in ("Key", "Bucket", "Region"))
                or remote["Empty"] is not True or remote["UploadsEmpty"] is not True
                or type(remote["VersionsRemoved"]) is not int or not 0 <= remote["VersionsRemoved"] <= 10000
                or type(remote["SessionsClosed"]) is not int or not 0 <= remote["SessionsClosed"] <= 10002):
            raise OperationError("offsite expiry confirmation did not record empty inventories")
        return proof
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise OperationError("offsite expiry confirmation missing or invalid") from error


def _expiry_confirmations(runtime, receipts):
    paths = sorted(islice(runtime.backups.glob("albums-expired-*"), 10001))
    if len(paths) > 10000:
        raise OperationError("offsite expiry confirmation limit rejected")
    result = {}
    for path in paths:
        if not re.fullmatch(r"albums-expired-[0-9a-f]{32}\.json", path.name):
            raise OperationError("offsite expiry confirmation filename rejected")
        key = "albums/recovery/" + path.name[15:-5] + ".enc"
        if key not in receipts:
            raise OperationError("offsite expiry confirmation has no matching publication")
        receipt, digest = receipts[key]
        result[key] = _read_expiry_confirmation(runtime, path, receipt, digest)
    return result


def audit(runtime, executable, credentials):
    configure(runtime)
    if not executable or not credentials:
        raise OperationError("offsite audit requires a transfer executable and private COS configuration")
    target = destination(credentials)
    receipts = {}
    for path in runtime.backups.glob("albums-offsite-*"):
        if len(receipts) >= 10000 or not re.fullmatch(r"albums-offsite-[0-9a-f]{32}\.json", path.name):
            raise OperationError("offsite audit receipt namespace or limit rejected")
        # Audit authenticates all ages and unresolved intents; restore keeps its explicit pin and age gate.
        r, digest = _read_receipt(runtime, path, None, expired=None)
        key = "albums/recovery/" + path.name[15:-5] + ".enc"
        if r["remote"]["Key"] != key or any(r["remote"][k] != v for k, v in target.items()):
            raise OperationError("offsite audit receipt filename or destination rejected")
        receipts[key] = (r, digest)
    confirmations = _expiry_confirmations(runtime, receipts)
    request = dict(Operation="inventory", Key="albums/recovery/", **target)
    result = json.loads(runtime.execute("albums-backup-inventory", [str(executable), str(credentials)],
                                       input=json.dumps(request).encode(), timeout=960))
    if (not isinstance(result, dict) or set(result) != {"Bucket", "Region", "Prefix", "Versions", "Uploads"}
            or any(result.get(k) != v for k, v in target.items()) or result.get("Prefix") != "albums/recovery/"
            or any(not isinstance(result.get(k), list) or len(result[k]) > 10000 for k in ("Versions", "Uploads"))):
        raise OperationError("offsite audit incomplete or invalid inventory")
    seen, keys, present, receipt_keys = set(), set(), set(), set()
    unexpected, size_mismatches = [], []
    for v in result["Versions"]:
        if (not isinstance(v, dict) or set(v) != {"Key", "Version", "Bytes", "DeleteMarker"}
                or not isinstance(v["Key"], str) or not re.fullmatch(r"albums/recovery/[0-9a-f]{32}(?:\.enc|\.receipt\.json)", v["Key"])
                or not isinstance(v["Version"], str) or v["Version"] == "null" or not re.fullmatch(r"[A-Za-z0-9_.~-]{1,1024}", v["Version"])
                or type(v["Bytes"]) is not int or v["Bytes"] < 0 or type(v["DeleteMarker"]) is not bool
                or v["DeleteMarker"] and v["Bytes"] != 0 or (v["Key"], v["Version"]) in seen):
            raise OperationError("offsite audit invalid version inventory")
        seen.add((v["Key"], v["Version"]))
        key = v["Key"].removesuffix(".receipt.json") + ".enc" if v["Key"].endswith(".receipt.json") else v["Key"]
        keys.add(key)
        if not v["DeleteMarker"] and key != v["Key"]:
            receipt_keys.add(key)
        if v["DeleteMarker"] or key not in receipts or key != v["Key"]:
            continue
        r = receipts[key][0]["remote"]
        present.add((key, v["Version"]))
        if r["Version"] != v["Version"]:
            unexpected.append(v)
        if r["Bytes"] != v["Bytes"]:
            size_mismatches.append(v)
    uploads = set()
    for u in result["Uploads"]:
        if (not isinstance(u, dict) or set(u) != {"Key", "UploadID"}
                or not isinstance(u["Key"], str) or not re.fullmatch(r"albums/recovery/[0-9a-f]{32}(?:\.enc|\.receipt\.json)", u["Key"])
                or not isinstance(u["UploadID"], str) or u["UploadID"] == "null" or not re.fullmatch(r"[A-Za-z0-9_.~-]{1,1024}", u["UploadID"])
                or (u["Key"], u["UploadID"]) in uploads):
            raise OperationError("offsite audit invalid upload inventory")
        uploads.add((u["Key"], u["UploadID"]))
        keys.add(u["Key"].removesuffix(".receipt.json") + ".enc" if u["Key"].endswith(".receipt.json") else u["Key"])
    cleared = confirmations.keys() - keys
    reappeared = sorted(confirmations.keys() & keys)
    unconfirmed = sorted(k for k, (r, _) in receipts.items() if k not in cleared and not r.get("publication_confirmed", True))
    missing = sorted(k for k, (r, _) in receipts.items() if k not in cleared and r.get("publication_confirmed", True) and (k, r["remote"]["Version"]) not in present)
    missing_receipts = sorted(k for k, (r, _) in receipts.items() if k not in cleared and r.get("publication_confirmed", True) and k not in receipt_keys)
    unknown = sorted(keys - receipts.keys())
    # Metadata enumeration neither reads object bodies nor closes earlier direct PUTs.
    return runtime.record("albums-offsite-audit", inventory=result, receipts=len(receipts), unregistered_keys=unknown,
                          unconfirmed_publications=unconfirmed, missing_confirmed_bundle_versions=missing,
                          missing_remote_receipts=missing_receipts,
                          confirmed_expired_keys=sorted(cleared), reappeared_expired_keys=reappeared,
                          unexpected_bundle_versions=unexpected, bundle_size_mismatches=size_mismatches,
                          has_unresolved_items=bool(uploads or unknown or unconfirmed or missing or missing_receipts or unexpected or size_mismatches or reappeared),
                          writers_quiescent=False, account_erasure_complete=False)


def prune_expired(runtime, executable, credentials):
    configure(runtime)
    if not executable or not credentials:
        raise OperationError("offsite pruning requires a transfer executable and dedicated cleanup credentials")
    target = destination(credentials)
    cutoff = time.time() - RETENTION
    pending, inspected, receipts = [], 0, {}
    # Preflight the whole set before any deletion; the caller holds the shared operations lock.
    paths = sorted(islice(runtime.backups.glob("albums-offsite-*"), 10001))
    if len(paths) > 10000:
        raise OperationError("offsite pruning receipt limit rejected")
    for path in paths:
        if not re.fullmatch(r"albums-offsite-[0-9a-f]{32}\.json", path.name):
            raise OperationError("offsite pruning receipt namespace or limit rejected")
        receipt, digest = _read_receipt(runtime, path, None, expired=None)
        remote = receipt["remote"]
        if (remote["Key"] != "albums/recovery/" + path.name[15:-5] + ".enc"
                or any(remote[k] != v for k, v in target.items())):
            raise OperationError("offsite pruning receipt filename or destination rejected")
        inspected += 1
        receipts[remote["Key"]] = (receipt, digest)
        if receipt["snapshot_created_at"] <= cutoff:
            if receipt.get("upload_protocol") != MULTIPART_PROTOCOL:
                raise OperationError("legacy offsite writes require reconciliation before batch pruning")
            pending.append((path, digest))
    _expiry_confirmations(runtime, receipts)
    results = []
    for path, digest in pending:
        results.append(expire(runtime, executable, credentials, path, digest))
    return runtime.record("albums-offsite-prune", inspected=inspected, expired=len(results), results=results,
                          local_receipts_retained=True, writers_quiescent=False, account_erasure_complete=False)


def expire(runtime, executable, credentials, path, digest):
    configure(runtime)
    r = read_receipt(runtime, path, digest, expired=True)
    confirmation = _expiry_path(runtime, r)
    if confirmation.exists() or confirmation.is_symlink():
        _read_expiry_confirmation(runtime, confirmation, r, digest)
    if not executable or not credentials:
        raise OperationError("offsite expiry requires a transfer executable and dedicated cleanup credentials")
    remote = r["remote"]
    request = {k: remote[k] for k in ("Key", "Bucket", "Region", "Bytes", "SHA256")}
    request.update(Operation="expire", ReceiptSHA256=digest, ReceiptBytes=Path(path).stat().st_size)
    if r.get("upload_protocol") == MULTIPART_PROTOCOL:
        request.update(Protocol=MULTIPART_PROTOCOL, UploadID=remote["UploadID"], ReceiptUploadID=r["receipt_upload_id"])
    result = json.loads(runtime.execute("albums-backup-expire", [str(executable), str(credentials)],
                                       input=json.dumps(request).encode(), timeout=960))
    if (not isinstance(result, dict) or result.get("Empty") is not True or type(result.get("VersionsRemoved")) is not int
            or not 0 <= result["VersionsRemoved"] <= 10000
            or any(result.get(k) != remote[k] for k in ("Key", "Bucket", "Region"))
            or r.get("upload_protocol") == MULTIPART_PROTOCOL and (result.get("UploadsEmpty") is not True
                or type(result.get("SessionsClosed")) is not int or not 0 <= result["SessionsClosed"] <= 10002)):
        raise OperationError("offsite expiry did not prove an empty version inventory")
    if r.get("upload_protocol") == MULTIPART_PROTOCOL:
        at = int(time.time())
        if at < r["snapshot_created_at"] + RETENTION:
            raise OperationError("offsite expiry clock moved before retention boundary")
        proof = dict(scope=EXPIRY_SCOPE, database=runtime.config["MYSQL_DATABASE"], receipt_sha256=digest, confirmed_at=at,
                     remote={k: result[k] for k in ("Key", "Bucket", "Region", "VersionsRemoved", "SessionsClosed", "Empty", "UploadsEmpty")})
        if confirmation.exists() or confirmation.is_symlink():
            _read_expiry_confirmation(runtime, confirmation, r, digest)
        atomic_json(confirmation, dict(expiry=proof, hmac_sha256=manifest_signature(backup_secret(runtime), proof)))
    # Retain the authenticated local receipt for retries and audit. This does not
    # prove absence of other bundles, snapshots or an account's historical data.
    return runtime.record("albums-offsite-expire", receipt=str(path), receipt_sha256=digest,
                          remote=result, expiry_confirmation=str(confirmation) if r.get("upload_protocol") == MULTIPART_PROTOCOL else None,
                          account_erasure_complete=False)


def publish_receipt(runtime, executable, credentials, path, digest):
    configure(runtime)
    cleanup_staging(runtime)
    r = read_receipt(runtime, path, digest)
    key = r["remote"]["Key"]
    if not re.fullmatch(r"albums/recovery/[0-9a-f]{32}\.enc", key):
        raise OperationError("invalid bundle key in publication receipt")
    if r.get("upload_protocol") != MULTIPART_PROTOCOL:
        raise OperationError("legacy receipt is read-only; publish a new recovery bundle instead")
    receipt_key = key.removesuffix(".enc") + ".receipt.json"
    if not r["receipt_upload_id"]:
        r["receipt_upload_id"] = initiate(runtime, executable, credentials, receipt_key,
                                          {k: r["remote"][k] for k in ("Bucket", "Region")})
        atomic_json(path, {"receipt": r, "hmac_sha256": manifest_signature(backup_secret(runtime), r)})
        digest = file_digest(path)
    # Retrying only the receipt must not re-upload the encrypted bundle. Keep
    # the local signed receipt even when the provider response is uncertain.
    with staging(runtime, "receipt", ("receipt.json",)) as folder:
        copied = folder / "receipt.json"
        copied.touch(mode=0o600, exist_ok=False)
        shutil.copyfile(path, copied)
        copied.chmod(0o600)
        read_receipt(runtime, copied, digest)
        remote = move(runtime, executable, credentials, "put", copied, receipt_key, digest, copied.stat().st_size,
                      UploadID=r["receipt_upload_id"], Bucket=r["remote"]["Bucket"], Region=r["remote"]["Region"])
        if remote["Bucket"] != r["remote"]["Bucket"] or remote["Region"] != r["remote"]["Region"]:
            raise OperationError("receipt and bundle destinations differ")
    return runtime.record("albums-offsite-publish", receipt=str(path), receipt_sha256=digest,
                          remote=r["remote"], receipt_remote=remote, remotely_read_verified=True,
                          latest_checkpoint_proven=False)


def fetch(runtime, executable, credentials, receipt, receipt_digest):
    configure(runtime)
    r = read_receipt(runtime, receipt, receipt_digest)
    remote = r["remote"]
    cleanup_staging(runtime)
    if shutil.disk_usage(runtime.backups).free < 3 * remote["Bytes"] + 64 * 1024**2:
        raise OperationError("insufficient space for the downloaded and decoded recovery bundle")
    # A complete verified result is handed off atomically; interrupted work remains leased staging.
    destination = runtime.backups / ("albums-recovered-" + secrets.token_hex(4))
    names = (r["snapshot"], r["snapshot"] + ".json", r["ledger"])
    with staging(runtime, "offsite", (*names, "bundle.enc", "bundle.tar"), destination=destination) as folder:
        encrypted, raw = folder / "bundle.enc", folder / "bundle.tar"
        move(runtime, executable, credentials, "get", encrypted, remote["Key"], remote["SHA256"], remote["Bytes"],
             Version=remote["Version"], Bucket=remote["Bucket"], Region=remote["Region"])
        if encrypted.stat().st_size != remote["Bytes"] or file_digest(encrypted) != remote["SHA256"]:
            raise OperationError("downloaded offsite content mismatch")
        raw.touch(mode=0o600, exist_ok=False)
        crypt(runtime, encrypted, raw, decrypt=True)
        expected = {r["snapshot"], r["snapshot"] + ".json", r["ledger"]}
        seen = set()
        total = 0
        with tarfile.open(raw, "r|") as archive:
            for member in archive:
                if (member.name not in expected or member.name in seen or member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE) or member.sparse is not None
                        or member.size < 0 or total + member.size > MAX_BYTES):
                    raise OperationError("unexpected recovery archive member")
                with archive.extractfile(member) as source, os.fdopen(os.open(folder / member.name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as output:
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
    _remove_recovered_owner(runtime, destination, names)
    return runtime.record("albums-offsite-fetch", directory=str(destination), snapshot=r["snapshot"],
                          deletion_ledger=str(destination / r["ledger"]), deletion_ledger_sha256=r["ledger_sha256"],
                          latest_checkpoint_proven=False, recovery_ready=False)
