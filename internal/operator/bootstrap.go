package operator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// seedCreatedBy 初始管理员种子的 created_by 标记。
const seedCreatedBy = "seed"

// Bootstrap 在 operator_account 表空时用 env 配置种子初始管理员（幂等，不依赖 CLI）。
//
// 决策：ADR-0008 决策 6。语义：
//   - 表已有任意运维账户 → **跳过**（幂等，即使 env 提供了 bootstrap 也不重复建）。
//   - 表空 + 提供了 username+password → 建初始管理员（created_by="seed"）。
//   - 表空 + 未提供 bootstrap：
//   - production → 返错（fail-fast：无任何运维账户且无种子 → 永远无法登录）；
//   - 非 production → 仅告警并跳过（dev/test 可后续手工建或测试注入）。
//
// 并发安全：Count→Create 之间无锁；若两实例同时启动且都判表空，二者都 Create，
// 但 **username UNIQUE 数据库约束是最终安全保障**——只有一条真正插入，另一条命中
// ErrUsernameExists 被视作「已被其他实例种子」而安全返回 nil（不报错）。
func Bootstrap(ctx context.Context, svc Service, username, password string, isProduction bool, log *slog.Logger) error {
	username = strings.TrimSpace(username)
	configured := username != "" && password != ""

	count, err := svc.Count(ctx)
	if err != nil {
		return fmt.Errorf("operator bootstrap: 统计运维账户失败: %w", err)
	}

	if count > 0 {
		if configured {
			log.Info("operator bootstrap: 已存在运维账户，跳过初始管理员种子（幂等）",
				slog.Int64("existing_count", count))
		}
		return nil
	}

	// 表空
	if !configured {
		if isProduction {
			return errors.New("operator bootstrap: production 下无任何运维账户且未配置 " +
				"GATEWAY_ADMIN_BOOTSTRAP_USERNAME / GATEWAY_ADMIN_BOOTSTRAP_PASSWORD —— " +
				"管理后台将无法登录（fail-fast）")
		}
		log.Warn("operator bootstrap: 无运维账户且未配置种子（非 production，跳过）；" +
			"管理后台暂无法登录，请配置 bootstrap env 或测试注入账户")
		return nil
	}

	// 表空 + 已配置 → 建初始管理员
	acct, err := svc.Create(ctx, CreateParams{
		Username:  username,
		Password:  password,
		CreatedBy: seedCreatedBy,
	})
	if err != nil {
		if errors.Is(err, ErrUsernameExists) {
			// 并发：已被其他实例种子，视作成功。
			log.Info("operator bootstrap: 初始管理员已被其他实例种子（并发），跳过",
				slog.String("username", username))
			return nil
		}
		return fmt.Errorf("operator bootstrap: 种子初始管理员失败（检查口令是否满足长度要求）: %w", err)
	}
	log.Info("operator bootstrap: 已种子初始管理员",
		slog.Int64("id", acct.ID), slog.String("username", acct.Username))
	return nil
}
