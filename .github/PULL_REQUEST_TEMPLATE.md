<!-- PR 描述请用中文。参考 CLAUDE.md §六「PR / 代码评审硬规则」 -->

## 变更摘要

<!-- 用 1-3 句话说明：做了什么、为什么 -->

## 关联

- 关联 ADR：<!-- 例：ADR-0001、ADR-0003；无则填「无」 -->
- 关联工作流：<!-- 例：Phase 2 工作流 E（账本）；无则填「无」 -->
- 关联计划：<!-- 例：docs/plans/2026-05-26-001-feat-phase-1-...md -->
- 关联 Issue：<!-- gh issue 编号；无则填「无」 -->

## 自检清单（CLAUDE.md §六）

- [ ] Commit message + PR 描述使用中文
- [ ] **Reimplement 自检（ADR-0001）**：本 PR diff 中**不包含**任何复制自 `third-party/new-api/` 的代码片段（已 grep 双向核对）
- [ ] **SQL 变更**：如有 SQL 变更，附 `EXPLAIN ANALYZE` 输出在 PR 评论
- [ ] **金钱 / 状态机 / 凭据加密**：如涉及，附并发 / 边界单元测试
- [ ] **Admin API**：如涉及，附 scope 检查 + 阀门验证测试
- [ ] **新增依赖**：先开 ADR；通过后才改 `go.mod` / `package.json`
- [ ] **Schema 变更**：必须有 `migrations/NNNN_*.up.sql` + `.down.sql`；CI 已跑 up/down/up 验证
- [ ] **删除生产数据**：必须有 `--dry-run` 模式；CI 已跑 dry-run 测试
- [ ] **Panic / fatal 路径**：在下方列出触发条件
- [ ] 本地 `make test` 全绿
- [ ] 本地 `make lint` 无 issue

## Panic / Fatal 触发条件

<!-- 列出本 PR 中所有 panic / log.Fatal / os.Exit 的触发条件；无则填「无」 -->

## 测试说明

<!-- 列出新增的测试函数；说明覆盖了哪些 happy / edge / error / integration 场景 -->

## Post-Deploy Monitoring & Validation

<!-- 部署后观测项；纯文档/骨架变更可填「无生产影响」 -->

- 监控面板：
- 关键日志查询：
- 健康信号：
- 异常信号 / 触发回滚：
- 验证窗口与负责人：
