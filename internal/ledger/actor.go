// Package ledger 定义账本服务的接口、Actor 上下文、事件类型与 sentinel error。
//
// 实现在 postgres.go。事件发布抽象在 outbox.go（OutboxPublisher 接口）。
//
// 本文件：Actor —— 操作者上下文，所有写方法的必传参数。
package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// ActorType 操作来源类型。
//
// 与 DB 层 actor_type enum 保持值一致（admin_token / cli / system / task）。
// service 层入参用本文件的常量；写 ledger entry 时由 internal/db 层负责字符串编码。
type ActorType string

const (
	// ActorTypeAdminToken 来自 Admin Token 认证的 HTTP 路径（D-min HTTP handler）。
	ActorTypeAdminToken ActorType = "admin_token"
	// ActorTypeCLI 来自 admin-cli 子命令。P0 阶段所有 admin-cli 写死 actor_id="bootstrap"。
	ActorTypeCLI ActorType = "cli"
	// ActorTypeSystem 来自系统组件（reconciler / rebuild / migration bootstrap）。
	ActorTypeSystem ActorType = "system"
	// ActorTypeTask 来自异步任务路径（工作流 F task service）。
	ActorTypeTask ActorType = "task"
)

// Actor 操作者上下文。
//
// 每个写操作（CreateAccount / Recharge / Reserve / Commit / Release / Refund / Freeze / Unfreeze /
// RebuildBalance）的入参都必须显式带 Actor，用于结构化审计（写入 ledger.actor_type + ledger.actor_id）。
//
// P0 安全约束：admin-cli 不接受 --created-by flag，写死 Actor{Type: ActorTypeCLI, ID: "bootstrap"}；
// HTTP 路径（D-min 落地）从 Admin Token 解析 token_id 作 actor_id。
type Actor struct {
	// Type 来源类型，必须是 ActorType* 常量之一。
	Type ActorType
	// ID 操作者标识：admin_token → token_id 字符串；cli → 用户名或 "bootstrap"；
	// system → 组件名（"reconciler" / "rebuild"）；task → task_id。
	ID string
}

// Validate 校验 Actor 字段合法性。
//
// 返回的 error **不**包装 sentinel（计划 §三 砍掉 ErrInvalidActor）；
// 调用方应直接用 fmt.Errorf("invalid actor: %w", err) 上抛。
func (a Actor) Validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return errors.New("Actor.ID 不能为空")
	}
	switch a.Type {
	case ActorTypeAdminToken, ActorTypeCLI, ActorTypeSystem, ActorTypeTask:
		return nil
	case "":
		return errors.New("Actor.Type 不能为空")
	default:
		return fmt.Errorf("Actor.Type 非法值 %q，仅支持 admin_token|cli|system|task", a.Type)
	}
}

// String 返回 "type:id" 形式，便于日志输出。
func (a Actor) String() string {
	return fmt.Sprintf("%s:%s", a.Type, a.ID)
}

// ActorSystem 构造 actor_type=system 的 Actor，actor_id 写死为传入子组件名（P0 约定）。
//
// 典型用法：
//   - reconciler drift 检测命中冻结：ActorSystem("reconciler")
//   - rebuild 三阶段：ActorSystem("rebuild")
//   - migration bootstrap：ActorSystem("migration")
//
// component 为空时返回 Actor{}（调用方 Validate 会失败），不在此处 panic：
// 显式优于隐式（CLAUDE.md §四 #6），错误由 Actor.Validate 集中检出。
func ActorSystem(component string) Actor {
	return Actor{Type: ActorTypeSystem, ID: component}
}
