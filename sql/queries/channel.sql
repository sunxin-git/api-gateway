-- channel.sql —— 渠道（上游 provider 凭据抽象）CRUD + 路由热路径查询
--
-- 设计文档：docs/multimedia-gateway-design.md §8.2
-- 实施计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 2（供 Unit 3 channel service）
--
-- 安全约定：credentials_encrypted 是 envelope 密文（AES-GCM，KEK），明文绝不入库；
--   key_version 标记加密用的 KEK 版本，支持轮换（ADR-0006 决策 4）。


-- name: InsertChannel :one
-- 创建渠道；credentials_encrypted 由 service 层（Unit 3）用 KEK 加密后传入。
INSERT INTO channel (
    name, provider_type, enabled,
    restricted_business_accounts, channel_purpose,
    credentials_encrypted, key_version, other_settings
)
VALUES (
    @name, @provider_type, @enabled,
    @restricted_business_accounts, sqlc.narg('channel_purpose'),
    @credentials_encrypted, @key_version, @other_settings
)
RETURNING *;


-- name: GetChannelByID :one
-- 按 id 查渠道（含密文；service 层即用即解、不回显明文）。
SELECT * FROM channel WHERE id = @id;


-- name: ListActiveChannels :many
-- 列出所有启用的渠道（路由候选 / 运营列表）。
SELECT * FROM channel
WHERE enabled = true
ORDER BY id;


-- name: ListActiveChannelsByProvider :many
-- 按 provider_type 列出启用的渠道（工厂按 provider 选 adapter 时用）。
SELECT * FROM channel
WHERE provider_type = @provider_type
  AND enabled = true
ORDER BY id;


-- name: UpdateChannelCredentials :one
-- 更新渠道凭据密文 + KEK 版本（凭据轮换 / KEK 重加密命令用，ADR-0006 决策 4）。
-- 仅动密文与版本，不碰其他配置；明文绝不入库。
UPDATE channel
SET credentials_encrypted = @credentials_encrypted,
    key_version           = @key_version,
    updated_at            = NOW()
WHERE id = @id
RETURNING *;


-- name: SetChannelEnabled :one
-- 启用 / 停用渠道（软下线优先于硬删除）。
UPDATE channel
SET enabled    = @enabled,
    updated_at = NOW()
WHERE id = @id
RETURNING id, enabled;


-- name: DeleteChannel :execrows
-- 硬删除渠道（运营慎用；返回受影响行数判断是否存在）。
DELETE FROM channel WHERE id = @id;
