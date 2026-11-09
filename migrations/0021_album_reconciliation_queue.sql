ALTER TABLE album_multipart_completions
    ADD COLUMN reconcile_attempts SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN reconcile_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN reconcile_until DATETIME(6) NULL,
    ADD COLUMN reconcile_after DATETIME(6) NULL,
    ADD KEY album_reconcile_queue (state, reconcile_attempts, reconcile_after, reconcile_until);
