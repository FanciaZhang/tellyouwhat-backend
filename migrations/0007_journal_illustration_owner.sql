ALTER TABLE journal_illustration_tasks
    ADD CONSTRAINT journal_illustration_owner_fk
    FOREIGN KEY (app_id, owner_id) REFERENCES app_attest_keys(app_id, key_id)
    ON DELETE CASCADE;
