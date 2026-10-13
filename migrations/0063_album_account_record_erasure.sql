-- Online records only; independent deletion evidence survives for backup recovery.
ALTER TABLE album_deletion_ledger
 ADD COLUMN records_erased_at DATETIME(6) NULL,
 ADD CONSTRAINT album_records_erased_after_request CHECK(records_erased_at IS NULL OR records_erased_at>=requested_at);
