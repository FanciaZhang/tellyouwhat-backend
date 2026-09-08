ALTER TABLE ai_cost_attempts
 ADD COLUMN audience VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'unknown',
 ADD COLUMN environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'unknown',
 ADD COLUMN usage_known BOOLEAN NOT NULL DEFAULT FALSE,
 ADD INDEX ai_cost_created_audience_idx (created_at, app_id, audience);

ALTER TABLE managed_entitlements ADD COLUMN price_milli BIGINT NULL;

CREATE TABLE operations_collection (
 singleton_id TINYINT UNSIGNED PRIMARY KEY,
 started_at DATETIME(6) NOT NULL,
 CHECK (singleton_id = 1)
) ENGINE=InnoDB;
INSERT INTO operations_collection VALUES (1, UTC_TIMESTAMP(6));

CREATE TABLE operations_free_cohorts (
 app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 first_free_at DATETIME(6) NOT NULL,
 PRIMARY KEY (app_id, key_id),
 INDEX operations_cohort_time_idx (first_free_at),
 FOREIGN KEY (app_id, key_id) REFERENCES app_attest_keys(app_id, key_id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE operations_purchase_observations (
 app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 key_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 original_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 price_milli BIGINT,
 currency VARCHAR(3) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 purchased_at DATETIME(6) NOT NULL,
 started_at DATETIME(6),
 signed_at DATETIME(6) NOT NULL,
 revoked_at DATETIME(6),
 observed_at DATETIME(6) NOT NULL,
 PRIMARY KEY (app_id, key_id, transaction_id),
 INDEX operations_purchase_original_idx (app_id, original_transaction_id, environment),
 FOREIGN KEY (app_id, key_id) REFERENCES app_attest_keys(app_id, key_id) ON DELETE CASCADE
) ENGINE=InnoDB;
