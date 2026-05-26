// main_test.go 是 api-gateway 进程入口的单元测试（计划 Unit 10）。
//
// 范围限制：
//   - 不启动整个 run()：那会拉 HTTP 端口 + reconciler goroutine + 信号 handler，
//     易与并行测试相互干扰；
//   - 仅测纯函数 newPGXPool 的 fail-fast 语义（DB 不可达 → error，不 Ping 成功 → close + error）；
//   - 真实的「整进程拉起 / SIGTERM 优雅停机」由本地手工验证（make run）+ docs/dev-setup.md 覆盖。
package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewPGXPool_EmptyDSN 验证空 DSN 立即 fail-fast，不去开 socket。
func TestNewPGXPool_EmptyDSN(t *testing.T) {
	pool, err := newPGXPool("")
	require.Nil(t, pool)
	require.Error(t, err)
	require.Contains(t, err.Error(), "PGDSN 为空",
		"空 DSN 应返回中文错误（CLAUDE.md §一）")
}

// TestNewPGXPool_InvalidDSN 验证非法 DSN 在 ParseConfig 阶段就拒绝。
func TestNewPGXPool_InvalidDSN(t *testing.T) {
	pool, err := newPGXPool("not-a-valid-postgres-dsn://?@#$%")
	require.Nil(t, pool)
	require.Error(t, err)
	// pgxpool.ParseConfig 错误信息为英文（外部库），用包装的中文前缀做断言。
	require.Contains(t, err.Error(), "pgxpool.ParseConfig",
		"非法 DSN 错误应被包装且含 pgxpool.ParseConfig 前缀")
}

// TestNewPGXPool_DBUnreachable 验证 DB 不可达时 Ping 失败 + pool 已 Close。
//
// 用 127.0.0.1:1（一般不会有进程监听）模拟不可达；
// 5s ping 超时内必然失败，函数返回中文错误。
func TestNewPGXPool_DBUnreachable(t *testing.T) {
	if testing.Short() {
		t.Skip("skip ping test in -short mode")
	}
	// 用一个语法合法但目标不可达的 DSN（127.0.0.1:1 几乎不会有 PG 监听）。
	dsn := "postgres://gateway:gateway_dev@127.0.0.1:1/gateway?sslmode=disable&connect_timeout=2"
	pool, err := newPGXPool(dsn)
	require.Nil(t, pool, "DB 不可达时不应返回非 nil pool（防 leak）")
	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "pool.Ping") ||
			strings.Contains(err.Error(), "pgxpool.NewWithConfig"),
		"DB 不可达错误应来自 Ping 或 NewWithConfig 阶段；实际: %s", err.Error())
}
