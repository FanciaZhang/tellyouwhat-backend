CREATE TABLE album_apple_identity_state (
    subject_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    revoked_before DATETIME(6) NULL,
    deleted_at DATETIME(6) NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE album_apple_grants ADD COLUMN identity_issued_at DATETIME(6) NULL;
