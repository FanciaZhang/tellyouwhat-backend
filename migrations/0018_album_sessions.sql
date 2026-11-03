CREATE TABLE IF NOT EXISTS album_sessions (
    session_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    account_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    grant_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_sha256 BINARY(32) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    revoked_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY album_session_token (token_sha256),
    KEY album_session_expiry (expires_at),
    CONSTRAINT album_session_account_fk FOREIGN KEY (account_id) REFERENCES album_accounts(account_id),
    CONSTRAINT album_session_grant_fk FOREIGN KEY (grant_id) REFERENCES album_apple_grants(grant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
