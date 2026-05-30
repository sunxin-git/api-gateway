package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/audit"
)

// CtxKeyAuditOutcomeCode gin.Context 中 audit outcome 业务 code 的 key（string）。
// handler 或子中间件可调 SetAuditOutcomeCode 写入更细粒度的语义，如 "idempotency_conflict" /
// "account_already_exists" / "ip_not_allowed"。
const CtxKeyAuditOutcomeCode = "admin_audit_outcome_code"

// SetAuditOutcomeCode 让 handler / 子 middleware 提示 audit 当前请求的业务 outcome。
//
// 在中间件 defer 中读取；最终落入 AuditRecord.OutcomeCode。
// 若调用方未设置，AdminAudit 将按 HTTP status 自动推断（"ok" / "client_error" / "internal_error"）。
func SetAuditOutcomeCode(c *gin.Context, code string) {
	c.Set(CtxKeyAuditOutcomeCode, code)
}

// requestHashHexLen 取 sha256 hex 的前 32 字符（128 bit），决策 D8。
const requestHashHexLen = 32

// AdminAudit 管理 API 审计中间件（计划 Unit 4 + R8 + 决策 D3 / D8）。
//
// 职责：
//
//  1. 读 body（最多 64 KiB）用于 request_hash；body > 64KiB 触发 413 preempt（避免下游误处理半截 body）
//  2. defer 模式记录 audit：c.Next 前注册 defer，确保 handler panic / abort / 正常返回均能 emit
//  3. 按 path / status / outcome 决定 Tier1 vs Tier2 路由到 audit.Logger
//  4. status ≥ 400 时调 thr.RecordHandlerError 触发熔断器累计（仅 token.CircuitBreakerEnabled）
//  5. Tier1 写失败时 bump auditWriteFailedCounter（readiness check 据此关闸）
//
// 必须放在 admin 链最后一个 middleware（handler 之前）；defer 顺序保证 audit 行最终一定 emit。
func AdminAudit(
	logger audit.AuditLogger,
	thr admintoken.Throttle,
	auditWriteFailedCounter *prometheus.CounterVec,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// 读 body 并复用（handler 需要再次 read）
		// 已被 AdminBodyLimit 包装为 MaxBytesReader(64KiB)；读到 MaxBytesError 表示 > 64KiB
		bodyBytes, readErr := readAndBufferBody(c.Request.Body)
		// 总是恢复 body，handler 才能 bind
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		var bodyTooLarge bool
		if readErr != nil {
			var mbe *http.MaxBytesError
			if errors.As(readErr, &mbe) {
				bodyTooLarge = true
			}
			// 其他 I/O 错误：不阻断流程，handler 读 body 时会自然报错
		}

		// 注册 defer：必须在 c.Next 之前注册，handler panic 才能命中
		defer func() {
			// 1. 先 recover panic：确保 audit 一定能 emit，再 re-panic 让外层 Recover middleware 写 500
			panicVal := recover()

			duration := time.Since(start)
			status := c.Writer.Status()
			if panicVal != nil {
				// panic 时 handler / 子 middleware 未写 status；audit 记录为 500
				status = http.StatusInternalServerError
			}

			// status >= 400 时累加熔断器错误计数（仅 token.CircuitBreakerEnabled）
			if status >= 400 {
				if vr := GetAdminTokenValidation(c); vr != nil && vr.Token != nil {
					_ = thr.RecordHandlerError(c.Request.Context(), vr.Token)
				}
			}

			// 构造 audit record
			path := c.FullPath()
			if path == "" {
				path = c.Request.URL.Path
			}
			rec := audit.AuditRecord{
				Event:         "admin_audit",
				RequestID:     GetRequestID(c),
				TimestampUTC:  time.Now().UTC(),
				SourceIP:      c.ClientIP(),
				Method:        c.Request.Method,
				Path:          path,
				RequestHash:   computeRequestHash(c.Request.Method, path, c.Request.URL.RawQuery, bodyBytes),
				BodySizeBytes: int64(len(bodyBytes)),
				Status:        status,
				DurationMs:    duration.Milliseconds(),
				OutcomeCode:   resolveOutcomeCode(c, status, bodyTooLarge),
			}
			// actor / token meta：优先归一化身份（支持 operator:<id>），回落旧 token 路径。
			if p := GetAdminPrincipal(c); p != nil {
				rec.Actor = p.AuditActor()
				if p.Token != nil {
					rec.TokenID = p.Token.ID
					rec.TokenDescription = p.Token.Description
				}
			} else if vr := GetAdminTokenValidation(c); vr != nil && vr.Token != nil {
				rec.TokenID = vr.Token.ID
				rec.TokenDescription = vr.Token.Description
				rec.Actor = "admin_token:" + strconv.FormatInt(vr.Token.ID, 10)
			} else {
				rec.Actor = "anonymous"
			}
			rec.Tier = determineTier(path, status, rec.OutcomeCode)

			if err := logger.Emit(c.Request.Context(), rec); err != nil {
				// Tier1 写失败：bump metric，让 readiness check 关闸（Unit 7 装配）
				if auditWriteFailedCounter != nil {
					auditWriteFailedCounter.WithLabelValues(auditTierLabel(rec.Tier), "emit_error").Inc()
				}
				_ = c.Error(err)
			}

			// 2. re-panic：让外层 Recover middleware 写 500 响应；不 re-panic 会让 panic 静默丢失
			if panicVal != nil {
				panic(panicVal)
			}
		}()

		// Body > 64 KiB：抢先 413（防止下游处理半截 body 返 200 让 audit 误记录成功）
		if bodyTooLarge {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": gin.H{
					"code":       "payload_too_large",
					"message":    "请求体超过上限（64 KiB）",
					"request_id": GetRequestID(c),
				},
			})
			return
		}

		c.Next()
	}
}

