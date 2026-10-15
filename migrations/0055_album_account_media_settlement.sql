-- Media/index erasure only; identity, billing and backup erasure remain separate.
ALTER TABLE album_account_object_purges
 ADD COLUMN media_settled_at DATETIME(6) NULL,
 ADD CONSTRAINT album_account_media_settled CHECK(media_settled_at IS NULL OR
   (state='objects_removed' AND media_settled_at>=objects_removed_at));
