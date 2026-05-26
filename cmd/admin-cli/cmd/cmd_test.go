package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCmd 以一组 args 跑根命令，返回 stdout+stderr 合并输出与 err。
func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestHelp列出所有子命令(t *testing.T) {
	out, err := runCmd(t, "--help")
	require.NoError(t, err)
	for _, sub := range []string{"migrate", "token", "account", "drift-check"} {
		assert.Contains(t, out, sub, "--help 应包含子命令 %s", sub)
	}
}

func TestMigrateUp占位返回错误(t *testing.T) {
	out, err := runCmd(t, "migrate", "up")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "尚未实现") ||
		strings.Contains(out, "尚未实现"),
		"占位命令应返回中文「尚未实现」错误，实际 err=%q out=%q", err, out)
}

func TestMigrateDown占位返回错误(t *testing.T) {
	_, err := runCmd(t, "migrate", "down", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "尚未实现")
}

func TestMigrateVersion占位返回错误(t *testing.T) {
	_, err := runCmd(t, "migrate", "version")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "尚未实现")
}

func TestTokenCreate占位返回错误(t *testing.T) {
	_, err := runCmd(t, "token", "create",
		"--scope", "business_account:read",
		"--ip-allowlist", "10.0.0.0/8")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "尚未实现")
}

func TestAccountCreate要求id_flag(t *testing.T) {
	_, err := runCmd(t, "account", "create")
	require.Error(t, err)
	// Cobra 必填 flag 缺失会输出英文，但仍是 error；此处不强求中文
}

func TestAccountCreate占位返回错误(t *testing.T) {
	_, err := runCmd(t, "account", "create", "--id", "biz-001")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "尚未实现")
}

func TestAccountRecharge占位返回错误(t *testing.T) {
	_, err := runCmd(t, "account", "recharge", "--id", "biz-001", "--amount", "100")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "尚未实现")
}

func TestDriftCheck占位返回错误(t *testing.T) {
	_, err := runCmd(t, "drift-check")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "尚未实现")
	assert.Contains(t, err.Error(), "Phase 2 工作流 E")
}
