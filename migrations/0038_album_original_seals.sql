-- Stop old verification workers before migration: their COPY attempts were not journaled.
CREATE TABLE album_original_seals (
 upload_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 resource_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 manifest_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 consumed_at DATETIME(6) NOT NULL,
 object_version VARCHAR(1024) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
 legacy BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY(upload_id,resource_id),
 CONSTRAINT album_original_seal_upload_fk FOREIGN KEY(upload_id) REFERENCES album_uploads(upload_id) ON DELETE CASCADE,
 CHECK(object_version IS NULL OR (object_version<>'' AND object_version<>'null'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- A pre-migration verification may already have sent COPY. Seed consumed intents,
-- never fabricate evidence that it issued only one request.
INSERT INTO album_original_seals(upload_id,resource_id,manifest_digest,consumed_at,object_version,legacy)
 SELECT u.upload_id,r.resource_id,u.manifest_digest,u.updated_at,v.object_version,TRUE
 FROM album_uploads u
 JOIN JSON_TABLE(u.manifest_json,'$.resources[*]' COLUMNS(resource_id VARCHAR(96) PATH '$.id')) r
 LEFT JOIN JSON_TABLE(COALESCE(u.sealed_objects,JSON_ARRAY()),'$[*]' COLUMNS(resource_id VARCHAR(96) PATH '$.resourceID',object_version VARCHAR(1024) PATH '$.versionID')) v ON v.resource_id=r.resource_id
 WHERE u.state IN ('verifying','originals_verified');
