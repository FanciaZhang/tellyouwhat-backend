-- Each row consumes ONE write attempt before a provider mutation. An unknown
-- outcome is reconciled read-only; it cannot allocate another attempt implicitly.
CREATE TABLE IF NOT EXISTS album_photo_writes (
    attempt_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    manifest_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    preset VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    size_bytes BIGINT NOT NULL,
    sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    pixel_width INT NOT NULL,
    pixel_height INT NOT NULL,
    object_version VARCHAR(1024) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    UNIQUE KEY album_photo_preset (owner_id, version_id, preset),
    CONSTRAINT album_photo_source_fk FOREIGN KEY (owner_id, version_id) REFERENCES album_asset_versions(owner_id, version_id),
    CHECK (size_bytes > 0 AND pixel_width > 0 AND pixel_height > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
