-- Media writers and deletion serialize on the storage row. Do not clear this
-- marker on renewed subscriptions or delayed provider receipts.
ALTER TABLE album_storage_accounts ADD COLUMN deletion_requested_at DATETIME(6) NULL;

UPDATE album_storage_accounts s JOIN album_account_deletions d ON d.account_id=s.owner_id
SET s.deletion_requested_at=d.requested_at;
