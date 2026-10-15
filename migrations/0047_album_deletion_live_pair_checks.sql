-- One observation lease per pair; each consumed component keeps its own COPY receipt.
ALTER TABLE album_live_pairs
    ADD COLUMN deletion_check_after DATETIME(6) NULL,
    ADD COLUMN deletion_check_attempts INT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN deletion_check_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN deletion_check_until DATETIME(6) NULL;
