-- Login proof is single-use across processes; raw proof secrets are never stored.
CREATE TABLE IF NOT EXISTS album_login_challenges (
    challenge_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    proof_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    apple_nonce CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    consumed_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY album_login_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
