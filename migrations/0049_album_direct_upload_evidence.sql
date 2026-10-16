-- Old rows may have exposed direct PUT URLs. Expiry does not stop an in-flight PUT.
-- New code explicitly initializes FALSE and commits TRUE before returning a URL.
ALTER TABLE album_uploads ADD COLUMN direct_upload_issued BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE album_live_pairs ADD COLUMN direct_upload_issued BOOLEAN NOT NULL DEFAULT TRUE;
