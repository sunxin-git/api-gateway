package operator

import (
	"errors"
	"strings"
	"testing"
)

// TestCreate_Authenticate_HappyPath 创建后用正确口令认证通过，返回视图不含哈希字段。
func TestCreate_Authenticate_HappyPath(t *testing.T) {
	_, svc := setupSuite(t)
	username := uniqueUsername("happy")
	const password = "correct-horse-battery"

	acct := createTestAccount(t, svc, username, password)
	if acct.Username != username {
		t.Fatalf("username = %q, want %q", acct.Username, username)
	}
	if !acct.Enabled {
		t.Fatalf("新建账户应 enabled")
	}

	got, err := svc.Authenticate(ctxT(t), username, password)
	if err != nil {
		t.Fatalf("Authenticate 正确口令应通过，得 %v", err)
	}
	if got.ID != acct.ID {
		t.Fatalf("Authenticate 返回 id=%d，want %d", got.ID, acct.ID)
	}
}

// TestCreate_UsernameExists 重复用户名 → ErrUsernameExists。
func TestCreate_UsernameExists(t *testing.T) {
	_, svc := setupSuite(t)
	username := uniqueUsername("dup")
	createTestAccount(t, svc, username, "password-one")

	_, err := svc.Create(ctxT(t), CreateParams{Username: username, Password: "password-two", CreatedBy: "test"})
	if !errors.Is(err, ErrUsernameExists) {
		t.Fatalf("重复用户名应返 ErrUsernameExists，得 %v", err)
	}
}

// TestCreate_InvalidParams 入参边界 → ErrInvalidParam。
func TestCreate_InvalidParams(t *testing.T) {
	_, svc := setupSuite(t)
	cases := []struct {
		name   string
		params CreateParams
	}{
		{"空用户名", CreateParams{Username: "", Password: "longenough", CreatedBy: "test"}},
		{"用户名过短", CreateParams{Username: "ab", Password: "longenough", CreatedBy: "test"}},
		{"用户名非法字符", CreateParams{Username: "bad user!", Password: "longenough", CreatedBy: "test"}},
		{"口令过短", CreateParams{Username: uniqueUsername("short"), Password: "short", CreatedBy: "test"}},
		{"口令超 72 字节", CreateParams{Username: uniqueUsername("long"), Password: strings.Repeat("x", 73), CreatedBy: "test"}},
		{"created_by 空", CreateParams{Username: uniqueUsername("nocb"), Password: "longenough", CreatedBy: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(ctxT(t), tc.params)
			if !errors.Is(err, ErrInvalidParam) {
				t.Fatalf("%s 应返 ErrInvalidParam，得 %v", tc.name, err)
			}
		})
	}
}

// TestAuthenticate_Failures 错口令 / 禁用账户 / 不存在用户名 → 统一 ErrAuthFailed（防枚举）。
func TestAuthenticate_Failures(t *testing.T) {
	_, svc := setupSuite(t)
	username := uniqueUsername("auth")
	const password = "the-real-password"
	acct := createTestAccount(t, svc, username, password)

	t.Run("错口令", func(t *testing.T) {
		_, err := svc.Authenticate(ctxT(t), username, "wrong-password")
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("错口令应返 ErrAuthFailed，得 %v", err)
		}
	})

	t.Run("不存在用户名", func(t *testing.T) {
		_, err := svc.Authenticate(ctxT(t), uniqueUsername("ghost"), password)
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("不存在用户名应返 ErrAuthFailed，得 %v", err)
		}
	})

	t.Run("禁用账户拒登", func(t *testing.T) {
		if _, err := svc.SetEnabled(ctxT(t), acct.ID, false); err != nil {
			t.Fatalf("SetEnabled false: %v", err)
		}
		_, err := svc.Authenticate(ctxT(t), username, password)
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("禁用账户正确口令应返 ErrAuthFailed，得 %v", err)
		}
		// 重新启用后又能登录
		if _, err := svc.SetEnabled(ctxT(t), acct.ID, true); err != nil {
			t.Fatalf("SetEnabled true: %v", err)
		}
		if _, err := svc.Authenticate(ctxT(t), username, password); err != nil {
			t.Fatalf("重新启用后应能登录，得 %v", err)
		}
	})
}

// TestGetByID_And_NotFound GetByID 命中 + 不存在 → ErrNotFound。
func TestGetByID_And_NotFound(t *testing.T) {
	_, svc := setupSuite(t)
	acct := createTestAccount(t, svc, uniqueUsername("get"), "password-get")

	got, err := svc.GetByID(ctxT(t), acct.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Username != acct.Username {
		t.Fatalf("GetByID username = %q, want %q", got.Username, acct.Username)
	}

	if _, err := svc.GetByID(ctxT(t), 999_000_111); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在 id 应返 ErrNotFound，得 %v", err)
	}
}

// TestSetEnabled_NotFound 启停不存在账户 → ErrNotFound。
func TestSetEnabled_NotFound(t *testing.T) {
	_, svc := setupSuite(t)
	if _, err := svc.SetEnabled(ctxT(t), 999_000_222, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在 id SetEnabled 应返 ErrNotFound，得 %v", err)
	}
}

// TestSetPassword 改口令后旧口令失效、新口令生效；不存在 → ErrNotFound；过短 → ErrInvalidParam。
func TestSetPassword(t *testing.T) {
	_, svc := setupSuite(t)
	username := uniqueUsername("pw")
	const oldPw = "old-password-123"
	const newPw = "new-password-456"
	acct := createTestAccount(t, svc, username, oldPw)

	if err := svc.SetPassword(ctxT(t), acct.ID, newPw); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, err := svc.Authenticate(ctxT(t), username, oldPw); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("旧口令应失效（ErrAuthFailed），得 %v", err)
	}
	if _, err := svc.Authenticate(ctxT(t), username, newPw); err != nil {
		t.Fatalf("新口令应生效，得 %v", err)
	}

	if err := svc.SetPassword(ctxT(t), acct.ID, "short"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("过短口令应返 ErrInvalidParam，得 %v", err)
	}
	if err := svc.SetPassword(ctxT(t), 999_000_333, newPw); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在 id SetPassword 应返 ErrNotFound，得 %v", err)
	}
}

// TestCount_Delta Count 随创建递增（增量断言，不依赖表绝对为空）。
func TestCount_Delta(t *testing.T) {
	_, svc := setupSuite(t)
	before, err := svc.Count(ctxT(t))
	if err != nil {
		t.Fatalf("Count before: %v", err)
	}
	createTestAccount(t, svc, uniqueUsername("cnt"), "password-cnt")
	after, err := svc.Count(ctxT(t))
	if err != nil {
		t.Fatalf("Count after: %v", err)
	}
	if after != before+1 {
		t.Fatalf("Count 应 +1：before=%d after=%d", before, after)
	}
}

// TestList_NoHashLeak List 返回视图字段不含哈希（结构上 OperatorAccount 无 password 字段，
// 此处兜底断言能列出且 username 正确）。
func TestList_NoHashLeak(t *testing.T) {
	_, svc := setupSuite(t)
	username := uniqueUsername("list")
	createTestAccount(t, svc, username, "password-list")

	accts, err := svc.List(ctxT(t))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, a := range accts {
		if a.Username == username {
			found = true
		}
	}
	if !found {
		t.Fatalf("List 应含刚建账户 %q", username)
	}
}
