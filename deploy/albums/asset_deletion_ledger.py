"""Per-asset fences in authenticated checkpoints; replay only into a quarantined drill."""
import datetime
import json
from pathlib import Path
import re
import time
import uuid

from ops_common import OperationError


def validate(entries):
    if not isinstance(entries, list) or len(entries) > 100000:
        raise OperationError("invalid asset deletion checkpoint size")
    assets, versions = set(), set()
    for row in entries:
        if not isinstance(row, list) or len(row) != 6:
            raise OperationError("invalid asset deletion row")
        owner, asset, revision, reason, stamp, ids = row
        try:
            valid_owner = isinstance(owner, str) and str(uuid.UUID(owner)) == owner and uuid.UUID(owner).int != 0
            date = datetime.datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%S.%fZ").replace(tzinfo=datetime.timezone.utc)
        except (ValueError, TypeError, AttributeError) as error:
            raise OperationError("invalid asset deletion identity or date") from error
        if (not valid_owner or not isinstance(asset, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,96}", asset)
                or type(revision) is not int or not 1 <= revision <= 9223372036854775807
                or reason not in ("trash", "retention") or date.year < 2000 or date.timestamp() > time.time() + 300
                or not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z", stamp)
                or not isinstance(ids, list) or not 1 <= len(ids) <= 1000 or (owner, asset) in assets):
            raise OperationError("invalid or conflicting asset deletion row")
        assets.add((owner, asset))
        for identity in ids:
            try:
                good = isinstance(identity, str) and str(uuid.UUID(identity)) == identity and uuid.UUID(identity).int != 0
            except (ValueError, AttributeError, TypeError):
                good = False
            if not good or (owner, identity) in versions or len(versions) >= 100000:
                raise OperationError("invalid or duplicated deleted version")
            versions.add((owner, identity))
    return entries


def export_query(runtime, sql, credentials):
    schema = runtime.execute("albums-asset-deletion-ledger-schema", sql + ["--execute=SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='album_asset_deletion_ledger'),EXISTS(SELECT 1 FROM schema_migrations WHERE name='0078_album_asset_deletion_ledger.sql')"], env=credentials).strip()
    if schema in (b"1\t0", b"1\t1"):
        query = "SELECT l.owner_id,l.asset_id,l.final_revision,l.reason,DATE_FORMAT(l.purged_at,'%Y-%m-%dT%H:%i:%s.%fZ'),l.version_ids FROM album_asset_deletion_ledger l UNION ALL SELECT 'invalid','',0,'','',JSON_ARRAY() WHERE EXISTS (SELECT 1 FROM album_object_purges p LEFT JOIN album_asset_deletion_ledger l ON l.owner_id=p.owner_id AND l.asset_id=p.asset_id WHERE p.state='completed' AND (l.owner_id IS NULL OR l.final_revision<>p.revision+1 OR l.reason<>p.reason OR l.purged_at<>p.completed_at)) ORDER BY owner_id,asset_id LIMIT 100001"
    elif schema == b"0\t0":
        # Read the same completed-purge evidence as 0078's backfill; missing feed/version evidence fails validation.
        query = "SELECT p.owner_id,p.asset_id,p.revision+1,IF(p.refunded_bytes>0,p.reason,'invalid'),DATE_FORMAT(p.completed_at,'%Y-%m-%dT%H:%i:%s.%fZ'),JSON_ARRAYAGG(c.version_id) FROM album_object_purges p LEFT JOIN album_library_changes c ON c.owner_id=p.owner_id AND c.asset_id=p.asset_id AND c.state='purged' AND c.revision=p.revision+1 WHERE p.state='completed' GROUP BY p.owner_id,p.asset_id,p.revision,p.reason,p.completed_at,p.refunded_bytes ORDER BY p.owner_id,p.asset_id LIMIT 100001"
    else:
        raise OperationError("asset deletion ledger schema is missing after migration or invalid")
    return query


def parse_export(raw):
    result = []
    try:
        for line in raw.decode().splitlines():
            owner, asset, revision, reason, stamp, ids = line.split("\t")
            result.append([owner, asset, int(revision), reason, stamp, json.loads(ids)])
    except (ValueError, UnicodeError) as error:
        raise OperationError("invalid asset deletion export") from error
    return validate(result)


