CREATE TABLE IF NOT EXISTS privacy_deletion_receipts (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_digest BINARY(32) NOT NULL,
    completed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (app_id, request_digest),
    CONSTRAINT privacy_deletion_receipt_app_fk FOREIGN KEY (app_id)
        REFERENCES apps(app_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
