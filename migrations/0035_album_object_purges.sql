-- Internal expired-trash object work only. Metadata/quota settlement is separate.
CREATE TABLE album_object_purges (
 owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 asset_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 revision BIGINT NOT NULL,
 trash_expires_at DATETIME(6) NOT NULL,
 targets_json JSON NOT NULL,
 target_index INT NOT NULL DEFAULT 0,
 plan_json JSON NULL,
 action_index INT NOT NULL DEFAULT 0,
 state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'queued',
 lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
 lease_until DATETIME(6) NULL,
 available_at DATETIME(6) NOT NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY(owner_id,asset_id),
 KEY album_object_purge_ready(state,available_at,lease_until),
 CONSTRAINT album_object_purge_asset_fk FOREIGN KEY(owner_id,asset_id) REFERENCES album_asset_lifecycle(owner_id,asset_id),
 CHECK(revision>0 AND target_index>=0 AND action_index>=0),
 CHECK(state IN ('queued','running','objects_removed')),
 CHECK((state='running')=(lease_token IS NOT NULL)),
 CHECK((lease_token IS NULL)=(lease_until IS NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
