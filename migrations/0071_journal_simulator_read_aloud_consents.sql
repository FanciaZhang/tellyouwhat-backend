-- Simulator integration identities must never create fictitious App Attest keys.
-- Production never enables the repository that reads or writes this table.
CREATE TABLE IF NOT EXISTS journal_simulator_read_aloud_consents (
    key_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    document_version VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    granted BOOLEAN NOT NULL,
    recorded_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    PRIMARY KEY (key_id, scope, document_version),
    INDEX simulator_consent_expiry (expires_at),
    CHECK (scope = 'journal_read_aloud'),
    CHECK (LEFT(key_id, 10) = 'simulator_'),
    CHECK (expires_at > recorded_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
