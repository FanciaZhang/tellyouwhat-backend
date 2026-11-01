-- Discovery hints only. Do not mutate the immutable manifest or original proof.
CREATE TABLE IF NOT EXISTS album_system_copy_locators (
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    locator_id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    copy_kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    cloud_identifier VARBINARY(4096) NOT NULL,
    PRIMARY KEY (owner_id, version_id, locator_id),
    CONSTRAINT album_system_copy_version_fk FOREIGN KEY (owner_id, version_id)
        REFERENCES album_asset_versions(owner_id, version_id) ON DELETE CASCADE,
    CHECK (copy_kind IN ('restored_original','daily_jpeg_v1','daily_png_v1','daily_mp4_1080_v1','daily_live_pair_v1')),
    CHECK (OCTET_LENGTH(cloud_identifier) > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
