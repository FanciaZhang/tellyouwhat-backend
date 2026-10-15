-- Logical cloud deletion never mutates the system photo library or removes bytes.
CREATE TABLE album_asset_lifecycle (
 owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 asset_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 revision BIGINT NOT NULL DEFAULT 0,
 lifecycle_epoch BIGINT NOT NULL DEFAULT 0,
 state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'active',
 trashed_at DATETIME(6) NULL,
 trash_expires_at DATETIME(6) NULL,
 PRIMARY KEY(owner_id,asset_id),
 KEY album_trash_due(state,trash_expires_at,owner_id),
 CONSTRAINT album_asset_lifecycle_owner_fk FOREIGN KEY(owner_id) REFERENCES album_storage_accounts(owner_id) ON DELETE CASCADE,
 CHECK(revision>=0 AND lifecycle_epoch>=0),
 CHECK(state IN ('active','trashed')),
 CHECK((state='trashed')=(trashed_at IS NOT NULL)),
 CHECK((trashed_at IS NULL)=(trash_expires_at IS NULL)),
 CHECK(trash_expires_at IS NULL OR trashed_at<trash_expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO album_asset_lifecycle(owner_id,asset_id,revision)
 SELECT owner_id,asset_id,COUNT(*) FROM album_asset_versions GROUP BY owner_id,asset_id;
ALTER TABLE album_uploads
 ADD COLUMN lifecycle_epoch BIGINT NOT NULL DEFAULT 0,
 DROP INDEX album_upload_manifest,
 ADD UNIQUE KEY album_upload_manifest(owner_id,manifest_digest,lifecycle_epoch);

-- Each event is immutable. Original publication cursors remain independent.
CREATE TABLE album_library_changes (
 sequence_id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
 owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 asset_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 revision BIGINT NOT NULL,
 state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 changed_at DATETIME(6) NOT NULL,
 trashed_at DATETIME(6) NULL,
 trash_expires_at DATETIME(6) NULL,
 UNIQUE KEY album_change_identity(owner_id,version_id,revision),
 KEY album_changes_owner(owner_id,sequence_id),
 CONSTRAINT album_changes_owner_fk FOREIGN KEY(owner_id) REFERENCES album_storage_accounts(owner_id) ON DELETE CASCADE,
 CHECK(revision>0),
 CHECK(state IN ('active','trashed')),
 CHECK((state='trashed')=(trashed_at IS NOT NULL)),
 CHECK((trashed_at IS NULL)=(trash_expires_at IS NULL)),
 CHECK(trash_expires_at IS NULL OR trashed_at<trash_expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
INSERT INTO album_library_changes(owner_id,asset_id,version_id,revision,state,changed_at)
 SELECT owner_id,asset_id,version_id,ROW_NUMBER() OVER(PARTITION BY owner_id,asset_id ORDER BY sequence_id),'active',published_at
 FROM album_asset_versions ORDER BY sequence_id;

CREATE TABLE album_asset_operations (
 owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 asset_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 expected_revision BIGINT NOT NULL,
 action VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 result_json JSON NOT NULL,
 PRIMARY KEY(owner_id,request_id),
 CONSTRAINT album_asset_operation_owner_fk FOREIGN KEY(owner_id) REFERENCES album_storage_accounts(owner_id) ON DELETE CASCADE,
 CHECK(expected_revision>0),
 CHECK(action IN ('trash','restore'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
