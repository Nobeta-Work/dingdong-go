-- 仅供本地 Demo。MySQL 空卷首次启动时执行，正常重启不会重置数据。
CREATE DATABASE IF NOT EXISTS dingdong_user CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE DATABASE IF NOT EXISTS dingdong_catalog CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE DATABASE IF NOT EXISTS dingdong_trade CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE DATABASE IF NOT EXISTS dingdong_payment CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE USER IF NOT EXISTS 'dingdong_user'@'%' IDENTIFIED BY 'demo-user';
CREATE USER IF NOT EXISTS 'dingdong_catalog'@'%' IDENTIFIED BY 'demo-catalog';
CREATE USER IF NOT EXISTS 'dingdong_trade'@'%' IDENTIFIED BY 'demo-trade';
CREATE USER IF NOT EXISTS 'dingdong_payment'@'%' IDENTIFIED BY 'demo-payment';
-- 应用账号只允许所属库 DML；迁移使用单独的库级 DDL 账号。
GRANT SELECT, INSERT, UPDATE, DELETE ON dingdong_user.* TO 'dingdong_user'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON dingdong_catalog.* TO 'dingdong_catalog'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON dingdong_trade.* TO 'dingdong_trade'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON dingdong_payment.* TO 'dingdong_payment'@'%';

CREATE USER IF NOT EXISTS 'migrate_user'@'%' IDENTIFIED BY 'demo-migrate-user';
CREATE USER IF NOT EXISTS 'migrate_catalog'@'%' IDENTIFIED BY 'demo-migrate-catalog';
CREATE USER IF NOT EXISTS 'migrate_trade'@'%' IDENTIFIED BY 'demo-migrate-trade';
CREATE USER IF NOT EXISTS 'migrate_payment'@'%' IDENTIFIED BY 'demo-migrate-payment';
GRANT ALL PRIVILEGES ON dingdong_user.* TO 'migrate_user'@'%';
GRANT ALL PRIVILEGES ON dingdong_catalog.* TO 'migrate_catalog'@'%';
GRANT ALL PRIVILEGES ON dingdong_trade.* TO 'migrate_trade'@'%';
GRANT ALL PRIVILEGES ON dingdong_payment.* TO 'migrate_payment'@'%';
