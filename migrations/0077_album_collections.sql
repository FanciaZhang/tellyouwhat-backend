-- Mutable grouping is separate from immutable media and never deletes originals.
CREATE TABLE IF NOT EXISTS album_collections (
    sequence_id BIGINT NOT NULL AUTO_INCREMENT,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    collection_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(80) NOT NULL,
    deleted BOOLEAN NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (sequence_id),
    UNIQUE KEY album_collection (owner_id, collection_id),
    KEY album_collection_cursor (owner_id, sequence_id),
    CONSTRAINT album_collection_owner_fk FOREIGN KEY (owner_id) REFERENCES album_accounts(account_id),
    CHECK (revision > 0),
    CHECK (deleted IN (0, 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS album_collection_members (
    sequence_id BIGINT NOT NULL AUTO_INCREMENT,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    collection_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    asset_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    present BOOLEAN NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (sequence_id),
    UNIQUE KEY album_collection_member (owner_id, collection_id, asset_id),
    KEY album_collection_member_cursor (owner_id, sequence_id),
    CONSTRAINT album_collection_member_asset_fk FOREIGN KEY (owner_id, asset_id)
        REFERENCES album_asset_lifecycle(owner_id, asset_id) ON DELETE CASCADE,
    CHECK (revision > 0),
    CHECK (present IN (0, 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
