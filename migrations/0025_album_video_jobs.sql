-- One paid submission intent per immutable source and preset. Polling never
-- allocates another intent; an ambiguous POST is reconciled read-only.
CREATE TABLE IF NOT EXISTS album_video_jobs (
    job_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    manifest_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    preset VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'queued',
    failures INT NOT NULL DEFAULT 0,
    available_at DATETIME(6) NOT NULL,
    deadline_at DATETIME(6) NULL,
    lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    lease_until DATETIME(6) NULL,
    submitted_at DATETIME(6) NULL,
    spec_json JSON NULL,
    reserve_size BIGINT NOT NULL DEFAULT 0,
    provider_job_id VARCHAR(33) CHARACTER SET ascii COLLATE ascii_bin NULL,
    result_json JSON NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    UNIQUE KEY album_video_source (owner_id, version_id, preset),
    KEY album_video_ready (state, available_at, lease_until),
    CONSTRAINT album_video_source_fk FOREIGN KEY (owner_id, version_id) REFERENCES album_asset_versions(owner_id, version_id),
    CHECK (state IN ('queued','running','completed','attention')),
    CHECK (failures >= 0 AND failures <= 8),
    CHECK (reserve_size >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
