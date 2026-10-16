-- APNs acceptance is not evidence that a device displayed a notification.
CREATE TABLE album_retention_deliveries (
    delivery_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    reminder_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    registration_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    registration_revision BIGINT NOT NULL,
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'pending',
    available_at DATETIME(6) NOT NULL,
    lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    accepted_at DATETIME(6) NULL,
    UNIQUE KEY album_delivery_target(reminder_id,registration_id,registration_revision),
    CONSTRAINT album_delivery_reminder_fk FOREIGN KEY(reminder_id) REFERENCES album_retention_reminders(reminder_id),
    CONSTRAINT album_delivery_registration_fk FOREIGN KEY(registration_id) REFERENCES album_push_registrations(registration_id),
    CHECK(registration_revision > 0),
    CHECK(state IN ('pending','accepted','rejected')),
    CHECK((state='accepted') = (accepted_at IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
