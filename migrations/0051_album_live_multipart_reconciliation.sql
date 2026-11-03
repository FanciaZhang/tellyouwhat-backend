ALTER TABLE album_live_pair_multiparts
    ADD COLUMN check_after DATETIME(6) NULL,
    ADD COLUMN check_attempts INT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN check_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN check_until DATETIME(6) NULL,
    ADD KEY album_live_multipart_due (state, check_after, check_until);
