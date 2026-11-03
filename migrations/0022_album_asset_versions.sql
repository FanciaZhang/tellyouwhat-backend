-- Immutable recoverable versions, independent of upload-task expiry. A logical
-- asset can have multiple versions; readers must not confuse this with a list
-- of current display items. Publication holds the existing owner/account lock,
-- so a cursor cannot advance past an earlier uncommitted event for that owner.
CREATE TABLE IF NOT EXISTS album_asset_versions (
    sequence_id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    asset_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    manifest_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    manifest_json JSON NOT NULL,
    sealed_objects JSON NOT NULL,
    original_bytes BIGINT NOT NULL,
    published_at DATETIME(6) NOT NULL,
    UNIQUE KEY album_version_identity (owner_id, version_id),
    KEY album_version_changes (owner_id, sequence_id),
    KEY album_version_asset (owner_id, asset_id, sequence_id),
    CONSTRAINT album_version_owner_fk FOREIGN KEY (owner_id) REFERENCES album_storage_accounts(owner_id),
    CHECK (original_bytes > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
