CREATE TABLE managed_entitlement_environments (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    started_at DATETIME(6) NULL,
    price_milli BIGINT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (app_id, key_id, environment),
    INDEX entitlement_environment_transaction_idx (app_id, original_transaction_id, environment),
    CONSTRAINT entitlement_environment_key_fk FOREIGN KEY (app_id, key_id)
        REFERENCES app_attest_keys(app_id, key_id) ON DELETE CASCADE,
    CHECK (environment IN ('production', 'sandbox'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO managed_entitlement_environments
    (app_id, key_id, original_transaction_id, environment, expires_at, started_at, price_milli, updated_at)
SELECT app_id, key_id, original_transaction_id, environment, expires_at, started_at, price_milli, updated_at
FROM managed_entitlements WHERE environment IN ('production', 'sandbox');
