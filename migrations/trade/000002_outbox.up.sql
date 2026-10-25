CREATE TABLE outbox (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    event_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    aggregate_id VARCHAR(64) NOT NULL,
    event_type VARCHAR(64) NOT NULL,
    payload JSON NOT NULL,
    trace_id VARCHAR(64) NOT NULL,
    schema_version INT NOT NULL DEFAULT 1,
    occurred_at DATETIME(3) NOT NULL,
    next_attempt_at DATETIME(3) NOT NULL,
    attempts INT NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL DEFAULT 'PENDING',
    lease_owner VARCHAR(128) NULL,
    lease_until DATETIME(3) NULL,
    last_error VARCHAR(512) NOT NULL DEFAULT '',
    UNIQUE KEY uk_outbox_event (event_id),
    KEY idx_outbox_scan (status, next_attempt_at),
    KEY idx_outbox_lease (status, lease_until),
    CONSTRAINT chk_outbox_status CHECK (status IN ('PENDING', 'PROCESSING', 'SENT', 'NEEDS_REVIEW'))
) ENGINE=InnoDB;

