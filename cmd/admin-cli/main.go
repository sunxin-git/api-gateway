// admin-cli 是 api-gateway 的离线运维 CLI 入口。
// 真实功能由 cmd/admin-cli/cmd 子包内的 Cobra 命令实现。
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/sunxin-git/api-gateway/cmd/admin-cli/cmd"
)

// exitCoder 让特定 RunE 错误携带退出码（如 token revoke 不存在 → exit 2，计划 Unit 6）。
//
// 任意子命令可返回实现本接口的 error；main 识别后用对应 exit code 退出。
// 不实现本接口的 error 仍按 Cobra 默认 exit 1。
type exitCoder interface{ ExitCode() int }

func main() {
	if err := cmd.Execute(); err != nil {
		// Cobra 默认会自行把错误打到 stderr；这里仅控制退出码。
		fmt.Fprintln(os.Stderr) // 留一个空行视觉分隔
		var ec exitCoder
		if errors.As(err, &ec) {
			os.Exit(ec.ExitCode())
		}
		os.Exit(1)
	}
}
