-- usage_logs 未使用索引瘦身：删除两个 0 扫描索引，减少插入写放大。
--
-- 生产实测（pg_stat_user_indexes）：两者 idx_scan = 0。
-- 代码核查：
--   - upstream_model 在 usage_logs 上无任何 WHERE/ORDER/GROUP 谓词；
--   - model 前导查询（ListByModelAndTimeRange）无生产调用方；聚合查询均以
--     account_id/api_key_id + created_at 前导（走 idx_usage_logs_account_created_at）。
-- 保留 (created_at, requested_model, upstream_model)（migration 078，有扫描）。
DROP INDEX CONCURRENTLY IF EXISTS idx_usage_logs_model_created_at;
DROP INDEX CONCURRENTLY IF EXISTS idx_usage_logs_created_model_upstream_model;
