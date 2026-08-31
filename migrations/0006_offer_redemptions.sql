CREATE TABLE IF NOT EXISTS app_store_offer_redemptions (
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    transaction_hash BINARY(32) NOT NULL,
    original_transaction_hash BINARY(32) NOT NULL,
    offer_identifier VARCHAR(255) NOT NULL,
    offer_type SMALLINT NOT NULL,
    redeemed_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (environment, transaction_hash),
    INDEX app_store_offer_redemptions_offer_idx (offer_identifier, environment, redeemed_at),
    INDEX app_store_offer_redemptions_original_idx (original_transaction_hash, offer_identifier)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
