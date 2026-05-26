-- smoke.sql —— Phase 1 探活查询
--
-- 目的：sqlc 在没有任何业务 query 的情况下也能跑通 generate；
--      同时作为 internal/db 包诞生的最小种子，避免空目录导致 go build 报错。
--
-- 命名说明：文件名不能以 `_` 开头，否则生成的 Go 文件 (`_smoke.sql.go`) 会被 go build
--          按官方约定忽略（`_` / `.` 前缀文件跳过编译），导致接口实现缺失。
--
-- 生命周期：Phase 2+ 真业务 query（ledger.sql / outbox.sql 等）合入后可保留
--          作为运行时健康探针，或删除并由真实查询取代。

-- name: HealthProbe :one
-- 返回数据库当前时间，用于 /readyz 探活与生成代码烟雾测试。
SELECT NOW()::timestamptz AS ts;
