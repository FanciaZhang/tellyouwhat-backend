-- Observation is not a byte-verified preview and cannot settle its quota.
ALTER TABLE album_photo_writes
    ADD COLUMN deletion_object_version VARCHAR(1024) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    ADD COLUMN deletion_check_after DATETIME(6) NULL,
    ADD COLUMN deletion_check_attempts INT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN deletion_check_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN deletion_check_until DATETIME(6) NULL,
    ADD CONSTRAINT album_photo_deletion_version_ck CHECK(deletion_object_version IS NULL OR (deletion_object_version<>'' AND deletion_object_version<>'null'));
