// Package businesskey 提供业务系统对外 API Key 的生成、鉴权、生命周期管理。
//
// 计划：docs/plans/2026-05-27-004-feat-workflow-f-min-openai-compat-relay-plan.md Unit 2
// 设计文档：docs/multimedia-gateway-design.md §9
//
// 包内职责：
//   - Business API Key CRUD（创建 / 列表 / 吊销 / 按 hash 鉴权查活跃）
//   - 鉴权校验（hash 匹配 + revoked 过滤）
//   - last_used_at 异步批量更新（5min 批量 flush；不阻塞鉴权热路径）
//   - 业务侧 RPM 限速（InProcessRPM；1:1 镜像 admintoken 同名实现）
//
// 不在本包：
//   - HTTP middleware（推 internal/httpapi/middleware/business_*.go，Unit 4）
//   - relay 转发逻辑（推 internal/relay/，Unit 3/5）
//
// 与 admintoken 的关系（F-min 决策 D4）：
//   - 共享 HMAC pepper（GATEWAY_TOKEN_PEPPER），同 plaintext 在两包算出同一 hash
//   - 但分属不同表（admin token / business api key），hash 查询路径完全独立
//   - 1:1 镜像 InProcessRPM 实现而非抽象 generic（CLAUDE.md §六：稳定 > 优雅）
package businesskey

import "errors"

// Sentinel errors —— 包对外契约。
//
// 调用方用 errors.Is 判断，**不**用类型断言；error 链可用 fmt.Errorf("...: %w", Err...) 包装。
var (
	// ErrKeyNotFound key plaintext 经 hash 后查不到匹配记录，或 key 已 revoked。
	// 鉴权 query 含 WHERE revoked_at IS NULL，revoked 与 not found 在 service 层不区分。
	ErrKeyNotFound = errors.New("business api key not found")

	// ErrRPMExceeded RPM 滚动窗口超阀门；middleware 层映射为 429 rate_limit_exceeded。
	ErrRPMExceeded = errors.New("requests per minute exceeded")

	// ErrInvalidParam Create 入参非法（业务账户为空 / description 空 / RPM 非正）。
	ErrInvalidParam = errors.New("invalid create params")
)
