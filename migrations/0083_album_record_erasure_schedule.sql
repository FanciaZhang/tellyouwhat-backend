-- A retry deadline keeps one unfinished account from blocking later requests.
SET @record_erasure_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='album_account_deletions' AND column_name='record_erase_after'), 'SELECT 1', 'ALTER TABLE album_account_deletions ADD COLUMN record_erase_after DATETIME(6) NULL, ADD KEY album_record_erase_schedule(record_erase_after,account_id)');
PREPARE record_erasure_statement FROM @record_erasure_ddl;
EXECUTE record_erasure_statement;
DEALLOCATE PREPARE record_erasure_statement;
