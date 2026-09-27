CREATE TABLE IF NOT EXISTS journal_image_budgets (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    month CHAR(7) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    reserved BIGINT UNSIGNED NOT NULL DEFAULT 0,
    used BIGINT UNSIGNED NOT NULL DEFAULT 0,
    PRIMARY KEY (app_id, account_id, month)
) ENGINE=InnoDB;
