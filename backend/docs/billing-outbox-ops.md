# Billing Outbox 运维手册

作用域：`billing_attempt_outbox` 表的消费（apply + finalization）、健康观测、告警与恢复。
代码入口：

- `backend/internal/service/billing_outbox_worker.go` — worker 消费循环、健康（`Health`）、
  终端增长告警（`sampleTerminalGrowth` / `evaluateTerminalGrowth`）。
- `backend/internal/repository/billing_outbox_repo.go` — SQL（Claim / Stats / CleanupTerminal，
  含终端恢复 SQL 注释，见该文件 `CleanupTerminal` 上方注释块）。

## 队列模型与 FIFO 语义

- 单表、id 单调递增（bigserial）。Claim 按 `(status, available_at, id)` 排序
  （`ORDER BY available_at, id`）——积压时新入队记录按 id 排在队尾，**严格 FIFO 消化**；
  失败重试按 `available_at` 退避后回到队列，不插队。
- 消化速率 = apply 吞吐：批量事务 + 用户分片级串行（`BillingApplyUserShardCount` 分片、
  advisory 锁跨实例生效），单实例约 3.1K/s，多实例线性叠加（约 10K/s 摄入量级）。
- **延迟估算：delay ≈ 积压数 / 消化速率**。例如单实例积压 100 万行、3K/s 消化时，
  新记录从入队到扣费完成约 5.5 分钟。积压期间**扣费滞后数小时属预期行为**（不是故障）：
  背压感知连续拉（`BackloggedRounds` 观测）会尽快消化，但吞吐上限由批量与分片串行决定。
- **监控以 `oldest_lag`（队首等待时长）为准**，不要用"直觉延迟"判断异常：只要
  `oldest_lag` 稳定在预期范围内（≈ 积压数/消化速率），滞后多久都属正常。

## 观测指标（GET /api/v1/admin/ops/billing-outbox/health，monitoring-gated）

| 字段 | 语义 |
| --- | --- |
| `pending` / `processing` / `terminal` | 队列三态行数（`terminal` 见下节） |
| `oldest_lag` | 队首记录等待时长（**主监控指标**） |
| `backlogged_rounds` | 积压轮累计（Claim 拉满且实际消化>0 的轮次，单调不减，**取相邻采样 delta**） |
| `round_timeouts` | finalization 整轮 deadline 触发的累计轮次（单调不减，取 delta；per-record 超时不计入） |
| `circuit_open` / `circuit_error` / `circuit_opened_at` | apply 熔断状态（连续永久错误 ≥ 5 打开） |
| `permanent_failures` | 熔断累计永久错误数（单调不减，**取 delta**） |
| `terminal_alert` | terminal 增长告警信息（空 = 正常；见告警节） |
| `last_error` / `stats_error` | 最近一次处理错误 / 统计查询错误 |

## Terminal 行与恢复

`terminal` 行 = 已耗尽重试的永久失败：

- PG 42xxx/22xxx（部署可修复）在 `attempts >= billingOutboxMaxAttempts`（10）后终态化，
  `last_error` 带 `[SQLSTATE xxxxx]` 前缀；
- 确定性数据毒药（infra 4xx / 哨兵错误，如实体 not-found）立即终态化，不受重试次数约束。

终态行不参与 Claim/重放，只在对账窗口内保留（`outbox_cleanup.terminal_retention_days`，
默认 30 天，对齐月度计费对账窗口），超期由 `CleanupTerminal` 批量删除。

### 恢复 SQL（与 `billing_outbox_repo.go` CleanupTerminal 注释同源）

误判 terminal（或需要重放某条 terminal 记录）时，可在保留期内手动重置回 pending 重试：

```sql
UPDATE billing_attempt_outbox
SET status = 'pending', available_at = NOW(), lease_until = NULL,
    leased_by = NULL, attempts = 0
WHERE status = 'terminal';
```

- **恢复窗口 = 终态保留期**（默认 30 天）：保留期外行已被 `CleanupTerminal` 删除，
  无恢复可能。保留期不得短于人工恢复窗口——调短保留期前先评估对账/恢复影响。
- 该重放只适用于 **apply 路径** terminal 的行：重放重新 apply，去重幂等后转入
  finalization，不会重复计费。
