-- M1 在所属库内串行检查活动额度；M2 才增加 Redis Lua 准入与恢复栅栏。
ALTER TABLE activity ADD COLUMN used_quota INT NOT NULL DEFAULT 0,
    ADD CONSTRAINT chk_activity_quota CHECK (used_quota >= 0 AND used_quota <= quota);
