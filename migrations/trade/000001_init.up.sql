-- 所有时间使用 UTC DATETIME(3)，金额使用分。此迁移建立约束，不代表业务流程已实现。
CREATE TABLE activity (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    title VARCHAR(128) NOT NULL,
    sku_id BIGINT UNSIGNED NOT NULL,
    sku_name VARCHAR(128) NOT NULL,
    price_cent BIGINT NOT NULL,
    quota INT NOT NULL,
    starts_at DATETIME(3) NOT NULL,
    ends_at DATETIME(3) NOT NULL,
    version BIGINT NOT NULL DEFAULT 1,
    status VARCHAR(16) NOT NULL DEFAULT 'DRAFT',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    CONSTRAINT chk_activity_values CHECK (price_cent > 0 AND quota > 0 AND ends_at > starts_at),
    CONSTRAINT chk_activity_status CHECK (status IN ('DRAFT', 'PUBLISHING', 'PUBLISHED', 'PAUSED'))
) ENGINE=InnoDB;

CREATE TABLE activity_request (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT UNSIGNED NOT NULL,
    request_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    parameter_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    activity_id BIGINT UNSIGNED NOT NULL,
    activity_version BIGINT NOT NULL,
    snapshot JSON NOT NULL COMMENT '受理时的活动、SKU、价格快照',
    order_no VARCHAR(64) NOT NULL,
    status VARCHAR(16) NOT NULL,
    failure_reason VARCHAR(255) NOT NULL DEFAULT '',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_request_user_id (user_id, request_id),
    UNIQUE KEY uk_request_order (order_no),
    KEY idx_request_recovery (status, updated_at),
    KEY idx_request_activity_user (activity_id, user_id),
    CONSTRAINT chk_request_status CHECK (status IN ('ACCEPTED', 'PROCESSING', 'ORDER_CREATED', 'FAILED'))
) ENGINE=InnoDB;

CREATE TABLE activity_participation (
    activity_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    holder_request_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status VARCHAR(16) NOT NULL,
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (activity_id, user_id),
    CONSTRAINT chk_participation_status CHECK (status IN ('AVAILABLE', 'HELD', 'CONSUMED'))
) ENGINE=InnoDB;

CREATE TABLE orders (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    order_no VARCHAR(64) NOT NULL,
    activity_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    amount_cent BIGINT NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    status VARCHAR(16) NOT NULL,
    inventory_status VARCHAR(16) NOT NULL DEFAULT 'LOCKED',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_orders_number (order_no),
    UNIQUE KEY uk_orders_activity_user (activity_id, user_id),
    KEY idx_orders_expiry (status, expires_at),
    KEY idx_orders_user (user_id, id),
    CONSTRAINT chk_order_amount CHECK (amount_cent > 0),
    CONSTRAINT chk_order_status CHECK (status IN ('PENDING_PAYMENT', 'PAID', 'CLOSING', 'CANCELED')),
    CONSTRAINT chk_order_inventory CHECK (inventory_status IN ('LOCKED', 'CONFIRMED', 'RELEASED'))
) ENGINE=InnoDB;

CREATE TABLE order_items (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    order_no VARCHAR(64) NOT NULL,
    sku_id BIGINT UNSIGNED NOT NULL,
    sku_name VARCHAR(128) NOT NULL,
    price_cent BIGINT NOT NULL,
    quantity INT NOT NULL DEFAULT 1,
    UNIQUE KEY uk_order_items_order_sku (order_no, sku_id),
    CONSTRAINT chk_order_item CHECK (quantity = 1 AND price_cent > 0)
) ENGINE=InnoDB;

CREATE TABLE operation_task (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    business_key VARCHAR(128) NOT NULL,
    action VARCHAR(64) NOT NULL,
    payload JSON NOT NULL,
    next_attempt_at DATETIME(3) NOT NULL,
    attempts INT NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL DEFAULT 'PENDING',
    lease_owner VARCHAR(128) NULL,
    lease_until DATETIME(3) NULL,
    last_error VARCHAR(512) NOT NULL DEFAULT '',
    UNIQUE KEY uk_task_business_action (business_key, action),
    KEY idx_task_scan (status, next_attempt_at),
    KEY idx_task_lease (status, lease_until),
    CONSTRAINT chk_task_status CHECK (status IN ('PENDING', 'PROCESSING', 'DONE', 'NEEDS_REVIEW'))
) ENGINE=InnoDB;
