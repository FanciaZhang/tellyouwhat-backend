-- Only character counts and request fingerprints are durable. Private text and
-- synthesized audio never enter these tables. IF NOT EXISTS permits DDL replay.
CREATE TABLE IF NOT EXISTS journal_read_aloud_accounts (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    subject_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    purpose VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    period_start DATETIME(6) NOT NULL,
    period_end DATETIME(6) NOT NULL,
    character_limit BIGINT UNSIGNED NOT NULL,
    used_characters BIGINT UNSIGNED NOT NULL DEFAULT 0,
    reserved_characters BIGINT UNSIGNED NOT NULL DEFAULT 0,
    PRIMARY KEY (app_id, subject_id, purpose, period_start),
    CHECK (purpose IN ('journal','preview')),
    CHECK (period_end > period_start)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS journal_read_aloud_requests (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    subject_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    period_start DATETIME(6) NOT NULL,
    input_hash BINARY(32) NOT NULL,
    purpose VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    reserved_characters BIGINT UNSIGNED NOT NULL,
    actual_characters BIGINT UNSIGNED NOT NULL DEFAULT 0,
    usage_known BOOLEAN NOT NULL DEFAULT FALSE,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    FOREIGN KEY (app_id,subject_id,purpose,period_start)
        REFERENCES journal_read_aloud_accounts(app_id,subject_id,purpose,period_start),
    INDEX read_aloud_subject_requests (app_id,subject_id,created_at),
    CHECK (purpose IN ('journal','preview')),
    CHECK (state IN ('reserved','running','succeeded','failed','cancelled','unknown')),
    CHECK (reserved_characters > 0 AND expires_at > created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
