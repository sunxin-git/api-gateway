-- entitlement.sql —— 账户×模型授权 grant / revoke / check
--
-- 实施计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 2（供 Unit 10/11）
-- 语义：行存在 = 已授权；revoke = 删行；check = 存在性。复合主键 (account, gateway_model)。


-- name: GrantEntitlement :one
-- 授权账户使用某 gateway model；幂等（重复 grant 仅刷新 updated_at，不报错）。
INSERT INTO business_account_model_entitlement (business_account_id, gateway_model)
VALUES (@business_account_id, @gateway_model)
ON CONFLICT (business_account_id, gateway_model) DO UPDATE
    SET updated_at = NOW()
RETURNING *;


-- name: RevokeEntitlement :execrows
-- 撤销授权（删行）；返回受影响行数判断是否原本存在。
DELETE FROM business_account_model_entitlement
WHERE business_account_id = @business_account_id
  AND gateway_model = @gateway_model;


-- name: CheckEntitlement :one
-- 鉴权热路径：账户是否被授权使用该 model（提交前校验）。
SELECT EXISTS (
    SELECT 1 FROM business_account_model_entitlement
    WHERE business_account_id = @business_account_id
      AND gateway_model = @gateway_model
) AS entitled;


-- name: ListEntitlementsByAccount :many
-- 列出账户的全部授权 model（admin / 运维）。
SELECT * FROM business_account_model_entitlement
WHERE business_account_id = @business_account_id
ORDER BY gateway_model;
