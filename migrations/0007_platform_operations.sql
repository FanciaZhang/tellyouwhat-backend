CREATE TABLE platform_ops_revisions (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    document JSON NOT NULL,
    created_at DATETIME(6) NOT NULL,
    published_at DATETIME(6),
    INDEX platform_ops_history_idx (created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE platform_ops_current (
    singleton_id TINYINT UNSIGNED PRIMARY KEY,
    revision_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    CHECK (singleton_id = 1),
    FOREIGN KEY (revision_id) REFERENCES platform_ops_revisions(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE ai_cost_attempts
    ADD COLUMN outcome VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin,
    ADD COLUMN latency_ms BIGINT UNSIGNED,
    ADD COLUMN input_tokens BIGINT UNSIGNED,
    ADD COLUMN output_tokens BIGINT UNSIGNED,
    ADD COLUMN model_name VARCHAR(128) NOT NULL DEFAULT '',
    ADD INDEX ai_cost_app_month_idx (app_id, month_start, status);

CREATE TABLE platform_ops_mutations (
    actor_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    action VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_hash BINARY(32) NOT NULL,
    response_json JSON NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (actor_id, idempotency_key),
    FOREIGN KEY (actor_id) REFERENCES admin_users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE platform_ops_rejections (
    hour_start DATETIME NOT NULL,
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    reason VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    total BIGINT UNSIGNED NOT NULL DEFAULT 0,
    PRIMARY KEY (hour_start, app_id, operation, reason)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE ai_jobs
    ADD COLUMN operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '';
