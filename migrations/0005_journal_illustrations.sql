CREATE TABLE IF NOT EXISTS journal_illustration_tasks (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    metadata JSON NOT NULL,
    request_ciphertext LONGBLOB NULL,
    request_nonce VARBINARY(12) NULL,
    expires_at DATETIME(6) NOT NULL,
    PRIMARY KEY (app_id, id),
    KEY illustration_expiry (expires_at)
) ENGINE=InnoDB;
