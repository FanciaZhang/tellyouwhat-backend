-- Independent of account foreign keys so final account erasure cannot erase
-- the intent needed to prevent an older snapshot from restoring access.
CREATE TABLE album_deletion_ledger (
 account_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
 authorizing_session_sha256 BINARY(32) NOT NULL,
 requested_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO album_deletion_ledger(account_id,request_id,authorizing_session_sha256,requested_at)
 SELECT account_id,request_id,authorizing_session_sha256,requested_at FROM album_account_deletions;
