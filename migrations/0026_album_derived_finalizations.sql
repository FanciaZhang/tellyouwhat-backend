-- Closing an immutable version forbids new paid/write intents. Uncertain
-- attempts retain their bounded escrow until recovery or verified retirement.
-- This indexed flag is only a scheduling hint. The immutable ledger below is
-- authoritative and is committed in the same transaction as closing the flag.
-- MySQL DDL auto-commits. Guard each addition so an interruption before the
-- migration ledger write can safely resume without duplicate-column failures.
SET @album_derived_migration_ddl = IF(EXISTS(
    SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='album_uploads' AND column_name='derived_budget_closed'
), 'SELECT 1', 'ALTER TABLE album_uploads ADD COLUMN derived_budget_closed BOOLEAN NOT NULL DEFAULT FALSE');
PREPARE album_derived_migration_stmt FROM @album_derived_migration_ddl;
EXECUTE album_derived_migration_stmt;
DEALLOCATE PREPARE album_derived_migration_stmt;
SET @album_derived_migration_ddl = IF(EXISTS(
    SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='album_uploads' AND index_name='album_derived_pending'
), 'SELECT 1', 'ALTER TABLE album_uploads ADD KEY album_derived_pending (state, derived_budget_closed, updated_at, upload_id)');
PREPARE album_derived_migration_stmt FROM @album_derived_migration_ddl;
EXECUTE album_derived_migration_stmt;
DEALLOCATE PREPARE album_derived_migration_stmt;

CREATE TABLE IF NOT EXISTS album_derived_finalizations (
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    manifest_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    budget_bytes BIGINT NOT NULL,
    retained_bytes BIGINT NOT NULL,
    held_bytes BIGINT NOT NULL,
    released_bytes BIGINT NOT NULL,
    closed_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (owner_id, version_id),
    CONSTRAINT album_derived_finalization_source_fk FOREIGN KEY (owner_id, version_id) REFERENCES album_asset_versions(owner_id, version_id),
    CHECK (budget_bytes > 0 AND retained_bytes >= 0 AND held_bytes >= 0 AND released_bytes >= 0),
    CHECK (retained_bytes + held_bytes + released_bytes = budget_bytes)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS album_derived_cleanups (
    cleanup_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    manifest_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    reference_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    reserve_bytes BIGINT NOT NULL,
    keep_version VARCHAR(1024) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'queued',
    attempts INT NOT NULL DEFAULT 0,
    available_at DATETIME(6) NOT NULL,
    lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    lease_until DATETIME(6) NULL,
    plan_json JSON NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    UNIQUE KEY album_cleanup_reference (owner_id, kind, reference_id),
    KEY album_cleanup_ready (state, available_at, lease_until),
    CONSTRAINT album_cleanup_finalization_fk FOREIGN KEY (owner_id, version_id) REFERENCES album_derived_finalizations(owner_id, version_id),
    CHECK (kind IN ('photo','video')),
    CHECK (state IN ('queued','running','completed','attention')),
    CHECK (attempts >= 0 AND attempts <= 8 AND reserve_bytes >= 0),
    CHECK ((keep_version IS NULL AND reserve_bytes > 0) OR (keep_version IS NOT NULL AND reserve_bytes = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

SET @album_derived_migration_ddl = IF(EXISTS(
    SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='album_photo_writes' AND column_name='retirement_started_at'
), 'SELECT 1', 'ALTER TABLE album_photo_writes ADD COLUMN retirement_started_at DATETIME(6) NULL');
PREPARE album_derived_migration_stmt FROM @album_derived_migration_ddl;
EXECUTE album_derived_migration_stmt;
DEALLOCATE PREPARE album_derived_migration_stmt;
SET @album_derived_migration_ddl = IF(EXISTS(
    SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='album_photo_writes' AND column_name='retired_at'
), 'SELECT 1', 'ALTER TABLE album_photo_writes ADD COLUMN retired_at DATETIME(6) NULL');
PREPARE album_derived_migration_stmt FROM @album_derived_migration_ddl;
EXECUTE album_derived_migration_stmt;
DEALLOCATE PREPARE album_derived_migration_stmt;
SET @album_derived_migration_ddl = IF(EXISTS(
    SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='album_video_jobs' AND column_name='retirement_started_at'
), 'SELECT 1', 'ALTER TABLE album_video_jobs ADD COLUMN retirement_started_at DATETIME(6) NULL');
PREPARE album_derived_migration_stmt FROM @album_derived_migration_ddl;
EXECUTE album_derived_migration_stmt;
DEALLOCATE PREPARE album_derived_migration_stmt;
SET @album_derived_migration_ddl = IF(EXISTS(
    SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='album_video_jobs' AND column_name='retired_at'
), 'SELECT 1', 'ALTER TABLE album_video_jobs ADD COLUMN retired_at DATETIME(6) NULL');
PREPARE album_derived_migration_stmt FROM @album_derived_migration_ddl;
EXECUTE album_derived_migration_stmt;
DEALLOCATE PREPARE album_derived_migration_stmt;
