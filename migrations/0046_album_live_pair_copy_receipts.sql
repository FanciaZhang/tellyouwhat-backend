-- A COPY receipt proves the write returned a version, not that its bytes were verified.
ALTER TABLE album_live_pairs
    ADD COLUMN photo_copy_version VARCHAR(1024) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    ADD COLUMN movie_copy_version VARCHAR(1024) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    ADD CONSTRAINT album_live_photo_copy_version_ck CHECK(photo_copy_version IS NULL OR (photo_consumed=TRUE AND photo_copy_version<>'' AND photo_copy_version<>'null')),
    ADD CONSTRAINT album_live_movie_copy_version_ck CHECK(movie_copy_version IS NULL OR (movie_consumed=TRUE AND movie_copy_version<>'' AND movie_copy_version<>'null'));

UPDATE album_live_pairs p JOIN JSON_TABLE(p.objects_json,'$[*]' COLUMNS(
    resource_id VARCHAR(96) PATH '$.resourceID', object_version VARCHAR(1024) PATH '$.versionID'
)) o ON o.resource_id='photo'
SET p.photo_copy_version=o.object_version WHERE p.photo_consumed=TRUE;
UPDATE album_live_pairs p JOIN JSON_TABLE(p.objects_json,'$[*]' COLUMNS(
    resource_id VARCHAR(96) PATH '$.resourceID', object_version VARCHAR(1024) PATH '$.versionID'
)) o ON o.resource_id='motion'
SET p.movie_copy_version=o.object_version WHERE p.movie_consumed=TRUE;
