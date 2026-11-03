-- Durable account-owned media. These tables do not inherit ai-temp expiration.
CREATE TABLE IF NOT EXISTS album_storage_accounts (
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    quota_bytes BIGINT NOT NULL DEFAULT 0,
    used_bytes BIGINT NOT NULL DEFAULT 0,
    reserved_bytes BIGINT NOT NULL DEFAULT 0,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    CHECK (quota_bytes >= 0 AND used_bytes >= 0 AND reserved_bytes >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS album_uploads (
    upload_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    manifest_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    manifest_json JSON NOT NULL,
    state VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_bytes BIGINT NOT NULL,
    derived_budget BIGINT NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    lease_until DATETIME(6) NULL,
    sealed_objects JSON NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY album_upload_request (owner_id, request_id),
    UNIQUE KEY album_upload_manifest (owner_id, manifest_digest),
    KEY album_upload_worker (state, lease_until, expires_at),
    CONSTRAINT album_upload_owner_fk FOREIGN KEY (owner_id) REFERENCES album_storage_accounts(owner_id),
    CHECK (original_bytes > 0 AND derived_budget > 0),
    CHECK (state IN ('uploading', 'queued', 'verifying', 'originals_verified'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
