ALTER TABLE app_store_offer_redemptions ADD COLUMN product_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '';

CREATE TABLE offer_delivery_pools (
 app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 pool_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 offer_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 offer_name VARCHAR(255) NOT NULL,
 subscription_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
 product_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
 kind VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 capacity INT UNSIGNED NOT NULL,
 ciphertext MEDIUMBLOB,
 nonce VARBINARY(32),
 active BOOLEAN NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 synced_at DATETIME(6) NOT NULL,
 PRIMARY KEY(app_id,pool_id),
 CHECK(kind IN ('oneTime','custom')),
 CHECK(environment IN ('production','sandbox'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE offer_delivery_codes (
 app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 pool_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 code_hash BINARY(32) NOT NULL,
 ciphertext MEDIUMBLOB NOT NULL,
 nonce VARBINARY(32) NOT NULL,
 inventory_state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'external',
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY(app_id,id),
 UNIQUE KEY offer_delivery_code_unique(app_id,code_hash),
 INDEX offer_delivery_available(app_id,pool_id,inventory_state),
 FOREIGN KEY(app_id,pool_id) REFERENCES offer_delivery_pools(app_id,pool_id),
 CHECK(inventory_state IN ('available','external','assigned'))
) ENGINE=InnoDB;

CREATE TABLE offer_delivery_requests (
 app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 pool_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 request_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 request_hash BINARY(32) NOT NULL,
 ciphertext MEDIUMBLOB NOT NULL,
 nonce VARBINARY(32) NOT NULL,
 source VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'manual',
 status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 version INT UNSIGNED NOT NULL DEFAULT 1,
 code_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,
 requested_at DATETIME(6) NOT NULL,
 assigned_at DATETIME(6),
 delivered_at DATETIME(6),
 reported_redeemed_at DATETIME(6),
 verified_original_hash BINARY(32),
 verified_at DATETIME(6),
 claim_generation INT UNSIGNED NOT NULL DEFAULT 0,
 claim_expires_at DATETIME(6),
 PRIMARY KEY(app_id,id),
 UNIQUE KEY offer_delivery_request_key(app_id,request_key),
 UNIQUE KEY offer_delivery_assigned_code(app_id,code_id),
 INDEX offer_delivery_request_list(app_id,pool_id,requested_at,id),
 FOREIGN KEY(app_id,pool_id) REFERENCES offer_delivery_pools(app_id,pool_id),
 FOREIGN KEY(app_id,code_id) REFERENCES offer_delivery_codes(app_id,id),
 CHECK(status IN ('requested','assigned','delivered','cancelled','rejected'))
) ENGINE=InnoDB;

CREATE TABLE offer_delivery_events (
 id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
 app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 target_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 actor_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 action VARCHAR(40) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 quantity INT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL,
 INDEX offer_delivery_event_list(app_id,target_id,id)
) ENGINE=InnoDB;

CREATE TABLE offer_delivery_verified_links (
 app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 offer_name VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 original_hash BINARY(32) NOT NULL,
 linked_at DATETIME(6) NOT NULL,
 PRIMARY KEY(app_id,request_id),
 UNIQUE KEY offer_delivery_verified_subscription(app_id,environment,offer_name,original_hash),
 FOREIGN KEY(app_id,request_id) REFERENCES offer_delivery_requests(app_id,id)
) ENGINE=InnoDB;
