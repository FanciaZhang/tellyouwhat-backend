CREATE TABLE album_deletion_video_jobs (
    job_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    account_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    available_at DATETIME(6) NOT NULL,
    attempts INT UNSIGNED NOT NULL DEFAULT 0,
    lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    lease_until DATETIME(6) NULL,
    acknowledged_at DATETIME(6) NULL,
    provider_job_id VARCHAR(33) CHARACTER SET ascii COLLATE ascii_bin NULL,
    provider_succeeded BOOLEAN NULL,
    KEY album_deletion_video_pending(acknowledged_at,available_at,lease_until),
    CONSTRAINT album_deletion_video_job FOREIGN KEY(job_id) REFERENCES album_video_jobs(job_id),
    CONSTRAINT album_deletion_video_request FOREIGN KEY(request_id) REFERENCES album_account_deletions(request_id),
    CONSTRAINT album_deletion_video_account FOREIGN KEY(account_id) REFERENCES album_accounts(account_id),
    CHECK ((acknowledged_at IS NULL AND provider_job_id IS NULL AND provider_succeeded IS NULL)
        OR (acknowledged_at IS NOT NULL AND provider_job_id IS NOT NULL AND provider_succeeded IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- A consumed submission intent may have reached CI even when its job ID was lost.
INSERT INTO album_deletion_video_jobs(job_id,account_id,request_id,available_at)
SELECT j.job_id,j.owner_id,d.request_id,d.requested_at
FROM album_video_jobs j JOIN album_account_deletions d ON d.account_id=j.owner_id
WHERE j.spec_json IS NOT NULL;
