CREATE TABLE album_deletion_apple_jobs (
    grant_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    account_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    available_at DATETIME(6) NOT NULL,
    attempts INT UNSIGNED NOT NULL DEFAULT 0,
    lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    lease_until DATETIME(6) NULL,
    acknowledged_at DATETIME(6) NULL,
    KEY album_apple_job_pending(acknowledged_at,available_at,lease_until),
    CONSTRAINT album_apple_job_grant FOREIGN KEY(grant_id) REFERENCES album_apple_grants(grant_id),
    CONSTRAINT album_apple_job_request FOREIGN KEY(request_id) REFERENCES album_account_deletions(request_id),
    CONSTRAINT album_apple_job_account FOREIGN KEY(account_id) REFERENCES album_accounts(account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Existing accepted requests also need every grant, including locally revoked ones.
INSERT INTO album_deletion_apple_jobs(grant_id,account_id,request_id,available_at)
SELECT g.grant_id,g.account_id,d.request_id,d.requested_at
FROM album_apple_grants g JOIN album_account_deletions d ON d.account_id=g.account_id;
