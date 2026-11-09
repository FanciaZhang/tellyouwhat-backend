-- Stable account capacity, never a client-supplied quota or a device key binding.
ALTER TABLE album_storage_accounts ADD COLUMN subscription_revision BIGINT NOT NULL DEFAULT 0;

CREATE TABLE album_cloud_subscriptions (
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_transaction_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    transaction_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    product_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    quota_bytes BIGINT NOT NULL,
    active BOOLEAN NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    signed_at DATETIME(6) NOT NULL,
    verified_at DATETIME(6) NOT NULL,
    PRIMARY KEY (environment, original_transaction_id),
    KEY album_subscription_owner (owner_id, environment, expires_at),
    CONSTRAINT album_subscription_owner_fk FOREIGN KEY (owner_id) REFERENCES album_storage_accounts(owner_id),
    CHECK (environment IN ('Sandbox','Production')),
    CHECK ((product_id='albums.cloud.50gb.annual' AND quota_bytes=50000000000) OR
           (product_id='albums.cloud.500gb.annual' AND quota_bytes=500000000000))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
