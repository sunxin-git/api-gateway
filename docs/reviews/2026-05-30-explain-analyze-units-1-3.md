# EXPLAIN ANALYZE — Phase 2 异步视频中继 Units 1-3（migration 0005-0008 + task 查询）

> 满足 CLAUDE.md §六硬规则「SQL 变更必须附 EXPLAIN ANALYZE 输出」。
> 来源：ce-review（performance + data-migrations 一致发现 Unit 2 新查询缺索引 → 补 migration 0008）。
> 环境：本地原生 PostgreSQL 18.1（`127.0.0.1:5432/gateway`），schema 版本 0008。
> 方法：seed 5000 行 task（5 种 status 各 1000；UPSTREAM_SUBMITTED 行带 `upstream_submitted_at`/`upstream_task_id`），
> `ANALYZE task` + `ANALYZE account_model_concurrency` 后跑 `EXPLAIN (ANALYZE, BUFFERS)`；跑完删除 seed 数据并重新 `ANALYZE`，不污染开发库。
> 下方数字为单次真实运行原文（缓存预热后；Planning Buffers 受元数据冷热影响，仅看 Execution + 索引命中）。

## 结论

补 migration 0008 四个部分索引后，task.sql 的回调反查与 3 个 reconciler scan **全部走 Index Scan**（无 Seq Scan）；
`ClaimConcurrencySlot` 的条件 UPSERT 经主键仲裁（`pk_account_model_concurrency`）+ Conflict Filter 应用 cap。
5000 行下均亚毫秒、Buffers 全 shared hit（无磁盘读）。索引谓词与查询 WHERE 对齐，Limit 100 借索引有序提前终止——只读 6~11 个 buffer，不扫全部 1000~3000 候选行。

| 查询 | 计划 | 索引 | rows | Execution Time | Buffers |
|---|---|---|---|---|---|
| `GetTaskByUpstreamTaskID`（回调热路径） | Index Scan | `idx_task_upstream_task_id` | 1 | 0.027 ms | hit=3 |
| `ScanStuckUpstreamSubmitted`（fetch reconciler） | Index Scan + Limit | `idx_task_stuck_upstream_submitted` | 100 | 0.093 ms | hit=11 |
| `ScanSubmittedNoJob`（reconciler） | Index Scan + Limit | `idx_task_submitted_no_job` | 100 | 0.048 ms | hit=11 |
| `ScanExpirableTasks`（expire worker） | Index Scan + Limit | `idx_task_expirable` | 100 | 0.040 ms | hit=6 |
| `ClaimConcurrencySlot`（R15 占位 UPSERT） | Insert + ON CONFLICT | 仲裁 `pk_account_model_concurrency` | 1 | 0.336 ms | hit=6 |

## 原始输出（单次真实运行）

```
=== seed counts ===
 SUBMITTED           | 1000
 UPSTREAM_SUBMITTING | 1000
 UPSTREAM_SUBMITTED  | 1000
 COMPLETED           | 1000
 SETTLED             | 1000

=== Q1 GetTaskByUpstreamTaskID ===
 Index Scan using idx_task_upstream_task_id on task  (cost=0.28..8.29 rows=1 width=266)
   (actual time=0.013..0.014 rows=1.00 loops=1)
   Index Cond: (upstream_task_id = 'up-7'::text)
   Buffers: shared hit=3
 Execution Time: 0.027 ms

=== Q2 ScanStuckUpstreamSubmitted ===
 Limit  (cost=0.28..9.53 rows=100 width=266) (actual time=0.026..0.065 rows=100.00 loops=1)
   Buffers: shared hit=11
   ->  Index Scan using idx_task_stuck_upstream_submitted on task
         (cost=0.28..18.78 rows=200 width=266) (actual time=0.025..0.056 rows=100.00 loops=1)
         Index Cond: (upstream_submitted_at < now())
         Buffers: shared hit=11
 Execution Time: 0.093 ms

=== Q3 ScanSubmittedNoJob ===
 Limit  (cost=0.28..6.83 rows=100 width=266) (actual time=0.014..0.033 rows=100.00 loops=1)
   Buffers: shared hit=11
   ->  Index Scan using idx_task_submitted_no_job on task
         (cost=0.28..65.78 rows=1000 width=266) (actual time=0.013..0.029 rows=100.00 loops=1)
         Index Cond: (submitted_at < now())
         Buffers: shared hit=11
 Execution Time: 0.048 ms

=== Q4 ScanExpirableTasks ===
 Limit  (cost=0.28..6.10 rows=100 width=266) (actual time=0.011..0.027 rows=100.00 loops=1)
   Buffers: shared hit=6
   ->  Index Scan using idx_task_expirable on task
         (cost=0.28..174.78 rows=3000 width=266) (actual time=0.011..0.022 rows=100.00 loops=1)
         Index Cond: (submitted_at < now())
         Buffers: shared hit=6
 Execution Time: 0.040 ms

=== Q5 ClaimConcurrencySlot (tx rollback, 已存在行 inflight=0, cap=5) ===
 Insert on account_model_concurrency amc  (cost=0.00..0.01 rows=1 width=76)
   (actual time=0.074..0.075 rows=1.00 loops=1)
   Conflict Resolution: UPDATE
   Conflict Arbiter Indexes: pk_account_model_concurrency
   Conflict Filter: (amc.inflight < 5)
   Tuples Inserted: 0
   Conflicting Tuples: 1
   Buffers: shared hit=6
 Execution Time: 0.336 ms
```

## 备注

- **复现要点**：seed 后必须 `ANALYZE` 更新统计，否则规划器对小表可能误选 Seq Scan。`business_account` 仅 `id` 必填（status/metadata/时间戳均有默认）。
- **cap=0 守卫**（ce-review #1）：Q5 的 `SELECT ... WHERE 5 >= 1` 体现守卫——cap=0 时该 WHERE 为假 → INSERT 不产生行 → 返 0 行 = 占不到，封堵「行不存在 + cap=0」绕过路径。Q5 计划中 `Conflict Filter: (amc.inflight < 5)` 是行已存在时的 cap 检查。
- **生产大表锁表提示**：0008 的 `CREATE INDEX`（非 CONCURRENTLY）在 golang-migrate 单事务内构建，持 task 表 ShareLock。当前为新表无忧；未来大表重建须用 `migrate -x`（去事务）+ `CREATE INDEX CONCURRENTLY`（见 schema.md migration 操作纪律）。
