ALTER TABLE album_asset_lifecycle
 DROP CHECK album_asset_lifecycle_chk_2,
 ADD CONSTRAINT album_asset_lifecycle_state CHECK(state IN ('active','trashed','purged'));
ALTER TABLE album_library_changes
 DROP CHECK album_library_changes_chk_2,
 ADD CONSTRAINT album_library_change_state CHECK(state IN ('active','trashed','purged'));
ALTER TABLE album_object_purges
 DROP CHECK album_object_purges_chk_2,
 ADD CONSTRAINT album_object_purge_state CHECK(state IN ('queued','running','objects_removed','completed')),
 ADD COLUMN refunded_bytes BIGINT NULL,
 ADD COLUMN completed_at DATETIME(6) NULL,
 ADD CONSTRAINT album_object_purge_settled CHECK((state='completed')=(completed_at IS NOT NULL)),
 ADD CONSTRAINT album_object_purge_refund CHECK((completed_at IS NULL)=(refunded_bytes IS NULL) AND (refunded_bytes IS NULL OR refunded_bytes>0));
