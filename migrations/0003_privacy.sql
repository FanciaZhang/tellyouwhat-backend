CREATE TABLE IF NOT EXISTS privacy_consents (
    key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    device_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    document_version VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    granted BOOLEAN NOT NULL,
    recorded_at DATETIME(6) NOT NULL,
    PRIMARY KEY (key_id, scope, document_version),
    INDEX privacy_consents_recorded_idx (recorded_at),
    CONSTRAINT privacy_consents_key_fk FOREIGN KEY (key_id)
        REFERENCES app_attest_keys(key_id) ON DELETE CASCADE,
    CHECK (scope IN (
        'adult',
        'privacy_and_terms',
        'lifetime_byok',
        'managed_subscription',
        'sensitive_health_ai'
    ))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
