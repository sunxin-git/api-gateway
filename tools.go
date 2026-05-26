//go:build tools
// +build tools

// Package tools 锁定本仓库通过 Go module 管理的工具版本。
//
// 标准做法：在 go.mod 里通过空白 import 把工具的 main 包当作"依赖"管理，
// 配合 build tag `tools` 让正常 `go build ./...` 不会把它编译进二进制。
//
// 用途：
//   - 让所有开发者 / CI 节点跑 `go run github.com/sqlc-dev/sqlc/cmd/sqlc` 时拉到同一版本
//   - 让 `go mod tidy` 把工具依赖一起升级，避免本地与 CI 漂移
//
// 工具范围说明（**重要，与 plan 偏离的工程取舍**）：
//
//  1. sqlc：通过 go.mod 管理（本文件 import）。
//     sqlc 是代码生成器，必须随仓库版本严格锁定，否则同一份 .sql 在不同节点
//     生成的 Go 代码不一致，CI 的 sqlc-diff 检查会假阳报错。
//
//  2. golang-migrate：通过 go.mod 引入 v4 库（业务代码后续可能用 lib）；
//     但 cmd/migrate 二进制**不**走 tools.go pattern，原因：
//     `github.com/golang-migrate/migrate/v4/cmd/migrate` 通过 build tag 默认引入
//     cloud 数据库驱动（snowflake / spanner / clickhouse / cockroachdb），传递依赖巨大。
//     改为：Makefile `make install-migrate` 从 GitHub Release 下载预编译二进制。
//
//  3. golangci-lint：不走 tools.go pattern。
//     golangci-lint 官方推荐用法是 `curl install.sh` 或 GitHub Actions 的
//     golangci-lint-action，而非 go install。其传递依赖 >1000 个模块，强行
//     纳入 go.mod 会污染 require 列表。
//     改为：CI 用 golangci-lint-action 安装；本地用 `make install-lint`。
//     详见 https://golangci-lint.run/usage/install/。
//
// 参考：
//   - https://github.com/golang/go/wiki/Modules#how-can-i-track-tool-dependencies-for-a-module
//   - ADR-0003（sqlc + golang-migrate 选型）
package tools

import (
	// sqlc：从 sql 文件生成类型安全的 Go DAO 代码（ADR-0003）
	_ "github.com/sqlc-dev/sqlc/cmd/sqlc"
)