def export(runtime, sql, credentials):
    query = export_query(runtime, sql, credentials)
    return parse_export(runtime.execute("albums-asset-deletion-ledger-export", sql + ["--execute=" + query], env=credentials))


def restore_fences(runtime, sql, entries):
    validate(entries)
    target = runtime.execute("albums-restore-asset-scope", sql + ["--execute=SELECT IF(DATABASE()='albums_restore_test' AND (SELECT COUNT(*) FROM album_recovery_state WHERE singleton_id=1 AND quarantined=TRUE)=1,1,0)"]).decode().strip()
    if target != "1":
        raise OperationError("asset fences require the quarantined disposable restore database")
    # Authenticated old snapshots may predate the independent receipt table.
    migration = Path(__file__).resolve().parents[2] / "migrations/0078_album_asset_deletion_ledger.sql"
    runtime.execute("albums-restore-asset-schema", sql, input=migration.read_bytes())
    tables = set(runtime.execute("albums-restore-asset-tables", sql + ["--execute=SHOW TABLES"]).decode().splitlines())
    script = """
CREATE TEMPORARY TABLE recovery_assets LIKE album_asset_deletion_ledger;
CREATE TEMPORARY TABLE recovery_versions (
 owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,
 asset_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin,
 version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,
 revision BIGINT,purged_at DATETIME(6),PRIMARY KEY(owner_id,version_id));
CREATE TEMPORARY TABLE recovery_asset_guard (ok INT NOT NULL CHECK(ok=1));
"""
    for start in range(0, len(entries), 50):
        values = ["('%s','%s',%d,'%s','%s','%s')" % (o,a,v,r,t.replace("T"," ").removesuffix("Z"),json.dumps(ids,separators=(",",":"))) for o,a,v,r,t,ids in entries[start:start+50]]
        script += "INSERT INTO recovery_assets VALUES " + ",".join(values) + ";\n"
    values = []
    for o,a,v,r,t,ids in entries:
        for identity in ids:
            values.append("('%s','%s','%s',%d,'%s')" % (o,a,identity,v,t.replace("T"," ").removesuffix("Z")))
            if len(values) == 500:
                script += "INSERT INTO recovery_versions VALUES " + ",".join(values) + ";\n"; values=[]
    if values:
        script += "INSERT INTO recovery_versions VALUES " + ",".join(values) + ";\n"
    script += """
START TRANSACTION;
INSERT INTO recovery_asset_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_asset_deletion_ledger old LEFT JOIN recovery_assets n ON n.owner_id=old.owner_id AND n.asset_id=old.asset_id
 WHERE n.owner_id IS NULL OR old.final_revision<>n.final_revision OR old.reason<>n.reason OR old.purged_at<>n.purged_at
 OR JSON_LENGTH(old.version_ids)<>JSON_LENGTH(n.version_ids) OR NOT JSON_CONTAINS(old.version_ids,n.version_ids) OR NOT JSON_CONTAINS(n.version_ids,old.version_ids)),0,1);
INSERT INTO recovery_asset_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_asset_lifecycle a JOIN recovery_assets n ON n.owner_id=a.owner_id AND n.asset_id=a.asset_id WHERE a.revision>n.final_revision OR (a.revision=n.final_revision AND a.state<>'purged')),0,1);
INSERT INTO recovery_asset_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_asset_versions v JOIN recovery_assets n ON n.owner_id=v.owner_id AND n.asset_id=v.asset_id WHERE NOT JSON_CONTAINS(n.version_ids,JSON_QUOTE(v.version_id))),0,1);
INSERT INTO recovery_asset_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_asset_versions v JOIN recovery_versions n ON n.owner_id=v.owner_id AND n.version_id=v.version_id WHERE v.asset_id<>n.asset_id),0,1);
INSERT INTO recovery_asset_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_library_changes c JOIN recovery_assets n ON n.owner_id=c.owner_id AND n.asset_id=c.asset_id WHERE c.revision>n.final_revision OR NOT JSON_CONTAINS(n.version_ids,JSON_QUOTE(c.version_id))),0,1);
INSERT INTO recovery_asset_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_uploads u JOIN recovery_assets n ON n.owner_id=u.owner_id AND JSON_UNQUOTE(JSON_EXTRACT(u.manifest_json,'$.assetID'))=n.asset_id WHERE NOT JSON_CONTAINS(n.version_ids,JSON_QUOTE(u.upload_id))),0,1);
INSERT INTO recovery_asset_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_uploads u JOIN recovery_versions n ON n.owner_id=u.owner_id AND n.version_id=u.upload_id WHERE JSON_EXTRACT(u.manifest_json,'$.assetID') IS NULL OR JSON_UNQUOTE(JSON_EXTRACT(u.manifest_json,'$.assetID'))<>n.asset_id),0,1);
INSERT INTO album_asset_deletion_ledger SELECT n.* FROM recovery_assets n LEFT JOIN album_asset_deletion_ledger old ON old.owner_id=n.owner_id AND old.asset_id=n.asset_id WHERE old.owner_id IS NULL;
DELETE c FROM album_share_claims c JOIN album_share_links s ON s.share_id=c.share_id JOIN recovery_versions n ON n.owner_id=s.owner_id AND n.version_id=s.version_id;
DELETE j FROM album_deletion_video_jobs j JOIN album_video_jobs v ON v.job_id=j.job_id AND v.owner_id=j.account_id JOIN recovery_versions n ON n.owner_id=v.owner_id AND n.version_id=v.version_id;
DELETE m FROM album_live_pair_multiparts m JOIN album_live_pairs p ON p.pair_upload_id=m.pair_upload_id JOIN recovery_versions n ON n.owner_id=p.owner_id AND n.version_id=p.version_id;
"""
    for table in ("album_share_links","album_derived_cleanups","album_derived_finalizations","album_photo_writes","album_photo_jobs","album_video_jobs","album_live_pairs"):
        script += "DELETE c FROM " + table + " c JOIN recovery_versions n ON n.owner_id=c.owner_id AND n.version_id=c.version_id;\n"
    for table in ("album_multipart_completions","album_multipart_intents","album_original_seals"):
        script += "DELETE c FROM " + table + " c JOIN album_uploads u ON u.upload_id=c.upload_id JOIN recovery_versions n ON n.owner_id=u.owner_id AND n.version_id=u.upload_id;\n"
    script += "DELETE u FROM album_uploads u JOIN recovery_versions n ON n.owner_id=u.owner_id AND n.version_id=u.upload_id;\n"
    script += "DELETE v FROM album_asset_versions v JOIN recovery_versions n ON n.owner_id=v.owner_id AND n.version_id=v.version_id;\n"
    for table in ("album_asset_favorites","album_collection_members","album_asset_operations","album_object_purges","album_library_changes"):
        if table in tables:
            script += "DELETE c FROM " + table + " c JOIN recovery_assets n ON n.owner_id=c.owner_id AND n.asset_id=c.asset_id;\n"
    script += """
INSERT INTO album_asset_lifecycle(owner_id,asset_id,revision,lifecycle_epoch,state)
 SELECT n.owner_id,n.asset_id,n.final_revision,n.final_revision,'purged' FROM recovery_assets n JOIN album_storage_accounts s ON s.owner_id=n.owner_id
 ON DUPLICATE KEY UPDATE revision=VALUES(revision),lifecycle_epoch=GREATEST(lifecycle_epoch,VALUES(lifecycle_epoch)),state='purged',trashed_at=NULL,trash_expires_at=NULL;
INSERT INTO album_library_changes(owner_id,asset_id,version_id,revision,state,changed_at)
 SELECT n.owner_id,n.asset_id,n.version_id,n.revision,'purged',n.purged_at FROM recovery_versions n JOIN album_storage_accounts s ON s.owner_id=n.owner_id ORDER BY n.owner_id,n.asset_id,n.version_id;
INSERT INTO recovery_asset_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_asset_versions v JOIN recovery_assets n ON n.owner_id=v.owner_id AND n.asset_id=v.asset_id),0,1);
INSERT INTO recovery_asset_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_asset_lifecycle a JOIN recovery_assets n ON n.owner_id=a.owner_id AND n.asset_id=a.asset_id WHERE a.state<>'purged' OR a.revision<>n.final_revision),0,1);
COMMIT;
"""
    # A historical balance is not current billing authority. Keep quarantine and
    # require separate quota reconciliation; never refund from a restored row.
    runtime.execute("albums-restore-asset-fences", sql, input=script.encode())
