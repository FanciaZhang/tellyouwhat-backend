"""Authenticated deletion checkpoints for offline, quarantined recovery."""
import datetime
import hmac
import hashlib
import json
from pathlib import Path
import re
import secrets
import time
import uuid

from ops_common import OperationError, atomic_json
from ops_backup import backup_secret, file_digest, manifest_signature

SCOPE = "albums-deletion-ledger-v1"
MAX_RECORDS = 100000


def validate(entries):
    if not isinstance(entries, list) or len(entries) > MAX_RECORDS:
        raise OperationError("invalid deletion ledger size")
    owners, requests = set(), set()
    for row in entries:
        if not isinstance(row, list) or len(row) != 4 or not all(isinstance(v, str) for v in row):
            raise OperationError("invalid deletion ledger row")
        owner, request, digest, stamp = row
        try:
            ids = [uuid.UUID(owner), uuid.UUID(request)]
            valid = all(v.int and str(v) == raw for v, raw in zip(ids, (owner, request)))
            date = datetime.datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%S.%fZ").replace(tzinfo=datetime.timezone.utc)
        except ValueError as error:
            raise OperationError("invalid deletion ledger identity or date") from error
        if (not valid or not re.fullmatch(r"[0-9a-f]{64}", digest)
                or not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z", stamp)
                or date.year < 2000 or date.timestamp() > time.time() + 300
                or owner in owners or request in requests):
            raise OperationError("conflicting deletion ledger row")
        owners.add(owner)
        requests.add(request)
    return entries


def publish(runtime, sql, credentials):
    # Full snapshots avoid missing a transaction that commits behind an allocated
    # sequence watermark. Publication is not proof that an offsite copy exists.
    exported_at = int(time.time())
    data = runtime.execute("albums-deletion-ledger-export", sql + ["--execute=SELECT account_id,request_id,LOWER(HEX(authorizing_session_sha256)),DATE_FORMAT(requested_at,'%Y-%m-%dT%H:%i:%s.%fZ') FROM album_deletion_ledger UNION ALL SELECT 'invalid','','','' WHERE EXISTS (SELECT 1 FROM album_account_deletions d LEFT JOIN album_deletion_ledger l ON l.account_id=d.account_id WHERE l.account_id IS NULL OR l.request_id<>d.request_id OR l.authorizing_session_sha256<>d.authorizing_session_sha256 OR l.requested_at<>d.requested_at) ORDER BY account_id LIMIT 100001"], env=credentials)
    entries = validate([line.split("\t") for line in data.decode().splitlines()])
    payload = {"scope": SCOPE, "database": runtime.config["MYSQL_DATABASE"], "exported_at": exported_at, "entries": entries}
    filename = "albums-deletions-" + secrets.token_hex(16) + ".json"
    path = runtime.backups / filename
    atomic_json(path, {"payload": payload, "hmac_sha256": manifest_signature(backup_secret(runtime), payload)})
    return runtime.record("albums-deletion-ledger", filename=filename, sha256=file_digest(path), records=len(entries), offsite=False)


def verify(runtime, path, digest, minimum_time):
    if not path or not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise OperationError("recovery requires an explicitly pinned deletion ledger")
    path = Path(path)
    try:
        if path.is_symlink() or path.stat().st_mode & 0o077 or path.stat().st_size > 64 * 1024 * 1024:
            raise OperationError("deletion ledger file is not private or bounded")
        with path.open("rb") as source:
            data = source.read(64 * 1024 * 1024 + 1)
        if len(data) > 64 * 1024 * 1024 or not hmac.compare_digest(hashlib.sha256(data).hexdigest(), digest):
            raise OperationError("deletion ledger checkpoint mismatch")
        envelope = json.loads(data)
        p = envelope["payload"]
        if not hmac.compare_digest(manifest_signature(backup_secret(runtime), p), envelope["hmac_sha256"]):
            raise OperationError("deletion ledger authentication failed")
        if (p["scope"] != SCOPE or p["database"] != runtime.config["MYSQL_DATABASE"]
                or type(p["exported_at"]) is not int or p["exported_at"] < minimum_time
                or p["exported_at"] > time.time() + 300):
            raise OperationError("deletion ledger scope or snapshot order mismatch")
        return validate(p["entries"])
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise OperationError("invalid deletion ledger checkpoint") from error


