-- Reuse the single-consumption COPY intent and its destination-version receipt.
-- Existing positive receipts remain authoritative; legacy intents never prove quiescence.
ALTER TABLE album_original_seals
    ADD COLUMN deletion_check_after DATETIME(6) NULL,
    ADD COLUMN deletion_check_attempts INT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN deletion_check_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN deletion_check_until DATETIME(6) NULL;
