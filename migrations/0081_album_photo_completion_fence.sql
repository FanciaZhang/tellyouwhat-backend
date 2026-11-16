-- Existing attempts cannot prove Complete was never issued; keep them unknown.
SET @photo_phase_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='album_photo_writes' AND column_name='multipart_phase'), 'SELECT 1', 'ALTER TABLE album_photo_writes ADD COLUMN multipart_phase ENUM(''unknown'',''ready'',''completing'',''aborted'') NOT NULL DEFAULT ''unknown''');
PREPARE photo_phase_statement FROM @photo_phase_ddl;
EXECUTE photo_phase_statement;
DEALLOCATE PREPARE photo_phase_statement;
SET @photo_phase_ddl = IF(EXISTS(SELECT 1 FROM information_schema.table_constraints WHERE constraint_schema=DATABASE() AND table_name='album_photo_writes' AND constraint_name='album_photo_completion_fence'), 'SELECT 1', 'ALTER TABLE album_photo_writes ADD CONSTRAINT album_photo_completion_fence CHECK((multipart_phase=''unknown'' OR multipart=TRUE) AND (multipart_phase<>''ready'' OR object_version IS NULL) AND (multipart_phase<>''completing'' OR provider_upload_id IS NOT NULL) AND (multipart_phase<>''aborted'' OR (provider_upload_id IS NOT NULL AND object_version IS NULL AND retirement_started_at IS NOT NULL)))');
PREPARE photo_phase_statement FROM @photo_phase_ddl;
EXECUTE photo_phase_statement;
DEALLOCATE PREPARE photo_phase_statement;
