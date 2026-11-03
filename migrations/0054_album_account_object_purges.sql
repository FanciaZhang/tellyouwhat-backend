-- Object removal only. Account metadata and backup erasure require separate receipts.
CREATE TABLE album_account_object_purges (
 account_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 area_index INT NOT NULL DEFAULT 0,
 plan_json JSON NULL,
 action_index INT NOT NULL DEFAULT 0,
 lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
 lease_until DATETIME(6) NULL,
 objects_removed_at DATETIME(6) NULL,
 updated_at DATETIME(6) NOT NULL,
 CONSTRAINT album_account_purge_deletion_fk FOREIGN KEY(account_id) REFERENCES album_account_deletions(account_id),
 UNIQUE KEY album_account_purge_request(request_id),
 CHECK(state IN ('queued','running','objects_removed')),
 CHECK(area_index BETWEEN 0 AND 6 AND action_index BETWEEN 0 AND 1000),
 CHECK((state='running')=(lease_token IS NOT NULL)),
 CHECK((lease_token IS NULL)=(lease_until IS NULL)),
 CHECK((state='objects_removed')=(objects_removed_at IS NOT NULL)),
 CHECK((state='objects_removed')=(area_index=6)),
 CHECK(plan_json IS NOT NULL OR action_index=0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
