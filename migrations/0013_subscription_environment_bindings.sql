ALTER TABLE app_attest_keys
    ADD COLUMN production_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    ADD COLUMN sandbox_transaction_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '';
