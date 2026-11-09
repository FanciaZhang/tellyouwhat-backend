-- Share secrets and plaintext passwords never enter these tables.
CREATE TABLE album_share_links (
    share_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    key_id CHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_digest BINARY(32) NOT NULL,
    token_digest BINARY(32) NOT NULL,
    password_proof VARBINARY(64) NULL,
    max_claims INT NOT NULL,
    claimed_count INT NOT NULL DEFAULT 0,
    created_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    revoked_at DATETIME(6) NULL,
    UNIQUE KEY album_share_request(owner_id,request_id),
    KEY album_share_owner_version(owner_id,version_id,expires_at),
    CONSTRAINT album_share_owner_fk FOREIGN KEY(owner_id) REFERENCES album_storage_accounts(owner_id),
    CONSTRAINT album_share_version_fk FOREIGN KEY(owner_id,version_id) REFERENCES album_asset_versions(owner_id,version_id),
    CHECK(max_claims BETWEEN 1 AND 100),
    CHECK(claimed_count BETWEEN 0 AND max_claims),
    CHECK(password_proof IS NULL OR OCTET_LENGTH(password_proof)=49),
    CHECK(expires_at>created_at AND expires_at<=DATE_ADD(created_at,INTERVAL 30 DAY))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE album_share_claims (
    claim_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    share_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    key_id CHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_digest BINARY(32) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    UNIQUE KEY album_share_claim_request(share_id,request_id),
    CONSTRAINT album_share_claim_link_fk FOREIGN KEY(share_id) REFERENCES album_share_links(share_id),
    CHECK(expires_at>created_at AND expires_at<=DATE_ADD(created_at,INTERVAL 1 HOUR))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
