package session

import (
	"errors"
	"testing"
	"time"
)

// TestCreate_Lookup_HappyPath 建会话后用明文 token 查回，身份/字段一致。
func TestCreate_Lookup_HappyPath(t *testing.T) {
	pool, svc := setupSuite(t, time.Hour)
	opID, username := createTestOperator(t, pool)

	token, csrf, expiresAt, err := svc.Create(ctxT(t), opID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if token == "" || csrf == "" {
		t.Fatalf("token / csrf 不应为空")
	}
	if !expiresAt.After(time.Now()) {
		t.Fatalf("expiresAt 应在未来：%v", expiresAt)
	}

	sc, err := svc.Lookup(ctxT(t), token)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sc.OperatorID != opID {
		t.Fatalf("Lookup operatorID = %d, want %d", sc.OperatorID, opID)
	}
	if sc.Username != username {
		t.Fatalf("Lookup username = %q, want %q", sc.Username, username)
	}
	if sc.CSRFToken != csrf {
		t.Fatalf("Lookup csrf = %q, want %q", sc.CSRFToken, csrf)
	}
}

// TestLookup_InvalidToken 随机 / 空 token → ErrSessionInvalid。
func TestLookup_InvalidToken(t *testing.T) {
	pool, svc := setupSuite(t, time.Hour)
	_ = pool
	if _, err := svc.Lookup(ctxT(t), "this-is-not-a-real-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("无效 token 应返 ErrSessionInvalid，得 %v", err)
	}
	if _, err := svc.Lookup(ctxT(t), ""); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("空 token 应返 ErrSessionInvalid，得 %v", err)
	}
}

// TestLookup_Expired 过期会话 → ErrSessionInvalid（DB expires_at > NOW() 过滤）。
func TestLookup_Expired(t *testing.T) {
	pool, svc := setupSuite(t, time.Hour)
	opID, _ := createTestOperator(t, pool)

	token := insertExpiredSession(t, pool, svc, opID)
	if _, err := svc.Lookup(ctxT(t), token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("过期会话应返 ErrSessionInvalid，得 %v", err)
	}
}

// TestLookup_DisabledOperator 账户禁用后会话失效（JOIN enabled=true 过滤）。
func TestLookup_DisabledOperator(t *testing.T) {
	pool, svc := setupSuite(t, time.Hour)
	opID, _ := createTestOperator(t, pool)

	token, _, _, err := svc.Create(ctxT(t), opID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 先能查到
	if _, err := svc.Lookup(ctxT(t), token); err != nil {
		t.Fatalf("禁用前应能查到：%v", err)
	}
	// 禁用账户 → 会话失效
	setOperatorEnabled(t, pool, opID, false)
	if _, err := svc.Lookup(ctxT(t), token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("禁用账户后会话应失效（ErrSessionInvalid），得 %v", err)
	}
}

// TestDelete_Idempotent 登出删会话后查不到；重复 Delete 幂等不报错。
func TestDelete_Idempotent(t *testing.T) {
	pool, svc := setupSuite(t, time.Hour)
	opID, _ := createTestOperator(t, pool)

	token, _, _, err := svc.Create(ctxT(t), opID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(ctxT(t), token); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Lookup(ctxT(t), token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("删除后应查不到，得 %v", err)
	}
	// 再删一次幂等
	if err := svc.Delete(ctxT(t), token); err != nil {
		t.Fatalf("重复 Delete 应幂等，得 %v", err)
	}
}

// TestDeleteByOperator 删某运维全部会话。
func TestDeleteByOperator(t *testing.T) {
	pool, svc := setupSuite(t, time.Hour)
	opID, _ := createTestOperator(t, pool)

	t1, _, _, _ := svc.Create(ctxT(t), opID)
	_, _, _, _ = svc.Create(ctxT(t), opID)

	n, err := svc.DeleteByOperator(ctxT(t), opID)
	if err != nil {
		t.Fatalf("DeleteByOperator: %v", err)
	}
	if n < 2 {
		t.Fatalf("应删 ≥2 个会话，得 %d", n)
	}
	if _, err := svc.Lookup(ctxT(t), t1); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("删后第一个会话应失效，得 %v", err)
	}
}

// TestDeleteExpired sweep 删过期会话，不动有效会话。
func TestDeleteExpired(t *testing.T) {
	pool, svc := setupSuite(t, time.Hour)
	opID, _ := createTestOperator(t, pool)

	validToken, _, _, _ := svc.Create(ctxT(t), opID)
	expiredToken := insertExpiredSession(t, pool, svc, opID)

	n, err := svc.DeleteExpired(ctxT(t))
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n < 1 {
		t.Fatalf("应删 ≥1 个过期会话，得 %d", n)
	}
	// 有效会话仍在；过期 token 已被删（Lookup 本就因过期返 invalid，这里验证有效不受影响）
	if _, err := svc.Lookup(ctxT(t), validToken); err != nil {
		t.Fatalf("有效会话不应被 sweep 删，得 %v", err)
	}
	_ = expiredToken
}
