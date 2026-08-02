-- Outbox 清理支撑与大表 autovacuum 调参。
--
-- 背景：
--   billing_attempt_outbox / scheduler_outbox 的成功行从不删除（succeeded 行保留
--   用于对账、已消费行仅推进 Redis watermark），表持续膨胀（生产实测分别达
--   90 万行 / 512 万行），claim 与健康统计在全表上扫描。
--   清理由 OutboxCleanupService 按保留期执行；本迁移只负责把清理与回收
--   所依赖的参数准备好：
--     1. autovacuum 调参：降低触发阈值，避免大表死元组长期堆积
--        （此前 scheduler_outbox 14 天未触发 autovacuum）。
--     2. billing_attempt_outbox 的 retention 索引（updated_at, id）已在
--        migration 171 中创建，清理直接复用。

-- 高频写入/删除表：死元组应快速回收，scale_factor 0.05 + 5 万行阈值
ALTER TABLE usage_logs SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_vacuum_threshold = 50000);
ALTER TABLE ops_error_logs SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_vacuum_threshold = 50000);
ALTER TABLE usage_billing_dedup SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_vacuum_threshold = 50000);

-- outbox 表：清理产生大量删除，按状态流转频繁更新，阈值略低
ALTER TABLE billing_attempt_outbox SET (autovacuum_vacuum_scale_factor = 0.1, autovacuum_vacuum_threshold = 20000);
ALTER TABLE scheduler_outbox SET (autovacuum_vacuum_scale_factor = 0.1, autovacuum_vacuum_threshold = 20000);

-- accounts：token 刷新/错误状态频繁更新
ALTER TABLE accounts SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_vacuum_threshold = 10000);
