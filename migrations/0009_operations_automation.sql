ALTER TABLE ai_cost_attempts
    ADD COLUMN outcome_recorded_at DATETIME(6),
    ADD COLUMN cancelled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD INDEX ai_cost_outcome_window_idx (outcome_recorded_at, app_id, operation);

CREATE TABLE platform_ops_circuits (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    document JSON NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (app_id, operation)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE platform_ops_patrol (
    singleton_id TINYINT UNSIGNED PRIMARY KEY,
    completed_at DATETIME(6),
    policy_revision CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,
    host_document JSON,
    host_checked_at DATETIME(6),
    CHECK (singleton_id = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
INSERT INTO platform_ops_patrol(singleton_id) VALUES (1);

CREATE TABLE platform_ops_incidents (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    active_key VARCHAR(240) CHARACTER SET ascii COLLATE ascii_bin UNIQUE,
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    rule_name VARCHAR(40) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    opened_at DATETIME(6) NOT NULL,
    last_seen_at DATETIME(6) NOT NULL,
    last_observed_at DATETIME(6) NOT NULL,
    resolved_at DATETIME(6),
    healthy_checks INT UNSIGNED NOT NULL DEFAULT 0,
    evidence JSON NOT NULL,
    INDEX platform_ops_incident_history_idx (opened_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE platform_ops_events (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    action VARCHAR(40) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    rule_name VARCHAR(40) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    evidence JSON NOT NULL,
    created_at DATETIME(6) NOT NULL,
    INDEX platform_ops_event_time_idx (created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
