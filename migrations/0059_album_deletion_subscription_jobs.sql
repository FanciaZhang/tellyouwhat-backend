-- Binding receipts do not prove media, identity or backup erasure.
CREATE TABLE album_deletion_subscription_jobs (
 environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 original_transaction_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 account_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 target_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 available_at DATETIME(6) NOT NULL,
 attempts INT UNSIGNED NOT NULL DEFAULT 0,
 lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
 lease_until DATETIME(6) NULL,
 verified_transaction_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
 verified_signed_at DATETIME(6) NULL,
 acknowledged_at DATETIME(6) NULL,
 PRIMARY KEY(environment,original_transaction_id),
 KEY album_subscription_binding_pending(environment,acknowledged_at,available_at,lease_until),
 CONSTRAINT album_binding_chain_fk FOREIGN KEY(environment,original_transaction_id) REFERENCES album_cloud_subscriptions(environment,original_transaction_id),
 CONSTRAINT album_binding_request_fk FOREIGN KEY(request_id) REFERENCES album_account_deletions(request_id),
 CHECK((lease_token IS NULL)=(lease_until IS NULL)),
 CHECK((acknowledged_at IS NULL)=(verified_transaction_id IS NULL)),
 CHECK((acknowledged_at IS NULL)=(verified_signed_at IS NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO album_deletion_subscription_jobs(environment,original_transaction_id,account_id,request_id,target_token,available_at)
 SELECT c.environment,c.original_transaction_id,c.owner_id,d.request_id,a.storekit_account_token,d.requested_at
 FROM album_cloud_subscriptions c JOIN album_account_deletions d ON d.account_id=c.owner_id JOIN album_accounts a ON a.account_id=c.owner_id;
