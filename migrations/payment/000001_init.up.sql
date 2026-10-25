CREATE TABLE payment (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    order_no VARCHAR(64) NOT NULL,
    payment_no VARCHAR(64) NULL,
    user_id BIGINT UNSIGNED NULL,
    amount_cent BIGINT NULL,
    expires_at DATETIME(3) NULL,
    status VARCHAR(16) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_payment_order (order_no),
    UNIQUE KEY uk_payment_number (payment_no),
    KEY idx_payment_expiry (status, expires_at),
    CONSTRAINT chk_payment_status CHECK (status IN ('PENDING', 'SUCCESS', 'CLOSED')),
    -- 先到的关单仅带 orderNo；CLOSED 允许空业务字段，迟到创建必须尊重终态。
    CONSTRAINT chk_payment_fields CHECK (status = 'CLOSED' OR
        (payment_no IS NOT NULL AND user_id IS NOT NULL AND amount_cent IS NOT NULL AND amount_cent > 0 AND expires_at IS NOT NULL))
) ENGINE=InnoDB;
