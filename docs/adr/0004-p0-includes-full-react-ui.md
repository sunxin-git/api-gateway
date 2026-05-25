# ADR-0004: P0 阶段引入完整 React UI

- **状态：** Accepted
- **日期：** 2026-05-25
- **决策人：** sunxin
- **相关文档：** `docs/multimedia-gateway-design.md` v1.3+
- **取代：** v1.2.4 文档「P0 不做完整 Web UI，纯 RESTful API + CLI」的范围决策

## 背景

v1.2.4 文档（含 7 轮 Codex 评审）明确说 P0 不做完整 Web UI——理由是收缩 P0 范围、3-4 周可控工期、UI 推到 P1。我曾基于此推荐 "纯 API + admin-cli 二进制" 方案。

但实际讨论中用户明确要求：**"P0 就上完整 React UI"**。

重新评估这个决策的实际影响：

**支持 "P0 包含 UI" 的理由：**

1. **商业平台对接体验**：种子客户的运营人员上线就需要可视化界面查看流水、配置 channel、改路由规则；纯 CLI 对非工程运营人员门槛高
2. **AI Agent 并行抵消工期**：传统人月估算 UI 加 2 周，但 Agent 并行下"前端骨架 + 业务页面"可以与后端 B-min/C-min/D-min 并行推进，实际增加工期不到 1 周
3. **API 设计被 UI 反向 review**：UI 联调能暴露 API 设计的不友好之处（如缺字段、命名歧义、状态枚举不全），P0 阶段 catch 比 P1 改成本低
4. **种子客户验收门槛**：1-2 个种子客户灰度（见 v1.3 P0 验收标准）实际上隐含「运营能用」要求，UI 是必须项
5. **代码 ownership 一次到位**：reimplement 路线（ADR-0001）下，先做"纯 API + CLI"等于"打两次地基"——基础数据访问 + 服务路由要写两次（CLI 一次、UI 一次），最终 UI 还是要做

**反对 "P0 包含 UI" 的理由（已被推翻）：**

| 反对理由 | 推翻 |
|---|---|
| 工期翻倍 | AI Agent 并行抵消大部分，仅多 3-5 天 |
| 注意力分散 | Agent 隔离工作流，人的 review 时间可以分批投入 |
| UI 风险大 | shadcn/ui + Vite + TanStack 都是成熟栈，无技术风险 |

## 决策

**P0 范围从 4 工作流扩展为 4 工作流 + 完整 React UI（6 个核心页面）+ 火山 seedance 2.0 provider 适配。**

P0 不可省略的 UI 页面：

1. **业务账户管理**：创建 business_account / 充值 / 余额查询 / ledger 流水 / suspend 与 resume
2. **Channel 配置**：列表 + 创建 + 编辑（含 5 项 ChannelCredentials 加密字段 + `restricted_business_accounts` + `channel_purpose`）；列表全程脱敏显示
3. **Channel Routing Rule**：列表 + 编辑（条件表达式 + `fallback_policy` + 试算面板）
4. **Admin Token 管理**：创建（含 scope / IP allowlist / 阀门）/ 轮换 / 撤销 / 审计日志查询
5. **Outbox 与 Webhook**：事件查询 + 投递历史 + 失败重放 + 订阅 URL 配置
6. **Task 流水查询**：debug 用，按 business_account / status / time range 检索

**仍保留 `cmd/admin-cli/` 二进制**——用于以下不适合从 UI 操作的敏感场景：

- 紧急冻结（怀疑数据安全时 UI 都不应可用）
- break-glass 双 Root 审批（两人在两台机器各自执行 CLI，可审计性强）
- 一次性数据迁移 / 修复
- 启动时校验工具（验证 DB 连接 / KEK 加载 / migration 状态）

## 后果

### 变得更容易

- ✅ **商业平台第一印象好**：种子客户见到 UI 而非"我给你一份 curl 命令清单"
- ✅ **运营 SOP 起步即标准化**：早期 < 10 客户时 UI 已经够用，避免 P1 重写运营流程
- ✅ **API 设计早期被 review**：UI 联调暴露 API 缺陷比 P1 客户接入后才发现成本低
- ✅ **避免双重投资**：P0 做 CLI + API 后再 P1 加 UI = 三次接入；P0 直接做 UI + API + CLI（CLI 缩减为运维敏感操作）= 一次到位
- ✅ **AI Agent 充分发挥**：前端是 shadcn/ui copy-paste 风格，AI Agent 写代码效率远高于手工

### 变得更难

- ⚠️ **P0 工期从 v1.2.4 原 3-4 周扩到 18-22 天**：但 Agent 并行下不夸张
- ⚠️ **联调阶段增加**：UI ↔ API 联调多一道环节，要预留 milestone review 时间
- ⚠️ **前端工具链引入**：pnpm / Vite / shadcn 等 Node 工具进入仓库，对运维有新增学习成本（但 monorepo + Go embed 模式下，最终交付仍是单二进制，部署不复杂）

### 备选方案为什么被拒绝

| 备选 | 拒绝理由 |
|---|---|
| **纯 API + admin-cli**（v1.2.4 原推荐 / 我曾推荐） | 用户明确否决；种子客户运营体验差；P1 还要重做 UI |
| **纯 API + 极简静态页面**（vanilla / htmx） | 商业平台对 UI 体验有更高要求；htmx 在复杂表单（routing rule 试算面板、ChannelCredentials 加密字段）下不够灵活 |
| **P0 只做 3 个核心页面，其余 P1 再加** | 部分页面缺失会让运营 SOP 出现盲区；6 个页面是真正"够用"的最小集 |

## 实施

详见 v1.3 文档的 "工作流 UI" 与第十六章 P0 落地清单（新增 Agent 11 / 12 / 13 负责前端）。

## 验证

- 6 个核心页面在 P0 ready 时全部上线
- shadcn/ui 组件库与后端 RESTful API 全程同源调用（同 origin，无 CORS）
- Session Cookie 鉴权 + CSRF token 在前端架构里就位
- 国际化（zh + en）覆盖所有 UI 文案
