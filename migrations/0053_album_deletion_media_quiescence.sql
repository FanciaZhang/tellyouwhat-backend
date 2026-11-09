-- A closed write barrier is not evidence that objects or account data are erased.
ALTER TABLE album_account_deletions
    ADD COLUMN media_quiesced_at DATETIME(6) NULL,
    ADD COLUMN media_check_after DATETIME(6) NULL,
    ADD COLUMN media_check_attempts INT UNSIGNED NOT NULL DEFAULT 0,
    ADD KEY album_deletion_media_due(media_quiesced_at,media_check_after),
    ADD CHECK(media_quiesced_at IS NULL OR media_quiesced_at>=requested_at);
