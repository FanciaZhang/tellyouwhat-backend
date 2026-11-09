CREATE TABLE health_ai_config_revisions (
 id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 operation VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 document JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 published_at DATETIME(6) NULL,
 INDEX health_ai_config_history (operation,created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE health_ai_config_current (
 operation VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 revision_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
