-- Restored databases stay offline until a separately verified cutover exists.
CREATE TABLE album_recovery_state (
 singleton_id TINYINT UNSIGNED PRIMARY KEY,
 quarantined BOOLEAN NOT NULL DEFAULT FALSE,
 quarantined_at DATETIME(6) NULL,
 CHECK(singleton_id=1),
 CHECK(quarantined IN (0,1)),
 CHECK((quarantined=TRUE)=(quarantined_at IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
INSERT INTO album_recovery_state(singleton_id) VALUES(1);
