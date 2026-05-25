# ADR-0005: 前端栈采用业界主流版（shadcn/ui + Vite + pnpm + Recharts），替代 v1.2.4 原推荐

- **状态：** Accepted
- **日期：** 2026-05-25
- **决策人：** sunxin
- **相关文档：** `docs/multimedia-gateway-design.md` v1.3+，ADR-0001、ADR-0004
- **取代：** v1.2.4 「前端栈沿用 new-api 主用」的隐含决策

## 背景

v1.2.4 文档对前端栈的描述是「参考 new-api 主用前端 `web/default`」，即：

| 维度 | v1.2.4 原推荐（new-api 风格） |
|---|---|
| 框架 | React 19 + TypeScript |
| 构建 | **Rsbuild**（字节，Rspack） |
| UI 库 | **Base UI 1.4**（Radix 新分支） |
| 包管理 | **Bun** |
| 路由 | TanStack Router |
| 状态 | Zustand |
| 数据获取 | TanStack Query |
| 表格 / 虚拟 | TanStack Table + Virtual |
| 表单 | react-hook-form + zod |
| 样式 | Tailwind 4 |
| 图标 | Hugeicons + lucide-react |
| 图表 | **VChart**（字节） |

这套选择继承自 new-api 已经在生产用了 1+ 年的栈。但本项目依据 ADR-0001 是 reimplement，不需要"和 new-api 对齐"；可以独立选当前业界最主流、招聘最易、文档最广的栈。

**重新评估关键选型：**

1. **Base UI vs shadcn/ui**：
   - **Base UI** 是 Radix UI 团队的新分支（1.x），社区资料还在起步
   - **shadcn/ui** 是当前 React 生态**事实标准**（4 万 GitHub stars，Vercel/Linear/Cal.com/Resend 等使用），不是 npm 库而是 CLI 直接把组件源码 copy 到代码库
   - 招聘"会用 shadcn"的工程师远多于"会用 Base UI"
   - 完全可定制、不被库版本绑架

2. **Rsbuild vs Vite**：
   - **Rsbuild**（字节）基于 Rspack，构建快但生态相对小
   - **Vite v6** 是 React / Vue 生态事实标准，文档 / 插件 / Stack Overflow 覆盖最广
   - 商业平台稳定优先，Vite 更保守稳

3. **Bun vs pnpm**：
   - **Bun** 1.0 才 2023 年稳定，企业级生产部署生态资料相对少
   - **pnpm** 是 monorepo 标准，国内外大厂广泛用
   - Bun 速度更快但商业平台稳定优先

4. **VChart vs Recharts**：
   - **VChart**（字节）国内出品但英文生态小
   - **Recharts** 是 React 图表事实标准，英文资料多 10x

## 决策

**前端栈采用业界主流调整版：**

| 维度 | v1.3 决定 |
|---|---|
| 框架 | React 19 + TypeScript（同 v1.2.4） |
| 构建 | **Vite v6** ← 替代 Rsbuild |
| UI 库 | **shadcn/ui + Radix UI** ← 替代 Base UI |
| 包管理 | **pnpm** ← 替代 Bun |
| 路由 | TanStack Router（同） |
| 状态 | Zustand（同） |
| 数据获取 | TanStack Query（同） |
| 表格 / 虚拟 | TanStack Table + TanStack Virtual（同） |
| 表单 | react-hook-form + zod（同） |
| 样式 | Tailwind v4（同） |
| 图标 | **lucide-react** ← shadcn 默认 |
| 图表 | **Recharts** ← 替代 VChart |
| i18n | i18next + react-i18next（同），P0 仅 zh + en |

## 后果

### 变得更容易

- ✅ **招聘 / 协作友好**：shadcn / Vite / pnpm / lucide / Recharts 都是社区资料最多的选型
- ✅ **shadcn 的 copy-paste 模式**：组件代码就在你仓库里，可任意定制，不被库版本绑架；与 ADR-0001 "reimplement-only" 哲学契合
- ✅ **AI Agent 写前端效率高**：shadcn 是 React 当前最被 LLM 训练数据覆盖的库，Agent 生成代码质量高
- ✅ **lucide 是 shadcn 默认**：跟 shadcn 一套生态用最顺
- ✅ **风险低**：Vite 6 + shadcn + pnpm 都已被 React 主流项目用 2+ 年

### 变得更难

- ⚠️ **失去 Rsbuild 的极致构建速度**：但 Vite 已经够快（HMR < 200ms）
- ⚠️ **失去 Bun 的运行时速度**：但你用 Bun 做的也只是 build tooling，不是运行时
- ⚠️ **Hugeicons 风格放弃**：lucide 风格统一性强但样式选择少；可以后期补充

### 备选方案为什么被拒绝

| 备选 | 拒绝理由 |
|---|---|
| **沿用 v1.2.4 推荐**（Rsbuild + Base UI + Bun + VChart） | 与 new-api 对齐意义已消失（ADR-0001）；多个选项相对小众，招聘 / 协作不友好 |
| **混合版**（Vite + shadcn + Bun + Recharts） | Bun 在商业平台上的稳定性储备不足；如果团队成员 Node 经验更多，pnpm 更稳 |
| **Ant Design Pro / Arco Design 企业级套件** | 整体感强但定制弱、Tailwind 集成差、文件大、与 shadcn 哲学冲突 |
| **Material UI** | 设计语言较固化，定制成本高 |

## 实施清单

- [ ] `web/admin/package.json` 用 pnpm 初始化
- [ ] `pnpm create vite@latest` 起骨架
- [ ] `pnpm dlx shadcn@latest init` 初始化 shadcn
- [ ] Tailwind v4 + tailwind-merge + cva 配置
- [ ] TanStack Router 文件式路由结构
- [ ] react-hook-form + zod 表单校验示例
- [ ] i18next zh + en 双语
- [ ] Recharts 集成一个示例（ledger 流水折线图）

## 验证

- `pnpm run build` 一次过
- `pnpm run dev` HMR < 500ms
- shadcn 组件能用 `pnpm dlx shadcn@latest add <component>` 加入
- 同源调用后端 `/admin/api/*` 无 CORS
