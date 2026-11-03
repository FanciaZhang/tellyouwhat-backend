CREATE TABLE health_ai_rollout_commands (
 id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 actor CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 request_hash BINARY(32) NOT NULL,
 endpoint VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 document JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 updated_at DATETIME(6) NOT NULL,
 UNIQUE KEY health_ai_rollout_replay (actor,idempotency_key),
 INDEX health_ai_rollout_pending (state,updated_at),
 INDEX health_ai_rollout_endpoint (endpoint,created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE health_ai_endpoint_prices (
 endpoint VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 document JSON NOT NULL,
 updated_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE health_ai_model_attempts (
 id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 endpoint VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 operation VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 policy_version VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 actual_model VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 price JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 INDEX health_ai_model_attempt_history (endpoint,created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
