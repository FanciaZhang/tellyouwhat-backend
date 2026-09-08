CREATE TABLE prompt_eval_samples (
 id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 payload LONGBLOB NOT NULL,
 nonce VARBINARY(12) NOT NULL,
 created_by CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 created_at DATETIME(6) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 INDEX prompt_eval_sample_expiry (expires_at)
) ENGINE=InnoDB;
CREATE TABLE prompt_eval_runs (
 id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 actor_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 request_hash BINARY(32) NOT NULL,
 candidate_revisions JSON NOT NULL,
 payload LONGBLOB NOT NULL,
 nonce VARBINARY(12) NOT NULL,
 status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 created_at DATETIME(6) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 UNIQUE KEY prompt_eval_command (actor_id,idempotency_key),
 INDEX prompt_eval_run_expiry (expires_at)
) ENGINE=InnoDB;
CREATE TABLE prompt_eval_items (
 run_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 item_index INT NOT NULL,
 status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 lease_owner CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,
 lease_until DATETIME(6),
 payload LONGBLOB,
 nonce VARBINARY(12),
 PRIMARY KEY(run_id,item_index),
 FOREIGN KEY(run_id) REFERENCES prompt_eval_runs(id) ON DELETE CASCADE,
 INDEX prompt_eval_claim(status,lease_until)
) ENGINE=InnoDB;
CREATE TABLE prompt_eval_budget_holds (
 run_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 month_start DATE NOT NULL,
 remaining_nanos BIGINT NOT NULL,
 reserved_nanos BIGINT NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 CHECK(remaining_nanos >= 0),
 INDEX prompt_eval_budget_month(app_id,month_start)
) ENGINE=InnoDB;
