-- Acceptance receipt only. Completion requires separately verified erasure.
CREATE TABLE album_account_deletions (
    account_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    authorizing_session_sha256 BINARY(32) NOT NULL,
    requested_at DATETIME(6) NOT NULL,
    UNIQUE KEY album_deletion_request(request_id),
    CONSTRAINT album_deletion_account_fk FOREIGN KEY(account_id) REFERENCES album_accounts(account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
