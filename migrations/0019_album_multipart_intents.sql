-- One provider initialization intent per manifest resource. An uncertain outcome
-- is never reclaimed by timeout: reconciliation must resolve it first.
CREATE TABLE IF NOT EXISTS album_multipart_intents (
    upload_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    intent_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider_upload_id VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (upload_id, resource_id),
    UNIQUE KEY album_multipart_intent_id (intent_id),
    CONSTRAINT album_multipart_upload_fk FOREIGN KEY (upload_id) REFERENCES album_uploads(upload_id),
    CHECK (state IN ('initiating', 'unknown', 'ready')),
    CHECK ((state = 'ready' AND provider_upload_id IS NOT NULL) OR
           (state <> 'ready' AND provider_upload_id IS NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
