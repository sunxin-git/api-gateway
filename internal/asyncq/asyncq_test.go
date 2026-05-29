package asyncq

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRedisAddr 返回集成测试用的 Redis 地址。
// 默认对齐 docker-compose 暴露端口 56379；可用 REDIS_ADDR 覆盖。
func testRedisAddr() string {
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:56379"
}

func TestDefaultQueuePriorities(t *testing.T) {
	q := DefaultQueuePriorities()
	require.Contains(t, q, QueueCritical)
	require.Contains(t, q, QueueDefault)
	require.Contains(t, q, QueueLow)
	assert.Len(t, q, 3)
	for name, w := range q {
		assert.Positive(t, w, "队列 %s 权重应为正", name)
	}
	// 优先级单调：critical > default > low（加权轮询防低优先级饿死）。
	assert.Greater(t, q[QueueCritical], q[QueueDefault])
	assert.Greater(t, q[QueueDefault], q[QueueLow])
}

func TestQueueNamesDistinctAndNonEmpty(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range []string{QueueCritical, QueueDefault, QueueLow} {
		assert.NotEmpty(t, n)
		assert.False(t, seen[n], "队列名重复: %s", n)
		seen[n] = true
	}
}

func TestConfigRedisOpt(t *testing.T) {
	cfg := Config{RedisAddr: "redis.internal:6380"}
	assert.Equal(t, "redis.internal:6380", cfg.redisOpt().Addr)
}

func TestNewClientNonNil(t *testing.T) {
	// 构造惰性，不连接 Redis；不调用 Ping（需真 Redis）。
	c := NewClient(Config{RedisAddr: "localhost:6379"})
	require.NotNil(t, c)
	require.NotNil(t, c.Client)
	_ = c.Close()
}

func TestNewServerNonNil(t *testing.T) {
	s := NewServer(Config{RedisAddr: "localhost:6379", Concurrency: 5})
	require.NotNil(t, s)
	require.NotNil(t, s.inner)
}

func TestNewServerEmptyQueuesUsesDefault(t *testing.T) {
	// Queues 为空 → 内部回落到 DefaultQueuePriorities；构造成功即覆盖该分支。
	s := NewServer(Config{RedisAddr: "localhost:6379"})
	require.NotNil(t, s)
}

func TestNewServerWithLogger(t *testing.T) {
	// 覆盖 slogAdapter 注入分支（不应 panic）。
	s := NewServer(Config{RedisAddr: "localhost:6379", Logger: slog.Default()})
	require.NotNil(t, s)
}

func TestNewServeMuxNonNil(t *testing.T) {
	require.NotNil(t, NewServeMux())
}

// TestPing_Integration 对真实 Redis 验证 server/client 的 Ping fail-fast 路径。
// Redis 不可达时跳过（与 internal/ledger 的 PG 集成测试同风格）。
func TestPing_Integration(t *testing.T) {
	addr := testRedisAddr()
	cfg := Config{RedisAddr: addr, Concurrency: 5}

	srv := NewServer(cfg)
	if err := srv.Ping(); err != nil {
		t.Skipf("Redis 不可达（%s），跳过集成测试: %v", addr, err)
	}

	// server 可达即说明连接 OK；client 走同一 RedisConnOpt 也应可达。
	cli := NewClient(cfg)
	defer func() { _ = cli.Close() }()
	require.NoError(t, cli.Ping(), "client Ping 应与 server 一致可达")
}
