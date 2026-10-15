-- Assembly is distinct from verified backup. Unknown external outcomes cannot
-- be retried merely because a lease or client request timed out.
CREATE TABLE IF NOT EXISTS album_multipart_completions (
    upload_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    operation_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    object_version VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (upload_id, resource_id),
    UNIQUE KEY album_multipart_completion_operation (operation_id),
    CONSTRAINT album_multipart_completion_fk FOREIGN KEY (upload_id, resource_id)
        REFERENCES album_multipart_intents(upload_id, resource_id),
    CHECK (state IN ('completing', 'unknown', 'retryable', 'assembled')),
    CHECK ((state = 'assembled' AND object_version IS NOT NULL) OR
           (state <> 'assembled' AND object_version IS NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
