ALTER TABLE album_cloud_subscriptions
    ADD COLUMN next_check_at DATETIME(6) NOT NULL DEFAULT '1970-01-01 00:00:00.000000',
    ADD COLUMN refresh_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN refresh_until DATETIME(6) NULL,
    ADD COLUMN refresh_failures INT NOT NULL DEFAULT 0,
    ADD KEY album_subscription_due (environment,next_check_at,refresh_until),
    ADD CHECK (refresh_failures BETWEEN 0 AND 255);

-- Only verified semantic fingerprints are retained. No transaction JWS, Apple
-- credentials, notification raw body, or user media is persisted here.
CREATE TABLE album_subscription_notifications (
    environment VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    notification_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    notification_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload_digest BINARY(32) NOT NULL,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    original_transaction_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    signed_at DATETIME(6) NOT NULL,
    processed_at DATETIME(6) NOT NULL,
    PRIMARY KEY(environment,notification_id),
    KEY album_subscription_notification_owner(owner_id,processed_at),
    CONSTRAINT album_subscription_notification_owner_fk FOREIGN KEY(owner_id) REFERENCES album_storage_accounts(owner_id),
    CHECK(environment IN ('Sandbox','Production'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
