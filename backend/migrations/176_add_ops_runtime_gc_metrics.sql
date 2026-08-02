-- ops_system_metrics 增加 Go runtime 内存 / GC 观测字段（进程内口径）。
--
-- 用途：定位内存占用去向与 GC 压力（生产 RSS 已贴近 GOMEMLIMIT，GC 持续高压）：
--   - heap_alloc_mb 接近 GOMEMLIMIT → soft limit 过紧，需调参或减分配；
--   - gc_cpu_fraction 高 → GC 本身是 CPU 主因；
--   - alloc_bytes_per_sec 高 → 需要减少分配（缓冲复用 / 惰性解析）。
ALTER TABLE ops_system_metrics
    ADD COLUMN IF NOT EXISTS heap_alloc_mb DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS heap_sys_mb DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS gc_num_cycles BIGINT,
    ADD COLUMN IF NOT EXISTS gc_total_pause_ms DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS gc_cpu_fraction DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS alloc_bytes_per_sec DOUBLE PRECISION;
