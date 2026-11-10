-- Existing direct-PUT attempts keep their recovery semantics and stored bytes.
SET @photo_multipart_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='album_photo_writes' AND column_name='multipart'), 'SELECT 1', 'ALTER TABLE album_photo_writes ADD COLUMN multipart BOOLEAN NOT NULL DEFAULT FALSE');
PREPARE photo_multipart_statement FROM @photo_multipart_ddl;
EXECUTE photo_multipart_statement;
DEALLOCATE PREPARE photo_multipart_statement;
SET @photo_multipart_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='album_photo_writes' AND column_name='provider_upload_id'), 'SELECT 1', 'ALTER TABLE album_photo_writes ADD COLUMN provider_upload_id VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NULL');
PREPARE photo_multipart_statement FROM @photo_multipart_ddl;
EXECUTE photo_multipart_statement;
DEALLOCATE PREPARE photo_multipart_statement;
SET @photo_multipart_ddl = IF(EXISTS(SELECT 1 FROM information_schema.table_constraints WHERE constraint_schema=DATABASE() AND table_name='album_photo_writes' AND constraint_name='album_photo_multipart_id'), 'SELECT 1', 'ALTER TABLE album_photo_writes ADD CONSTRAINT album_photo_multipart_id CHECK((provider_upload_id IS NULL OR multipart=TRUE) AND (multipart=FALSE OR object_version IS NULL OR provider_upload_id IS NOT NULL))');
PREPARE photo_multipart_statement FROM @photo_multipart_ddl;
EXECUTE photo_multipart_statement;
DEALLOCATE PREPARE photo_multipart_statement;
