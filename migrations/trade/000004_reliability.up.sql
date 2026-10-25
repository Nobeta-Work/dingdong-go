-- 所有准入代次由数据库推进；Redis 丢失时先切换代次，再允许重建后的新请求提交。
ALTER TABLE activity
    ADD COLUMN admission_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN admission_state VARCHAR(16) NOT NULL DEFAULT 'COLD',
    ADD COLUMN admission_revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
    ADD CONSTRAINT chk_admission_state CHECK (admission_state IN ('COLD','RECOVERING','ACTIVE'));
ALTER TABLE activity_request ADD COLUMN admission_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0;
ALTER TABLE operation_task ADD COLUMN trace_id VARCHAR(64) NOT NULL DEFAULT '';
UPDATE operation_task SET action='InventoryReleaseRequested' WHERE action='RELEASE_BEFORE_ORDER';
CREATE TABLE event_inbox (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    event_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload MEDIUMTEXT NOT NULL COMMENT '保留原始消息，非法 JSON 也能进入待人工检查状态',
    attempts INT NOT NULL DEFAULT 0,
    deliveries INT NOT NULL DEFAULT 1,
    next_attempt_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    status VARCHAR(16) NOT NULL DEFAULT 'PENDING',
    lease_owner VARCHAR(128) NULL,
    lease_until DATETIME(3) NULL,
    last_error VARCHAR(512) NOT NULL DEFAULT '',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_inbox_event (event_id),
    KEY idx_inbox_scan (status, next_attempt_at),
    KEY idx_inbox_lease (status, lease_until),
    CONSTRAINT chk_inbox_status CHECK (status IN ('PENDING','PROCESSING','DONE','NEEDS_REVIEW'))
) ENGINE=InnoDB;

