CREATE TABLE IF NOT EXISTS album_accounts (
    account_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    apple_subject_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'active',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY album_apple_identity (apple_subject_sha256),
    CHECK (state IN ('active','disabled','deleting'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS album_apple_grants (
    grant_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    account_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_sha256 BINARY(32) NOT NULL,
    encrypted_token BLOB NOT NULL,
    token_nonce BINARY(12) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    revoked_at DATETIME(6) NULL,
    UNIQUE KEY album_account_refresh_token (account_id,token_sha256),
    CONSTRAINT album_grant_account_fk FOREIGN KEY (account_id) REFERENCES album_accounts(account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
