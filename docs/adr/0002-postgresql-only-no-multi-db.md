# ADR-0002: 数据库统一为 PostgreSQL，放弃三库兼容

- **状态：** Accepted
- **日期：** 2026-05-25
- **决策人：** sunxin
- **相关文档：** `docs/multimedia-gateway-design.md` v1.3+，ADR-0001
- **取代：** v1.2.4 第 13 条基线决策中「SQLite / MySQL / PostgreSQL 三库兼容」约束

## 背景

v1.2.4 文档沿用 new-api 的硬约束：「所有数据库代码必须同时兼容 SQLite + MySQL ≥ 5.7.8 + PostgreSQL ≥ 9.6」（`CLAUDE.md` Rule 2）。这条约束在 new-api 项目下有合理性——它是开源项目，要让任何用户随手部署。

但本项目定位是「**商业化多媒体 AI 网关，目标客户是有专业运维的 B2B 企业**」，且依据 ADR-0001 不再受 new-api 约束。重新评估三库兼容的成本/收益：

**核心业务对数据库特性的需求：**

| 关键特性 | PostgreSQL | MySQL | SQLite |
|---|---|---|---|
| `SELECT FOR UPDATE SKIP LOCKED`（outbox 多节点扫描必需） | ✅ | ✅ 8.0+ | ❌ 要降级到 `BEGIN IMMEDIATE` 全库锁 |
| Serializable 隔离级别（账本财务必备） | ✅ 完整 | ⚠️ 较弱 | ⚠️ 单 writer |
| 行级锁 / Advisory lock | ✅ | ✅ | ❌ 全库锁 |
| JSONB 索引（Provider Cost Catalog metadata 查询） | ✅ 原生 | ⚠️ JSON 但弱 | ⚠️ JSON1 扩展 |
| 多节点部署（高可用基础） | ✅ | ✅ | ❌ 单文件 |
| sqlc 工具链支持完善度 | ✅ 最优 | ✅ 良好 | ✅ 良好 |

**三库兼容的真实代价：**

- 每个核心 SQL 写 2-3 个版本（v1.2.4 已经在 outbox claim/lease 给 SQLite 写"降级路径"）
- sqlc 生成 query 时要按 engine 分目录维护
- 集成测试矩阵 × 3 倍
- 维护中长期成本：6-12 个月后某个 SQL 在 SQLite 跑挂的概率高
- SQLite 在生产几乎没有真实落地——给"任何用户都能装"的设计妥协，被本项目用不到

**外部约束变更：**

- 用户告知**本地开发环境已有 PostgreSQL（原生安装，不用 docker）**
- 这消除了 "保留 SQLite 用于开发期" 的最后理由

## 决策

**生产 / 开发 / 测试**全部统一到 **PostgreSQL**：

1. **生产环境**：PostgreSQL ≥ 15（推荐 16+，享受 `LOGIN INHERIT` 等便利特性）
2. **开发环境**：本地原生 PostgreSQL（与生产同版本）
3. **CI / 集成测试**：GitHub Actions 起 PG service container 或 dockertest
4. **删除所有 SQLite / MySQL 兼容代码**——包括：
   - v1.2.4 文档的 SQLite `BEGIN IMMEDIATE` 双步 CAS 降级路径
   - 跨库类型适配（如 `commonGroupCol` / `commonTrueVal`）
   - 三库 boolean / decimal / json 抽象层

## 后果

### 变得更容易

- ✅ **核心 SQL 只写一份**：账本 CAS、outbox `SELECT FOR UPDATE SKIP LOCKED`、状态机转移全部用 PG 原生语法
- ✅ **sqlc 生成代码量减少 60%**（不需要为每个 engine 生成一套）
- ✅ **集成测试矩阵简化**：单 PG，所有 SQL 都按真实生产路径测
- ✅ **可以充分利用 PG 高级特性**：JSONB GIN 索引、Partial Index、CTE、Window Functions、`LISTEN/NOTIFY`（未来事件总线可选项）、Logical Replication（多区域 CDC）
- ✅ **财务正确性更强**：PG 的 Serializable + Row-level Lock 在多媒体场景下是真实需要
- ✅ **环境一致性最优**：开发 / 测试 / 生产同库，杜绝"我本地能跑生产挂了"

### 变得更难

- ⚠️ **失去"单文件部署"能力**：未来如果有"个人版"需求（如内部小工具），不能像 SQLite 那样零配置。但本项目目标是商业 B2B，不需要
- ⚠️ **本地开发依赖 PG**：新工程师入职要装 PG（不能像 SQLite 那样 `go run` 即用）。但用户已确认本地有 PG，且 PG 安装在 macOS/Linux/Windows 都成熟
- ⚠️ **未来如果某客户强烈要求 MySQL**：要单独评估改造成本（大概率拒绝该客户）

### 备选方案为什么被拒绝

| 备选 | 拒绝理由 |
|---|---|
| 沿用 v1.2.4 三库兼容 | 维护成本高且业务真实需求只有 PG；继承 new-api 历史包袱 |
| PG + MySQL 两库 | 仍要写两套 SQL；MySQL 在你的场景下没有相对 PG 的优势 |
| PG + SQLite 两库（PG 生产 + SQLite 开发） | 用户本地有 PG，没必要双轨；SQLite 在 CI 跑过的代码不能保证 PG 上行为一致 |

## 实施清单（落在 v1.3 文档更新中）

- [ ] 删除 v1.2.4 文档里的 SQLite 降级路径（outbox claim/lease 段、`BEGIN IMMEDIATE` CAS 段）
- [ ] 删除 "跨库 SQL 注意" 段
- [ ] sqlc 配置 `engine: "postgresql"` 单一
- [ ] migrations/ 目录 SQL 全用 PG 语法（`CREATE TABLE ... (id BIGSERIAL ...)` 等）
- [ ] 文档明示「商业部署仅支持 PostgreSQL ≥ 15」
- [ ] `docker-compose.yml`（如有）只起 PG service
- [ ] 启动校验：`PG version >= 15`，否则拒绝启动

## 验证

- migrations/ 全部用 PG 原生类型（`BIGSERIAL`、`JSONB`、`TIMESTAMPTZ`）
- 核心 SQL（账本 CAS、outbox SKIP LOCKED、状态机转移）能在 PG 上 EXPLAIN 优化通过
- 集成测试全程使用本地 PG，不用任何 in-memory 模拟