def restore_fences(runtime, sql, entries):
    validate(entries)
    # All values are canonical UUID/hex/UTC date strings, never arbitrary SQL.
    batches = []
    for start in range(0, len(entries), 500):
        values = ",".join("('%s','%s',UNHEX('%s'),'%s')" % (a, r, h, d.replace("T", " ").removesuffix("Z")) for a, r, h, d in entries[start:start + 500])
        batches.append("INSERT INTO recovery_deletions VALUES " + values + ";")
    insert = "\n".join(batches)
    script = """
CREATE TEMPORARY TABLE recovery_deletions LIKE album_deletion_ledger;
CREATE TEMPORARY TABLE recovery_guard (ok INT NOT NULL CHECK(ok=1));
""" + insert + """
START TRANSACTION;
INSERT INTO recovery_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_deletion_ledger old LEFT JOIN recovery_deletions n ON n.account_id=old.account_id
 WHERE n.account_id IS NULL OR n.request_id<>old.request_id OR n.authorizing_session_sha256<>old.authorizing_session_sha256 OR n.requested_at<>old.requested_at
),0,1);
INSERT INTO recovery_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_account_deletions old LEFT JOIN recovery_deletions n ON n.account_id=old.account_id
 WHERE n.account_id IS NULL OR n.request_id<>old.request_id OR n.authorizing_session_sha256<>old.authorizing_session_sha256 OR n.requested_at<>old.requested_at
),0,1);
INSERT INTO album_deletion_ledger SELECT n.* FROM recovery_deletions n LEFT JOIN album_deletion_ledger old ON old.account_id=n.account_id WHERE old.account_id IS NULL;
INSERT INTO album_account_deletions(account_id,request_id,authorizing_session_sha256,requested_at)
 SELECT n.* FROM recovery_deletions n JOIN album_accounts a ON a.account_id=n.account_id LEFT JOIN album_account_deletions old ON old.account_id=n.account_id WHERE old.account_id IS NULL;
UPDATE album_accounts a JOIN recovery_deletions n ON n.account_id=a.account_id SET a.state='deleting';
UPDATE album_storage_accounts a JOIN recovery_deletions n ON n.account_id=a.owner_id SET a.deletion_requested_at=n.requested_at;
UPDATE album_apple_grants g JOIN recovery_deletions n ON n.account_id=g.account_id SET g.revoked_at=COALESCE(g.revoked_at,n.requested_at);
UPDATE album_retention_reminders m JOIN recovery_deletions n ON n.account_id=m.owner_id SET m.state='cancelled',m.lease_token=NULL,m.lease_until=NULL WHERE m.state='pending';
INSERT INTO album_deletion_apple_jobs(grant_id,account_id,request_id,available_at)
 SELECT g.grant_id,g.account_id,n.request_id,n.requested_at FROM album_apple_grants g JOIN recovery_deletions n ON n.account_id=g.account_id LEFT JOIN album_deletion_apple_jobs j ON j.grant_id=g.grant_id WHERE j.grant_id IS NULL;
INSERT INTO album_deletion_video_jobs(job_id,account_id,request_id,available_at)
 SELECT v.job_id,v.owner_id,n.request_id,n.requested_at FROM album_video_jobs v JOIN recovery_deletions n ON n.account_id=v.owner_id LEFT JOIN album_deletion_video_jobs j ON j.job_id=v.job_id WHERE j.job_id IS NULL AND v.spec_json IS NOT NULL;
INSERT INTO album_deletion_subscription_jobs(environment,original_transaction_id,account_id,request_id,target_token,available_at)
 SELECT c.environment,c.original_transaction_id,c.owner_id,n.request_id,a.storekit_account_token,n.requested_at FROM album_cloud_subscriptions c JOIN recovery_deletions n ON n.account_id=c.owner_id JOIN album_accounts a ON a.account_id=c.owner_id
 LEFT JOIN album_deletion_subscription_jobs j ON j.environment=c.environment AND j.original_transaction_id=c.original_transaction_id WHERE j.original_transaction_id IS NULL;
UPDATE album_deletion_subscription_jobs j JOIN recovery_deletions n ON n.account_id=j.account_id
 SET j.acknowledged_at=NULL,j.verified_transaction_id=NULL,j.verified_signed_at=NULL,j.lease_token=NULL,j.lease_until=NULL,j.available_at=n.requested_at;
INSERT INTO recovery_guard SELECT IF(EXISTS(SELECT 1 FROM album_accounts a JOIN recovery_deletions n ON n.account_id=a.account_id WHERE a.state<>'deleting'),0,1);
COMMIT;
"""
    runtime.execute("albums-restore-deletion-fences", sql, input=script.encode())