- **finalization 阶段** terminal 的行（仅确定性毒药或 PG 错误达 maxAttempts 时产生）重置
  为 pending 会命中已存在的去重键直接 Ack，**不会重放**已扣费记录的后置效应（缓存失效/
  通知），需人工处理。

## Terminal 增长告警（worker 内建，slog.Error + health.terminal_alert）

worker 的 run 循环按 ≤60s 节奏（默认 60s，与轮次节奏解耦）采样 `repo.Stats`
（与 Health 共用同一查询，不新增查询形状；DB 额外负载 ≤1 次/分钟），评估两个条件：

| 条件 | 阈值 | 推演 |
| --- | --- | --- |
| terminal 绝对量 | > 500,000 行 | 健康稳态终态化预算 ≤ 0.1 行/s（10K/s 摄入的 0.001%）；30 天保留窗口稳态堆积 ≈ 26 万行，阈值 ≈ 2× 该上限——健康累计不误报，超阈说明终态化率长期偏离预算（纯清理停摆的检测延迟见下方边界说明） |
| terminal 增长速率 | > 5 行/s（60s 采样 delta ≥ 300 行） | 健康预算的 50×（摄入的 0.05%）：系统性误分类（如 schema 破坏导致全员 42P01 在 maxAttempts=10 后终态化）的信号；5 行/s 持续约 28 小时才推过绝对量阈，速率告警先行、绝对量兜底既有大堆积 |

**清理停摆的检测边界**：清理是独立服务（`outbox_cleanup_service.go`，独立 run 循环与
Redis 单例 leader 锁，失败有自己的 `slog.Warn` 日志通道）——本告警的速率路径在清理
停摆时不触发（停摆期间无新增 terminal 行、速率归零），绝对量路径只在堆积从健康稳态
（≈26 万）爬过阈值（50 万）后触发，健康 churn 下约 1 个月延迟。清理停摆应以清理
服务的失败日志与健康为准交叉核验，本告警只做长延迟兜底。

告警行为：

- 触发沿（条件从正常越界）记一条 `slog.Error`（含 terminal 行数、阈值、速率）并置位
  `health.terminal_alert`；**同一条件在 30 分钟冷却窗口内至多记 1 条**（重新告警的
  最小间隔 = 冷却时长，与中间状态无关），持续条件冷却后重新确认。
- 条件回落（计数低于阈值且速率归零）自动清除 `terminal_alert`；**冷却窗口内回落后
  再次越界（阈值附近振荡，如 499K↔501K）不重新告警、也不重置窗口**——日志频率硬性
  ≤1 条/30 分钟。清理删除产生负 delta，不会误报。
- 采样路径的 `repo.Stats` 失败（告警传感器故障，与 `Health` 的 `stats_error` 同一查询）
  记一条 `slog.Warn`——Warn 由独立冷却（默认 60s）硬性限频：持续失败时 ≤1 条/分钟，
  且冷却窗口内不重试查询（不随积压热循环 ~100 轮/秒刷屏、也不放大故障 DB 负载）；
  失败采样不更新基线，恢复后距上次成功采样超过 2 个采样节奏（默认 120s）的首个成功
  采样按首次采样重建基线——不按跨失败窗口的陈旧平均评估速率（不误报），绝对量超阈
  仍告警，下一采样从新基线起算速率。
- 首次采样（含 worker 重启后）只建立基线，但绝对量超阈立即告警——重启后遗留的大堆积
  同样会被看到。
- 恢复指引：查看 terminal 行 `last_error` 的 `[SQLSTATE]` 前缀定位根因 → 修复根因 →
  按上节恢复 SQL 重放 → 保留期清理任务兜底收敛。

## 对账指引

- `usage_billing_dedup`（request_id + fingerprint 去重键）是**独立兜底**：无论 outbox 行
  如何重放、重复入队（含 enqueue 幂等检查因清理删除而重新插入新行），apply 只发生一次，
  不会重复计费。
- 对账以 dedup 结果为准：outbox 行只是计费动作的载体，`succeeded`/`terminal` 分布异常
  应回查 dedup 表确认实际扣费，再决定是否按上节 SQL 恢复。
