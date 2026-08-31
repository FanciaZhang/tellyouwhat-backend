CREATE TABLE IF NOT EXISTS admin_operations (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    admin_user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    action VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_hash BINARY(32) NOT NULL,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'processing',
    response_status SMALLINT UNSIGNED,
    response_json JSON,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    completed_at DATETIME(6),
    UNIQUE KEY admin_operations_idempotency_unique (admin_user_id, idempotency_key),
    INDEX admin_operations_created_idx (created_at),
    CONSTRAINT admin_operations_user_fk FOREIGN KEY (admin_user_id)
        REFERENCES admin_users(id) ON DELETE CASCADE,
    CHECK (state IN ('processing', 'completed'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
