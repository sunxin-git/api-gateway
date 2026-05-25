# ADR-0001: 采用 reimplement-only 策略，不基于 new-api 二次开发

- **状态：** Accepted
- **日期：** 2026-05-25
- **决策人：** sunxin
- **相关文档：** `docs/multimedia-gateway-design.md` v1.3+
- **取代：** v1.2.4 第 13 条基线决策「基于 new-api 二次开发」

## 背景

v1.2.4 设计文档（含 7 轮 Codex 评审）一直假设我们在 [QuantumNous/new-api](https://github.com/QuantumNous/new-api) 基础上做二次开发——把 new-api 整体克隆进 `third-party/new-api/`，然后修改、扩展。

但 new-api 的许可证是 **GNU Affero General Public License v3.0 (AGPLv3)**。AGPLv3 的核心约束包括：

1. **Copyleft 传染**：任何 derivative work 必须继承 AGPLv3
2. **Affero clause**（比 GPL 更严）：通过网络对外提供服务的衍生品，**网络用户有权索要完整源码**
3. **范围广义**：fork、import、照搬代码片段（即使重命名变量）都构成 derivative work

本项目的商业模式是「**多媒体创作平台的基础设施层（API 模型网关 + 计费）**」，对接 toB / toC 客户。这与 AGPLv3 的开源义务存在结构性冲突：

| 商业诉求 | AGPLv3 影响 |
|---|---|
| 闭源企业账户/计费/销售层加价规则 | ❌ 不允许——客户可索要源码 |
| 闭源凭据加密 / KEK 管理实现 | ❌ 不允许 |
| 商业合同价 / VIP 折扣实现 | ❌ 不允许 |
| 出售商业许可 | ❌ 不可行——AGPLv3 不允许重新许可 |
| 双重授权 | ❌ 不可行——你不是原作者，无权重许可 new-api 代码 |

## 决策

**采用 reimplement-only 策略：**

1. **不在 new-api 基础上 fork / 修改 / 复制**——你的代码仓库 (`api-gateway`) 不包含任何 new-api 代码
2. **架构思路参考是合法的**——版权法的 idea/expression dichotomy 原则保护抽象思想（架构模式、分层方法、协议对接），仅保护具体表达（代码、注释、字面文字）
3. **代码 100% 自写**——读完 new-api 实现思路后**独立**写自己的代码，不照搬片段（即使重命名）
4. **基于 v1.2.4 设计文档（你的原创）和 provider 官方协议**实现，不基于 new-api 的具体代码
5. **物理保留 `third-party/new-api/` 作只读参考**——`.gitignore` 已包含 `third-party/`，绝不 commit 进仓库（已验证 `git check-ignore` 生效）

## 操作纪律

- ✅ 阅读 new-api 代码（无论本地或 GitHub）——零风险
- ✅ 看完 new-api 后用自己的话写——零风险
- ✅ 看 OpenAI / Anthropic / 火山引擎等 provider 官方文档对接 API——零风险
- ❌ 复制 new-api 代码片段（即使重命名变量 / 改函数签名）
- ❌ 复制 new-api 的 SQL 语句、注释、错误消息、prompt 模板
- ❌ `go.mod` import new-api 任何包（Go 编译器物理强制不会犯错）
- ❌ commit `third-party/new-api/*` 进 git（`.gitignore` 已强制）

**PR review 卡控**：reviewer 要警惕"代码片段看起来过于像 new-api"，要求重写。

## 后果

### 变得更容易

- ✅ **商业模式无障碍**：闭源企业账户/计费/凭据/销售层规则不受 AGPLv3 约束
- ✅ **不被 license 绑架**：未来 new-api 改 license 或者商业纠纷不影响本项目
- ✅ **设计自由度高**：不需要继承 new-api 的历史包袱（如 30+ controller 平铺式布局、AGPLv3 的 NOTICE 条款）

### 变得更难

- ⚠️ **工作量上升**：不能 fork 一个现成的 90% 功能，要从设计文档自己重建。但通过 reimplement 范围收缩（仅多媒体 + LLM 简化版，不做全功能对标）和 AI Agent 并行实现，工期仍在 18-22 天可控范围
- ⚠️ **architecture 风险自己扛**：不能像 fork 那样有 5 年的生产验证；要靠 v1.2.4 设计文档 + 7 轮评审 + 集成测试 + 灰度发布兜底
- ⚠️ **运营技能不能从 new-api 文档直接复用**：要自己写运营 SOP

### 备选方案为什么被拒绝

| 备选 | 拒绝理由 |
|---|---|
| 继续 AGPLv3 + 开源 | 与商业模式根本冲突 |
| Fork + 改 license（如 MIT） | AGPLv3 不允许重新许可；除非联系原作者拿商业授权（罕见且贵） |
| Fork + 仅自用（不对外服务） | 与「toB/toC 平台基础设施」定位不符 |
| 用 Docker 镜像 sidecar（避免代码 derivative） | AGPLv3 对 Docker 镜像的开源义务有歧义，法律风险仍存在；且性能/集成度大幅下降 |

## 验证

- `.gitignore` 第 47 行确认 `third-party/`：`git check-ignore -v third-party/new-api/main.go` 输出确认
- 仓库公开后 license 标识为「商业项目，闭源」（具体待商业模式确定后写 LICENSE）
- 代码 review 全程注意 derivative 风险
