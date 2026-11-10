-- Independent of live account/media rows: historical recovery must retain this fence.
CREATE TABLE IF NOT EXISTS album_asset_deletion_ledger (
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    asset_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    final_revision BIGINT NOT NULL,
    reason VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    purged_at DATETIME(6) NOT NULL,
    version_ids JSON NOT NULL,
    PRIMARY KEY (owner_id, asset_id),
    CHECK (final_revision > 0),
    CHECK (reason IN ('trash', 'retention')),
    CHECK (JSON_TYPE(version_ids) = 'ARRAY' AND JSON_LENGTH(version_ids) BETWEEN 1 AND 1000)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Backfill only completed purges with a matching final feed, never infer from age.
INSERT INTO album_asset_deletion_ledger(owner_id,asset_id,final_revision,reason,purged_at,version_ids)
SELECT p.owner_id,p.asset_id,p.revision+1,p.reason,p.completed_at,JSON_ARRAYAGG(c.version_id)
FROM album_object_purges p
JOIN album_library_changes c ON c.owner_id=p.owner_id AND c.asset_id=p.asset_id
    AND c.state='purged' AND c.revision=p.revision+1
LEFT JOIN album_asset_deletion_ledger l ON l.owner_id=p.owner_id AND l.asset_id=p.asset_id
WHERE p.state='completed' AND p.refunded_bytes>0 AND p.completed_at IS NOT NULL AND l.owner_id IS NULL
GROUP BY p.owner_id,p.asset_id,p.revision,p.reason,p.completed_at;
