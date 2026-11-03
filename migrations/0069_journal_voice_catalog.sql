CREATE TABLE IF NOT EXISTS journal_voice_catalog_revisions (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    document JSON NOT NULL,
    base_version VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_by CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    voice_count SMALLINT UNSIGNED NOT NULL,
    enabled_count SMALLINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL,
    published_at DATETIME(6),
    INDEX journal_voice_catalog_history (created_at,id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS journal_voice_catalog_current (
    singleton_id TINYINT UNSIGNED PRIMARY KEY,
    revision_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,
    FOREIGN KEY (revision_id) REFERENCES journal_voice_catalog_revisions(id),
    CHECK (singleton_id=1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO journal_voice_catalog_current(singleton_id,revision_id) VALUES(1,NULL)
    ON DUPLICATE KEY UPDATE singleton_id=1;

CREATE TABLE IF NOT EXISTS journal_voice_verifications (
    policy_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    document JSON NOT NULL,
    verified_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS journal_voice_catalog_mutations (
    actor_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    action VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_hash BINARY(32) NOT NULL,
    response_json JSON,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (actor_id,idempotency_key),
    FOREIGN KEY (actor_id) REFERENCES admin_users(id) ON DELETE CASCADE,
    CHECK (state IN ('running','succeeded','failed','unknown'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
