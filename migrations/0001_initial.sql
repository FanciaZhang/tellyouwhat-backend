CREATE TABLE IF NOT EXISTS apps (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    display_name VARCHAR(128) NOT NULL,
    bundle_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
    apple_app_id BIGINT UNSIGNED,
    managed_product_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO apps (app_id, display_name, bundle_id, managed_product_id)
VALUES
    ('health', '告你健康', 'cn.tellyouwhat.healthapp', 'health.ai.subscription.monthly'),
    ('journal', '告你手记', 'cn.tellyouwhat.journalapp', 'journal.ai.subscription.monthly')
ON DUPLICATE KEY UPDATE
    display_name = VALUES(display_name),
    bundle_id = VALUES(bundle_id),
    managed_product_id = VALUES(managed_product_id),
    enabled = TRUE,
    updated_at = CURRENT_TIMESTAMP(6);

CREATE TABLE IF NOT EXISTS app_attest_keys (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    device_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    public_key_der LONGBLOB NOT NULL,
    assertion_counter BIGINT UNSIGNED NOT NULL DEFAULT 0,
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    receipt LONGBLOB NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (app_id, key_id),
    UNIQUE KEY app_attest_device_unique (app_id, device_id),
    CONSTRAINT app_attest_app_fk FOREIGN KEY (app_id) REFERENCES apps(app_id) ON DELETE CASCADE,
    CHECK (environment IN ('development', 'production'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS managed_entitlements (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (app_id, key_id),
    INDEX managed_entitlements_transaction_idx (app_id, original_transaction_id),
    CONSTRAINT managed_entitlements_key_fk FOREIGN KEY (app_id, key_id)
        REFERENCES app_attest_keys(app_id, key_id) ON DELETE CASCADE,
    CHECK (environment IN ('development', 'production', 'sandbox'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS app_store_notifications (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    notification_uuid VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    processed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (app_id, notification_uuid),
    INDEX app_store_notifications_processed_idx (app_id, processed_at),
    CONSTRAINT app_store_notifications_app_fk FOREIGN KEY (app_id) REFERENCES apps(app_id) ON DELETE CASCADE,
    CHECK (environment IN ('production', 'sandbox'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS ai_jobs (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    body_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner_key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner_device_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    request_ciphertext LONGBLOB NOT NULL,
    request_nonce VARBINARY(32) NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    result_ciphertext LONGBLOB,
    result_nonce VARBINARY(32),
    input_tokens INT UNSIGNED NOT NULL DEFAULT 0,
    output_tokens INT UNSIGNED NOT NULL DEFAULT 0,
    failure_category VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
    claim_expires_at DATETIME(6),
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    PRIMARY KEY (app_id, id),
    UNIQUE KEY ai_jobs_request_unique (app_id, request_id),
    INDEX ai_jobs_status_created_idx (status, created_at),
    INDEX ai_jobs_expiry_idx (expires_at),
    CONSTRAINT ai_jobs_owner_key_fk FOREIGN KEY (app_id, owner_key_id)
        REFERENCES app_attest_keys(app_id, key_id) ON DELETE CASCADE,
    CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS job_dispatch_outbox (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    job_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    available_at DATETIME(6) NOT NULL,
    attempts INT UNSIGNED NOT NULL DEFAULT 0,
    claimed_until DATETIME(6),
    last_error VARCHAR(255) NOT NULL DEFAULT '',
    PRIMARY KEY (app_id, job_id),
    INDEX job_dispatch_outbox_ready_idx (available_at, claimed_until),
    CONSTRAINT job_dispatch_outbox_job_fk FOREIGN KEY (app_id, job_id)
        REFERENCES ai_jobs(app_id, id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS idempotency_records (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner_key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    body_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    response_status INT,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (app_id, request_id),
    INDEX idempotency_records_expiry_idx (expires_at),
    CONSTRAINT idempotency_records_owner_key_fk FOREIGN KEY (app_id, owner_key_id)
        REFERENCES app_attest_keys(app_id, key_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS usage_ledger (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    device_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    input_tokens INT UNSIGNED NOT NULL,
    output_tokens INT UNSIGNED NOT NULL,
    occurred_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY usage_ledger_request_unique (app_id, request_id),
    INDEX usage_ledger_subject_time_idx (app_id, original_transaction_id, occurred_at),
    INDEX usage_ledger_occurred_idx (occurred_at),
    CONSTRAINT usage_ledger_key_fk FOREIGN KEY (app_id, key_id)
        REFERENCES app_attest_keys(app_id, key_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS media_objects (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    object_id VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner_key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner_device_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    media_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    size_bytes BIGINT UNSIGNED NOT NULL,
    mime_type VARCHAR(255) CHARACTER SET ascii COLLATE ascii_general_ci NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    consumed_at DATETIME(6),
    deleted_at DATETIME(6),
    PRIMARY KEY (app_id, object_id),
    INDEX media_objects_expiry_idx (expires_at),
    CONSTRAINT media_objects_owner_key_fk FOREIGN KEY (app_id, owner_key_id)
        REFERENCES app_attest_keys(app_id, key_id) ON DELETE CASCADE,
    CHECK (kind IN ('image', 'audio')),
    CHECK (size_bytes > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS privacy_consents (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    device_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    document_version VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    granted BOOLEAN NOT NULL,
    recorded_at DATETIME(6) NOT NULL,
    PRIMARY KEY (app_id, key_id, scope, document_version),
    INDEX privacy_consents_recorded_idx (app_id, recorded_at),
    CONSTRAINT privacy_consents_key_fk FOREIGN KEY (app_id, key_id)
        REFERENCES app_attest_keys(app_id, key_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS app_store_offer_redemptions (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    transaction_hash BINARY(32) NOT NULL,
    original_transaction_hash BINARY(32) NOT NULL,
    offer_identifier VARCHAR(255) NOT NULL,
    offer_type SMALLINT NOT NULL,
    redeemed_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (app_id, environment, transaction_hash),
    INDEX app_store_offer_redemptions_offer_idx (app_id, offer_identifier, environment, redeemed_at),
    INDEX app_store_offer_redemptions_original_idx (app_id, original_transaction_hash, offer_identifier),
    CONSTRAINT app_store_offer_redemptions_app_fk FOREIGN KEY (app_id) REFERENCES apps(app_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS admin_users (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    webauthn_id VARBINARY(64) NOT NULL UNIQUE,
    display_name VARCHAR(128) NOT NULL,
	role VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'owner',
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'active',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
	CHECK (role IN ('owner')),
	CHECK (status IN ('active', 'disabled'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS admin_app_roles (
    admin_user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    role VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (admin_user_id, app_id),
    CONSTRAINT admin_roles_user_fk FOREIGN KEY (admin_user_id) REFERENCES admin_users(id) ON DELETE CASCADE,
    CONSTRAINT admin_roles_app_fk FOREIGN KEY (app_id) REFERENCES apps(app_id) ON DELETE CASCADE,
    CHECK (role IN ('viewer', 'operator', 'owner'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS admin_webauthn_credentials (
    credential_id VARBINARY(1024) PRIMARY KEY,
    admin_user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    display_name VARCHAR(128) NOT NULL,
    credential_json LONGBLOB NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_used_at DATETIME(6),
    CONSTRAINT admin_webauthn_credentials_user_fk FOREIGN KEY (admin_user_id) REFERENCES admin_users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS admin_recovery_codes (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    admin_user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code_hash VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    consumed_at DATETIME(6),
    CONSTRAINT admin_recovery_codes_user_fk FOREIGN KEY (admin_user_id) REFERENCES admin_users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS admin_bootstrap_tokens (
    token_hash BINARY(32) PRIMARY KEY,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    consumed_at DATETIME(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS admin_audit_events (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    admin_user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    action VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    target_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    outcome VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    metadata_json JSON NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX admin_audit_events_created_idx (created_at),
    INDEX admin_audit_events_actor_idx (admin_user_id, created_at),
    CONSTRAINT admin_audit_events_user_fk FOREIGN KEY (admin_user_id) REFERENCES admin_users(id) ON DELETE SET NULL,
    CONSTRAINT admin_audit_events_app_fk FOREIGN KEY (app_id) REFERENCES apps(app_id) ON DELETE SET NULL,
    CHECK (outcome IN ('succeeded', 'denied', 'failed'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS admin_operations (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    admin_user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin,
    idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    action VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_hash BINARY(32) NOT NULL,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'processing',
    response_status SMALLINT UNSIGNED,
    response_json JSON,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    completed_at DATETIME(6),
	UNIQUE KEY admin_operations_idempotency_unique (admin_user_id, app_id, idempotency_key),
    INDEX admin_operations_created_idx (created_at),
    CONSTRAINT admin_operations_user_fk FOREIGN KEY (admin_user_id) REFERENCES admin_users(id) ON DELETE CASCADE,
    CONSTRAINT admin_operations_app_fk FOREIGN KEY (app_id) REFERENCES apps(app_id) ON DELETE SET NULL,
    CHECK (state IN ('processing', 'completed'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
