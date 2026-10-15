ALTER TABLE album_account_deletions
 ADD COLUMN media_purge_after DATETIME(6) NULL,
 ADD KEY album_account_media_purge_schedule(media_purge_after,account_id);
