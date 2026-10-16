ALTER TABLE album_object_purges
 ADD COLUMN reason VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'trash',
 ADD COLUMN environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
 ADD COLUMN subscription_revision BIGINT NULL,
 ADD COLUMN verification_until DATETIME(6) NULL,
 ADD COLUMN started_at DATETIME(6) NULL,
 ADD CONSTRAINT album_purge_reason CHECK(reason IN ('trash','retention')),
 ADD CONSTRAINT album_purge_retention_proof CHECK(
  (reason='trash' AND environment IS NULL AND subscription_revision IS NULL AND verification_until IS NULL AND started_at IS NULL) OR
  (reason='retention' AND environment IS NOT NULL AND subscription_revision IS NOT NULL AND environment IN ('Sandbox','Production') AND subscription_revision>0 AND verification_until IS NOT NULL));
ALTER TABLE album_asset_lifecycle
 DROP CHECK album_asset_lifecycle_state,
 ADD CONSTRAINT album_asset_lifecycle_state CHECK(state IN ('active','trashed','purging','purged'));
