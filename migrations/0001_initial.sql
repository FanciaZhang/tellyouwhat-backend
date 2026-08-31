CREATE TABLE IF NOT EXISTS app_attest_keys (
    key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    device_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
    transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    public_key_der LONGBLOB NOT NULL,
    assertion_counter BIGINT UNSIGNED NOT NULL DEFAULT 0,
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    receipt LONGBLOB NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    CHECK (environment IN ('development', 'production'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS managed_entitlements (
    key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    original_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX managed_entitlements_transaction_idx (original_transaction_id),
    CONSTRAINT managed_entitlements_key_fk FOREIGN KEY (key_id)
        REFERENCES app_attest_keys(key_id) ON DELETE CASCADE,
    CHECK (environment IN ('development', 'production', 'sandbox'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS prompt_contract_versions (
    operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    contract_version VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    prompt_version VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    release_state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (operation, contract_version, prompt_version),
    CHECK (release_state IN ('current', 'previous'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS app_store_notifications (
    notification_uuid VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    original_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    processed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX app_store_notifications_processed_idx (processed_at),
    CHECK (environment IN ('production', 'sandbox'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS ai_jobs (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
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
    INDEX ai_jobs_status_created_idx (status, created_at),
    INDEX ai_jobs_expiry_idx (expires_at),
    CONSTRAINT ai_jobs_owner_key_fk FOREIGN KEY (owner_key_id)
        REFERENCES app_attest_keys(key_id) ON DELETE CASCADE,
    CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS job_dispatch_outbox (
    job_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    available_at DATETIME(6) NOT NULL,
    attempts INT UNSIGNED NOT NULL DEFAULT 0,
    claimed_until DATETIME(6),
    last_error VARCHAR(255) NOT NULL DEFAULT '',
    INDEX job_dispatch_outbox_ready_idx (available_at, claimed_until),
    CONSTRAINT job_dispatch_outbox_job_fk FOREIGN KEY (job_id)
        REFERENCES ai_jobs(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS idempotency_records (
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    owner_key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    body_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    response_status INT,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX idempotency_records_expiry_idx (expires_at),
    CONSTRAINT idempotency_records_owner_key_fk FOREIGN KEY (owner_key_id)
        REFERENCES app_attest_keys(key_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS usage_ledger (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
    key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    device_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    input_tokens INT UNSIGNED NOT NULL,
    output_tokens INT UNSIGNED NOT NULL,
    occurred_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX usage_ledger_subject_time_idx (original_transaction_id, occurred_at),
    INDEX usage_ledger_occurred_idx (occurred_at),
    CONSTRAINT usage_ledger_key_fk FOREIGN KEY (key_id)
        REFERENCES app_attest_keys(key_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS media_objects (
    object_id VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
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
    INDEX media_objects_expiry_idx (expires_at),
    CONSTRAINT media_objects_owner_key_fk FOREIGN KEY (owner_key_id)
        REFERENCES app_attest_keys(key_id) ON DELETE CASCADE,
    CHECK (kind IN ('image', 'audio')),
    CHECK (size_bytes > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO prompt_contract_versions (operation, contract_version, prompt_version, release_state) VALUES
    ('voice_transcription', 'ai-request-v1', 'voice-transcription-v1', 'current'),
    ('voice_transcription', 'ai-request-v1', 'voice-transcription-v0', 'previous'),
    ('meal_photo_capture', 'ai-request-v1', 'meal-photo-v4', 'current'),
    ('meal_photo_capture', 'ai-request-v1', 'meal-photo-v3', 'previous'),
    ('meal_text_capture', 'ai-request-v1', 'meal-text-v4', 'current'),
    ('meal_text_capture', 'ai-request-v1', 'meal-text-v3', 'previous'),
    ('meal_decision', 'ai-request-v1', 'meal-decision-v10-fresh-exploration', 'current'),
    ('meal_decision', 'ai-request-v1', 'meal-decision-v9', 'previous'),
    ('diet_analysis', 'ai-request-v1', 'diet-day-review-v4', 'current'),
    ('diet_analysis', 'ai-request-v1', 'diet-day-review-v3', 'previous'),
    ('health_nutrition_analysis', 'ai-request-v1', 'health-nutrition-v1', 'current'),
    ('health_nutrition_analysis', 'ai-request-v1', 'health-nutrition-v0', 'previous'),
    ('health_behavior_analysis', 'ai-request-v1', 'health-behavior-v1', 'current'),
    ('health_behavior_analysis', 'ai-request-v1', 'health-behavior-v0', 'previous')
ON DUPLICATE KEY UPDATE operation = VALUES(operation);
