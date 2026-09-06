CREATE TABLE IF NOT EXISTS ai_cost_control_state (
    singleton_id TINYINT UNSIGNED PRIMARY KEY,
    CHECK (singleton_id = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO ai_cost_control_state (singleton_id) VALUES (1)
ON DUPLICATE KEY UPDATE singleton_id = VALUES(singleton_id);

CREATE TABLE IF NOT EXISTS ai_cost_months (
    month_start DATE PRIMARY KEY,
    budget_nanos BIGINT UNSIGNED NOT NULL,
    charged_nanos BIGINT UNSIGNED NOT NULL DEFAULT 0,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS ai_cost_attempts (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    month_start DATE NOT NULL,
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    operation VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    meter VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    reserved_nanos BIGINT UNSIGNED NOT NULL,
    actual_nanos BIGINT UNSIGNED,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL,
    lease_expires_at DATETIME(6) NOT NULL,
    completed_at DATETIME(6),
    INDEX ai_cost_attempts_pending_idx (status, lease_expires_at),
    INDEX ai_cost_attempts_month_idx (month_start, created_at),
    CONSTRAINT ai_cost_attempts_month_fk FOREIGN KEY (month_start)
        REFERENCES ai_cost_months(month_start) ON DELETE RESTRICT,
    CHECK (status IN ('pending', 'settled', 'unknown'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
