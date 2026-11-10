-- Only explicit user category corrections; no OCR, inferred labels or media.
CREATE TABLE IF NOT EXISTS album_content_corrections (
    sequence_id BIGINT NOT NULL AUTO_INCREMENT,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    correction_json JSON NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (sequence_id),
    UNIQUE KEY album_content_correction_version (owner_id, version_id),
    KEY album_content_correction_cursor (owner_id, sequence_id),
    CONSTRAINT album_content_correction_version_fk FOREIGN KEY (owner_id, version_id)
        REFERENCES album_asset_versions(owner_id, version_id) ON DELETE CASCADE,
    CHECK (revision > 0),
    CHECK (JSON_TYPE(correction_json) = 'OBJECT' AND JSON_STORAGE_SIZE(correction_json) <= 1024)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
