CREATE TABLE album_live_pair_multiparts (
    pair_upload_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    intent_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider_upload_id VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NULL,
    operation_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    object_version VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (pair_upload_id, resource_id),
    UNIQUE KEY album_live_multipart_intent (intent_id),
    CONSTRAINT album_live_multipart_pair_fk FOREIGN KEY (pair_upload_id) REFERENCES album_live_pairs(pair_upload_id),
    CHECK (resource_id IN ('photo','motion')),
    CHECK (state IN ('initiating','unknown','ready','completing','completion_unknown','assembled','aborting','aborted')),
    CHECK ((state IN ('initiating','unknown') AND provider_upload_id IS NULL) OR (state NOT IN ('initiating','unknown') AND provider_upload_id IS NOT NULL)),
    CHECK ((state IN ('completing','completion_unknown','assembled','aborting','aborted') AND operation_id IS NOT NULL) OR (state IN ('initiating','unknown','ready') AND operation_id IS NULL)),
    CHECK ((state='assembled' AND object_version IS NOT NULL) OR (state<>'assembled' AND object_version IS NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
