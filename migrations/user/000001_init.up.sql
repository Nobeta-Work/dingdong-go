CREATE TABLE users (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    username VARCHAR(64) NOT NULL,
    password_hash VARCHAR(100) NOT NULL COMMENT 'bcrypt 哈希，不存储明文密码',
    role VARCHAR(16) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_users_username (username),
    CONSTRAINT chk_users_role CHECK (role IN ('USER', 'OPERATOR'))
) ENGINE=InnoDB;
