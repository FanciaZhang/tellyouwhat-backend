ALTER TABLE album_multipart_intents
    ADD COLUMN deletion_check_after DATETIME(6) NULL,
    ADD COLUMN deletion_check_attempts INT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN deletion_check_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN deletion_check_until DATETIME(6) NULL,
    ADD COLUMN deletion_abort_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN deletion_aborted_at DATETIME(6) NULL,
    ADD KEY album_multipart_deletion_due (deletion_aborted_at,deletion_check_after,deletion_check_until),
    ADD CONSTRAINT album_multipart_abort_ready CHECK (deletion_abort_id IS NULL OR state='ready'),
    ADD CONSTRAINT album_multipart_abort_receipt CHECK (deletion_aborted_at IS NULL OR deletion_abort_id IS NOT NULL);
