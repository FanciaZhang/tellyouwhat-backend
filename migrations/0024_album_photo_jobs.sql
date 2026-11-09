CREATE TABLE IF NOT EXISTS album_photo_jobs (
    job_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    preset VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'queued',
    attempts INT NOT NULL DEFAULT 0,
    available_at DATETIME(6) NOT NULL,
    lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    lease_until DATETIME(6) NULL,
    updated_at DATETIME(6) NOT NULL,
    UNIQUE KEY album_photo_job_source (owner_id, version_id, preset),
    KEY album_photo_job_ready (state, available_at, lease_until),
    CONSTRAINT album_photo_job_source_fk FOREIGN KEY (owner_id, version_id) REFERENCES album_asset_versions(owner_id, version_id),
    CHECK (state IN ('queued','running','completed','attention')),
    CHECK (attempts >= 0 AND attempts <= 8)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
