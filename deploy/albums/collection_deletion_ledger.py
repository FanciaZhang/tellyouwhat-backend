"""Replay durable collection tombstones without restoring names or deleting media."""
import datetime
from pathlib import Path
import re
import time
import uuid

from ops_common import OperationError


def validate(entries):
    if not isinstance(entries, list) or len(entries) > 100000:
        raise OperationError('invalid collection deletion checkpoint size')
    seen = set()
    for row in entries:
        if not isinstance(row, list) or len(row) != 5:
            raise OperationError('invalid collection deletion row')
        owner, identity, revision, request, stamp = row
        try:
            valid = all(isinstance(v, str) and str(uuid.UUID(v)) == v and uuid.UUID(v).int
                        for v in (owner, identity, request))
            date = datetime.datetime.strptime(stamp, '%Y-%m-%dT%H:%M:%S.%fZ').replace(tzinfo=datetime.timezone.utc)
        except (ValueError, TypeError, AttributeError) as error:
            raise OperationError('invalid collection deletion identity or date') from error
        if (not valid or type(revision) is not int or not 1 <= revision <= 9223372036854775807
                or not re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z', stamp)
                or date.year < 2000 or date.timestamp() > time.time() + 300 or (owner, identity) in seen):
            raise OperationError('invalid or conflicting collection deletion row')
        seen.add((owner, identity))
    return entries


def export_query(runtime, sql, credentials):
    schema = runtime.execute('albums-collection-deletion-schema', sql + ["--execute=SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='album_collections'),EXISTS(SELECT 1 FROM schema_migrations WHERE name='0077_album_collections.sql')"], env=credentials).strip()
    if schema == b'0\t0':
        return None
    if schema not in (b'1\t0', b'1\t1'):
        raise OperationError('collection table missing after migration or invalid')
    return "SELECT owner_id,collection_id,revision,request_id,DATE_FORMAT(updated_at,'%Y-%m-%dT%H:%i:%s.%fZ') FROM album_collections WHERE deleted=TRUE UNION ALL SELECT 'invalid','',0,'','' WHERE EXISTS(SELECT 1 FROM album_collections WHERE deleted=TRUE AND name<>'') ORDER BY owner_id,collection_id LIMIT 100001"


def parse_export(raw):
    rows = []
    try:
        for line in raw.decode().splitlines():
            owner, identity, revision, request, stamp = line.split('\t')
            rows.append([owner, identity, int(revision), request, stamp])
    except (ValueError, UnicodeError) as error:
        raise OperationError('invalid collection deletion export') from error
    return validate(rows)


def export(runtime, sql, credentials):
    query = export_query(runtime, sql, credentials)
    if query is None:
        return []
    return parse_export(runtime.execute('albums-collection-deletion-export', sql + ['--execute=' + query], env=credentials))


def restore_fences(runtime, sql, entries):
    validate(entries)
    target = runtime.execute('albums-restore-collection-scope', sql + ["--execute=SELECT IF(DATABASE()='albums_restore_test' AND (SELECT COUNT(*) FROM album_recovery_state WHERE singleton_id=1 AND quarantined=TRUE)=1,1,0)"]).decode().strip()
    if target != '1':
        raise OperationError('collection fences require the quarantined disposable restore database')
    migration = Path(__file__).resolve().parents[2] / 'migrations/0077_album_collections.sql'
    runtime.execute('albums-restore-collection-schema', sql, input=migration.read_bytes())
    script = '''
CREATE TEMPORARY TABLE recovery_collections LIKE album_collections;
CREATE TEMPORARY TABLE recovery_collection_guard (ok INT NOT NULL CHECK(ok=1));
'''
    for start in range(0, len(entries), 100):
        values = ["('%s','%s',%d,'%s','','1','%s')" % (o,c,v,r,t.replace('T',' ').removesuffix('Z'))
                  for o,c,v,r,t in entries[start:start+100]]
        script += 'INSERT INTO recovery_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES ' + ','.join(values) + ';\n'
    script += '''
START TRANSACTION;
-- Whole-account deletion receipts cover collections already removed from the live source.
INSERT INTO recovery_collection_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_collections c LEFT JOIN recovery_collections n ON n.owner_id=c.owner_id AND n.collection_id=c.collection_id
 LEFT JOIN album_deletion_ledger a ON a.account_id=c.owner_id WHERE c.deleted=TRUE AND n.owner_id IS NULL AND a.account_id IS NULL),0,1);
INSERT INTO recovery_collection_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_collections c JOIN recovery_collections n ON n.owner_id=c.owner_id AND n.collection_id=c.collection_id
 WHERE c.revision>n.revision OR c.updated_at>n.updated_at OR (c.revision=n.revision AND NOT c.deleted)
 OR (c.deleted AND (c.revision<>n.revision OR c.request_id<>n.request_id OR c.updated_at<>n.updated_at OR c.name<>''))),0,1);
INSERT INTO recovery_collection_guard SELECT IF(EXISTS (
 SELECT 1 FROM album_collection_members c JOIN recovery_collections n ON n.owner_id=c.owner_id AND n.collection_id=c.collection_id WHERE c.updated_at>n.updated_at),0,1);
DELETE c FROM album_collection_members c JOIN recovery_collections n ON n.owner_id=c.owner_id AND n.collection_id=c.collection_id;
DELETE c FROM album_collections c JOIN recovery_collections n ON n.owner_id=c.owner_id AND n.collection_id=c.collection_id WHERE c.revision<n.revision;
INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at)
 SELECT n.owner_id,n.collection_id,n.revision,n.request_id,'',TRUE,n.updated_at FROM recovery_collections n
 JOIN album_accounts a ON a.account_id=n.owner_id
 LEFT JOIN album_collections c ON c.owner_id=n.owner_id AND c.collection_id=n.collection_id WHERE c.owner_id IS NULL ORDER BY n.owner_id,n.collection_id;
INSERT INTO recovery_collection_guard SELECT IF(EXISTS (
 SELECT 1 FROM recovery_collections n JOIN album_accounts a ON a.account_id=n.owner_id
 LEFT JOIN album_collections c ON c.owner_id=n.owner_id AND c.collection_id=n.collection_id
 WHERE c.owner_id IS NULL OR NOT c.deleted OR c.name<>'' OR c.revision<>n.revision OR c.request_id<>n.request_id OR c.updated_at<>n.updated_at),0,1);
COMMIT;
'''
    runtime.execute('albums-restore-collection-fences', sql, input=script.encode())
