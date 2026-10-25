CREATE TABLE sku (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    price_cent BIGINT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    CONSTRAINT chk_sku_price CHECK (price_cent > 0)
) ENGINE=InnoDB;

CREATE TABLE inventory (
    sku_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
    total INT NOT NULL,
    available INT NOT NULL,
    locked INT NOT NULL DEFAULT 0,
    sold INT NOT NULL DEFAULT 0,
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    CONSTRAINT chk_inventory_nonnegative CHECK (total >= 0 AND available >= 0 AND locked >= 0 AND sold >= 0),
    CONSTRAINT chk_inventory_balance CHECK (available + locked + sold = total)
) ENGINE=InnoDB;

CREATE TABLE inventory_reservation (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    order_no VARCHAR(64) NOT NULL,
    sku_id BIGINT UNSIGNED NOT NULL,
    quantity INT NOT NULL,
    status VARCHAR(16) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_reservation_order_sku (order_no, sku_id),
    CONSTRAINT chk_reservation_quantity CHECK (quantity > 0),
    CONSTRAINT chk_reservation_status CHECK (status IN ('LOCKED', 'CONFIRMED', 'RELEASED'))
) ENGINE=InnoDB;
