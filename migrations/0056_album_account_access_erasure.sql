ALTER TABLE album_account_deletions
 ADD COLUMN access_erased_at DATETIME(6) NULL,
 ADD CONSTRAINT album_access_erased_after_request CHECK(access_erased_at IS NULL OR access_erased_at>=requested_at);
