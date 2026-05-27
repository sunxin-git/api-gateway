// Package admintoken 提供 Admin Token 的生成、鉴权、生命周期管理。
//
// 计划：docs/plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md Unit 2
// 设计文档：docs/multimedia-gateway-design.md §9bis.6
//
// 包内职责：
//   - Token CRUD（创建 / 列表 / 吊销 / 按 hash 查活跃）
//   - 鉴权校验（hash 匹配 + revoked/expired + IP CIDR）
//   - scope 检查
//
// 不在本包：
//   - 阀门 / 限流 / 熔断（推 internal/admintoken/throttle.go，Unit 3 落地）
//   - HTTP middleware（推 internal/httpapi/middleware/admin_*.go，Unit 4 落地）
//
// 设计原则（CLAUDE.md §四 一致）：
//   - 显式优于隐式：Service 接口入参 / 返回值 DTO 显式；不暴露内部 token_hash 给上层
//   - 失败优先：所有错误返 sentinel；CIDR 空数组 = 拒绝（fail-closed）
//   - DIP：Service 是接口，PostgresService 是实现，便于测试注入 mock
package admintoken

import "errors"

// Sentinel errors —— 包对外契约。
//
// 调用方用 errors.Is 判断，**不**用类型断言；error 链可用 fmt.Errorf("...: %w", Err...) 包装。
var (
	// ErrTokenNotFound token plaintext 经 hash 后查不到匹配记录。
	// 也覆盖 token 早已 revoked 或 expired 的情况（鉴权 query 已含 WHERE revoked_at IS NULL AND ...）。
	ErrTokenNotFound = errors.New("admin token not found")

	// ErrTokenRevoked token 已 revoke（保留为细分；当前 ValidateByPlaintext 单 query 只判活跃，
	// 不会区分 NotFound vs Revoked。保留语义供未来 admin-cli token list 用）。
	ErrTokenRevoked = errors.New("admin token revoked")

	// ErrTokenExpired token expires_at 已过（同上，鉴权 query 单 query 内已过滤）。
	ErrTokenExpired = errors.New("admin token expired")

	// ErrIPNotAllowed 请求源 IP 不在 token 的 ip_allowlist CIDR 内。
	// 空 allowlist = fail-closed 拒全部（计划 D2 决策）。
	ErrIPNotAllowed = errors.New("source IP not in allowlist")

	// ErrInsufficientScope token 持有的 scopes 不含 handler 所需 scope。
	// 由 middleware 的 AdminScope("xxx") 抛出；Service.CheckScope 只返 bool。
	ErrInsufficientScope = errors.New("insufficient scope")

	// ErrInvalidParam Create 入参非法（描述空 / scopes 空 / ip_allowlist 空）。
	ErrInvalidParam = errors.New("invalid create params")
)