// readAndBufferBody 把 body 一次性读到 buffer；body 为 nil 时返空切片。
//
// 错误：透传 underlying body 的 error；调用方按 *http.MaxBytesError 类型判断超限。
func readAndBufferBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	// 容量预估：admin API 请求 body 通常 ≤ 几 KB；以 4KB 起步避免频繁 grow
	buf := bytes.NewBuffer(make([]byte, 0, 4096))
	_, err := buf.ReadFrom(body)
	return buf.Bytes(), err
}

// computeRequestHash 实施决策 D8 的请求 hash 算法。
//
// 输入：method + " " + path + "?" + sorted_query + "\n" + body[:64KB]
// 输出：sha256(input).hex 的前 32 字符（128 bit）
//
// 注意：
//   - path 用 Gin route template（c.FullPath）；避免账户 id 引入高基数差异
//     —— 但同 template 不同 :id 的请求 hash 会相同；body 中应含 account id
//   - 不包含 header（含 Authorization 明文，会污染 audit）
//   - body 超 64KB 时仅 hash 前 64KB（caller 保证 bodyBytes ≤ 64KB；本函数已是 cap-bounded）
func computeRequestHash(method, path, rawQuery string, body []byte) string {
	// body 防御 cap（理论上 AdminBodyLimit 已限到 64KB；这里二保险）
	if len(body) > AdminBodyLimitBytes {
		body = body[:AdminBodyLimitBytes]
	}
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte(" "))
	h.Write([]byte(path))
	h.Write([]byte("?"))
	h.Write([]byte(canonicalQuery(rawQuery)))
	h.Write([]byte("\n"))
	h.Write(body)
	full := hex.EncodeToString(h.Sum(nil))
	if len(full) >= requestHashHexLen {
		return full[:requestHashHexLen]
	}
	return full
}

// canonicalQuery 把 raw query string 标准化为"按 key 字典序 + 同 key 多值字典序"格式。
//
// 用于 request_hash 输入；保证 ?a=1&b=2 与 ?b=2&a=1 hash 相同（语义等价）。
func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		// 非法 query：直接使用原始字符串（hash 仍稳定，仅丧失"键序无关"语义）
		return rawQuery
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	first := true
	for _, k := range keys {
		vals := append([]string{}, values[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
	}
	return b.String()
}

// determineTier 按 path / status / outcome 决定 audit tier（决策 D3 简化）。
//
// 规则：
//   - path 含 "/refund" → Tier1（不可逆动作，必须同步落盘）
//   - status == 401 或 403 → Tier1（auth_failed / scope denied 是攻击信号）
//   - outcome == "idempotency_conflict" → Tier1（业务系统 / 攻击重放区分关键）
//   - 其他 → Tier2
//
// 注意：token lifecycle (create/revoke) 在 admin-cli 内完成，不走本中间件链；
// 当 P1 把 token CRUD 暴露为 HTTP API 时需扩展规则。
func determineTier(path string, status int, outcomeCode string) audit.AuditTier {
	if strings.Contains(path, "/refund") {
		return audit.Tier1
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return audit.Tier1
	}
	if outcomeCode == "idempotency_conflict" {
		return audit.Tier1
	}
	return audit.Tier2
}

// resolveOutcomeCode 决定 audit record 的 outcome_code 字段。
//
// 优先级：
//  1. 子中间件 / handler 通过 SetAuditOutcomeCode 显式设置
//  2. status 范围推断（fallback）
func resolveOutcomeCode(c *gin.Context, status int, bodyTooLarge bool) string {
	if v, ok := c.Get(CtxKeyAuditOutcomeCode); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if bodyTooLarge {
		return "payload_too_large"
	}
	switch {
	case status >= 200 && status < 300:
		return "ok"
	case status >= 400 && status < 500:
		return "client_error"
	case status >= 500:
		return "internal_error"
	default:
		return "unknown"
	}
}

// auditTierLabel 把 AuditTier 转为 metric label（不能含中文 / 特殊字符）。
func auditTierLabel(t audit.AuditTier) string {
	switch t {
	case audit.Tier1:
		return "tier1"
	case audit.Tier2:
		return "tier2"
	default:
		return "unknown"
	}
}
