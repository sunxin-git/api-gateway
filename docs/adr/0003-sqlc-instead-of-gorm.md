# ADR-0003: 数据访问层采用 sqlc，替代 GORM

- **状态：** Accepted
- **日期：** 2026-05-25
- **决策人：** sunxin
- **相关文档：** `docs/multimedia-gateway-design.md` v1.3+，ADR-0002
- **取代：** v1.2.4 设计文档中所有 GORM 风格的代码示例

## 背景

v1.2.4 文档的代码示例统一用 GORM v2 风格：

```go
return s.db.Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(entry).Error; err != nil {
        return err
    }
    return s.outbox.PublishInTx(tx, event, payload)
})
```

这是沿用 new-api 的选择。但本项目依据 ADR-0001 是 reimplement，可以重新审视：

**重新审视：v1.2.4 设计的核心路径都在写 SQL**

v1.2.4 设计文档里反复强调「SQL 即真相」（账本不变量、状态机 CAS、outbox claim/lease 都靠 SQL 保证）：

- 账本 CAS：`UPDATE business_account_balance SET available = available - :amount WHERE business_account_id = :id AND available >= :amount AND version = :v`
- Outbox claim：`SELECT ... FOR UPDATE SKIP LOCKED`
- 任务状态机 CAS：`UPDATE tasks SET status = :next WHERE id = :id AND status = :current`
- 不变量校验：`available + reserved + used_total = recharge_total`

**GORM 在这种场景下是 ORM 的"弱项"：**

- ORM 的强项是「简单 CRUD + 关联映射」，对账本财务系统的复杂 CAS 路径帮助有限
- 复杂查询要绕回 `db.Raw()` 写裸 SQL—— 等于"用了 GORM 但不用 GORM"
- GORM 的 reflection 性能负担在多媒体高频路径下值得避免
- GORM 的 schema 自动迁移在 PG 上有边界（v1.2.4 已强约束「migration 走 GORM 抽象 + 复杂场景手写」）

**sqlc 哲学：SQL 即源头**

sqlc 从 .sql 文件生成 Go 代码：

```sql
-- name: ReserveQuota :execrows
UPDATE business_account_balance
SET available = available - $2,
    reserved = reserved + $2,
    version = version + 1
WHERE business_account_id = $1 AND available >= $2;
```

生成出：

```go
func (q *Queries) ReserveQuota(ctx context.Context, businessAccountID string, amount int64) (int64, error) {
    result, err := q.db.ExecContext(ctx, reserveQuota, businessAccountID, amount)
    if err != nil { return 0, err }
    return result.RowsAffected()
}
```

返回类型由 SQL 推导，改 SQL 后**编译时**就发现 caller 不兼容。

## 决策

**采用 sqlc + database/sql 作为唯一数据访问层。**

1. **migrations 单独管理**：用 `golang-migrate` 或 `goose` 维护 schema migration（与 sqlc 配合，schema 变更 → sqlc generate → 代码更新）
2. **所有 query 写在 `sql/queries/*.sql`**：按子系统分组（`ledger.sql` / `routing.sql` / `outbox.sql` / `admin.sql` / `relay.sql`）
3. **sqlc.yaml 单 engine**：`engine: "postgresql"`（基于 ADR-0002）
4. **事务边界**：用 `database/sql` 标准 `BeginTx() / Commit() / Rollback()`；sqlc 生成的 `Queries` 提供 `WithTx(tx)` 方法
5. **复杂查询不能用 sqlc 表达时**（极少见）：直接写 `db.QueryRowContext` + 手工扫描，不引入第三方 ORM

**v1.2.4 文档代码示例改写**：所有 `tx *gorm.DB` 改写为 `database/sql` 风格的事务对象，所有 `db.Create / Find / Where / Updates` 改写为 sqlc query 调用。

## 后果

### 变得更容易

- ✅ **SQL 是单一真相源**：业务逻辑 = SQL，PR review 只需看 .sql 文件
- ✅ **类型安全升级**：改 SQL 后所有 caller 编译时立刻报错；不会有 GORM `db.Find(&users)` 类型推断错误的隐藏 bug
- ✅ **性能更好**：零反射，所有 SQL 在生成时就绑定 prepared statement
- ✅ **可读性强**：维护时直接看 .sql 文件，比看 GORM 表达式更直接
- ✅ **与 ADR-0002 PG 单选完美配合**：sqlc 对 PG 支持最完善，PG 高级特性（JSONB、Window、CTE）都能用
- ✅ **DBA review 友好**：DBA 直接看 .sql 文件做 EXPLAIN，不需要逆向推导 GORM 表达式

### 变得更难

- ⚠️ **学习曲线**：团队第一次用 sqlc 要熟悉工具链（写 SQL → `sqlc generate` → 用生成代码），约 1-2 天上手
- ⚠️ **schema 自动迁移没了**：不能像 GORM `AutoMigrate()` 一行搞定，要用 golang-migrate 写 migration 文件。但这是正向（生产严肃项目都该手工管 migration）
- ⚠️ **某些动态查询要绕路**：sqlc 对完全动态的 WHERE 条件支持有限；少数场景要拼裸 SQL（但这本来 GORM 也做不到优雅）

### 备选方案为什么被拒绝

| 备选 | 拒绝理由 |
|---|---|
| **GORM v2** | 你的核心路径都是 CAS / 状态机 SQL，GORM 帮不上忙；reflection 负担；schema 自动迁移在生产不可信 |
| **ent** (Facebook) | 代码生成 + 强类型，但 graph 模型与你的领域（账本流水）不匹配；学习曲线最陡 |
| **sqlx** | 手写 SQL + Scan 辅助，比 sqlc 灵活但没有类型生成；30+ queries 后会想念 sqlc 的编译期检查 |
| **bun** | 比 GORM 快但生态小、文档少；切换收益不足以抵消风险 |
| **纯 database/sql** | 没有 query 文件管理、没有类型生成；50+ queries 时维护痛苦 |

## 实施清单（落在 v1.3 文档与代码骨架中）

- [ ] `sql/sqlc.yaml` 配置 engine PG + queries 目录 + 生成路径
- [ ] `sql/queries/{ledger,outbox,routing,admin,relay,task}.sql` 分子系统组织
- [ ] `migrations/` 目录用 `golang-migrate` 风格 `0001_init.up.sql` / `0001_init.down.sql`
- [ ] `Makefile` 加 `make sqlc` / `make migrate-up` / `make migrate-down` 命令
- [ ] CI 加 `sqlc generate` 后 `git diff` 必须为空（防止"忘了 generate"）
- [ ] v1.2.4 文档所有 `tx *gorm.DB` / `db.Create(entry)` 等示例改写

## 验证

- 启动 `sqlc generate` 不报错
- 生成的 Go 代码全部通过 `go vet`
- 所有 query 文件能用 `EXPLAIN` 在 PG 上跑通
- 核心 query（`ReserveQuota` / `ClaimOutboxEvent` / `CompareAndSwapTaskStatus`）经过并发测试无超卖、无重复消费
