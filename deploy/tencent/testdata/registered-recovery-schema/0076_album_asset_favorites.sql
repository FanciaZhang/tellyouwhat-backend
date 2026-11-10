-- User choices follow the logical asset, independently of immutable backups.
CREATE TABLE IF NOT EXISTS album_asset_favorites (
    sequence_id BIGINT NOT NULL AUTO_INCREMENT,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    asset_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    favorite BOOLEAN NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (sequence_id),
    UNIQUE KEY album_asset_favorite (owner_id, asset_id),
    KEY album_asset_favorite_cursor (owner_id, sequence_id),
    CONSTRAINT album_asset_favorite_asset_fk FOREIGN KEY (owner_id, asset_id)
        REFERENCES album_asset_lifecycle(owner_id, asset_id) ON DELETE CASCADE,
    CHECK (revision > 0),
    CHECK (favorite IN (0, 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
