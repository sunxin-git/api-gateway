// admin-cli 是 api-gateway 的离线运维 CLI 入口。
// 真实功能由 cmd/admin-cli/cmd 子包内的 Cobra 命令实现；Phase 1 仅暴露占位骨架。
package main

import (
	"fmt"
	"os"

	"github.com/sunxin-git/api-gateway/cmd/admin-cli/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// Cobra 默认会自行把错误打到 stderr；这里仅控制退出码。
		// 避免重复打印：cmd.Execute 在 SilenceErrors=false 时已输出错误。
		fmt.Fprintln(os.Stderr) // 留一个空行视觉分隔
		os.Exit(1)
	}
}
