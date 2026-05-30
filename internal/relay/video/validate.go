package video

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// 校验错误码（机器可读；Unit 10 errors.go 映射为 OpenAI 兼容 400 的 error.code）。
//
// 形状对齐 OpenAI：HTTP 400 + body `{"error":{"type":"invalid_request_error",
// "code":<Code>,"param":<Param>,"message":<Message>}}`。本层只产出语义化 Code/Param/Message，
// HTTP 状态码与 JSON 包装在 handler 层做（关注点分离）。
const (
	// CodeUnsupportedTaskType task_type 不在能力支持集（MVP 仅 text_to_video）。
	CodeUnsupportedTaskType = "unsupported_task_type"
	// CodeMissingRequiredParam 缺少必填参数（含必填字符串为空白）。
	CodeMissingRequiredParam = "missing_required_param"
	// CodeInvalidParamType 参数类型不符（如 duration 传了字符串 / 小数）。
	CodeInvalidParamType = "invalid_param_type"
	// CodeParamOutOfRange 整数参数越出取值档 [Min, Max]。
	CodeParamOutOfRange = "param_out_of_range"
	// CodeInvalidEnumValue 枚举参数取值不在合法集。
	CodeInvalidEnumValue = "invalid_enum_value"
)

// ErrorType 是 OpenAI 兼容错误响应的 type 字段固定值（校验类错误一律 invalid_request_error）。
const ErrorType = "invalid_request_error"

// ValidationError 是请求校验失败的结构化错误（计划 §Unit 4：拒绝时返回足够信息映射 400）。
//
// 实现 error 接口，但字段是结构化的，便于 handler 层映射为 OpenAI JSON（type/code/param/message）。
type ValidationError struct {
	// Code 机器可读错误码（见上常量）。
	Code string
	// Param 出错参数名（task_type / prompt / duration / ...）；便于业务定位。
	Param string
	// Message 中文人读消息（不含敏感内容；prompt 文本本身绝不回显）。
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("video 请求校验失败 [code=%s param=%s]: %s", e.Code, e.Param, e.Message)
}

// ValidatedRequest 是校验通过后的规范化请求（defaults 已填充、类型已收敛）。
//
// 下游消费：
//   - Unit 5 seedance adapter：按 typed 字段组装上游 body（content[prompt] / ratio / duration / ...）。
//   - Unit 7 billing：按 Resolution + Duration + Fps 算 reserve 上界。
//
// Params 为规范化全量参数视图（integer→int64、enum/string→string、boolean→bool），
// **不含**请求里的未知字段（未声明参数被静默忽略，fail-closed：绝不把未知字段透传上游）。
type ValidatedRequest struct {
	TaskType   TaskType
	Prompt     string
	Duration   int
	Resolution string
	Ratio      string
	Fps        int
	Params     map[string]any
}

// Validate 按能力描述符校验 (taskType, params)（计划 §Unit 4 validate.go）。
//
// 顺序：task_type 支持集 → 逐参数（必填 / 类型 / 取值档 / 枚举）。任一失败立即返回首个
// ValidationError（fail-fast，错误可定位到具体 param）。全通过则返回 defaults 已填的
// ValidatedRequest。
//
// params 来源：handler 把业务请求 body 的参数（prompt / duration / resolution / ratio / fps）
// 收进一个 map[string]any（JSON number 通常解析为 float64，本函数做整除收敛）。
func (c *Capability) Validate(taskType string, params map[string]any) (*ValidatedRequest, *ValidationError) {
	tt := TaskType(strings.TrimSpace(taskType))
	if tt == "" {
		return nil, &ValidationError{
			Code:    CodeMissingRequiredParam,
			Param:   "task_type",
			Message: "缺少必填参数 task_type",
		}
	}
	if !c.SupportsTaskType(tt) {
		return nil, &ValidationError{
			Code:    CodeUnsupportedTaskType,
			Param:   "task_type",
			Message: fmt.Sprintf("task_type=%q 暂不支持（MVP 仅支持 %s）", tt, TaskTypeTextToVideo),
		}
	}

	normalized := make(map[string]any, len(c.params))
	for _, spec := range c.params {
		raw, present := params[spec.Key]
		if !present || raw == nil {
			if spec.Required {
				return nil, &ValidationError{
					Code:    CodeMissingRequiredParam,
					Param:   spec.Key,
					Message: fmt.Sprintf("缺少必填参数 %q", spec.Key),
				}
			}
			normalized[spec.Key] = spec.Default
			continue
		}
		val, verr := validateParamValue(spec, raw)
		if verr != nil {
			return nil, verr
		}
		normalized[spec.Key] = val
	}

	return buildValidatedRequest(tt, normalized), nil
}

// validateParamValue 校验单个参数值并收敛为规范类型。
func validateParamValue(spec ParamSpec, raw any) (any, *ValidationError) {
	switch spec.Type {
	case ParamTypeString:
		s, ok := coerceString(raw)
		if !ok {
			return nil, invalidType(spec, "字符串")
		}
		if spec.Required && strings.TrimSpace(s) == "" {
			return nil, &ValidationError{
				Code:    CodeMissingRequiredParam,
				Param:   spec.Key,
				Message: fmt.Sprintf("必填参数 %q 不能为空", spec.Key),
			}
		}
		return s, nil

	case ParamTypeInteger:
		n, ok := coerceInt(raw)
		if !ok {
			return nil, invalidType(spec, "整数")
		}
		if n < spec.Min || n > spec.Max {
			return nil, &ValidationError{
				Code:    CodeParamOutOfRange,
				Param:   spec.Key,
				Message: fmt.Sprintf("参数 %q=%d 越出取值档 [%d, %d]", spec.Key, n, spec.Min, spec.Max),
			}
		}
		return n, nil

	case ParamTypeEnum:
		s, ok := coerceString(raw)
		if !ok {
			return nil, invalidType(spec, "字符串枚举")
		}
		if !containsString(spec.Enum, s) {
			return nil, &ValidationError{
				Code:    CodeInvalidEnumValue,
				Param:   spec.Key,
				Message: fmt.Sprintf("参数 %q=%q 非法，合法取值 %v", spec.Key, s, spec.Enum),
			}
		}
		return s, nil

	case ParamTypeBoolean:
		b, ok := coerceBool(raw)
		if !ok {
			return nil, invalidType(spec, "布尔")
		}
		return b, nil

	default:
		// 不应发生：capability.validateSpec 已在构造期挡掉非法 Type。
		return nil, &ValidationError{
			Code:    CodeInvalidParamType,
			Param:   spec.Key,
			Message: fmt.Sprintf("参数 %q 声明类型未知", spec.Key),
		}
	}
}

// invalidType 构造类型不符错误。
func invalidType(spec ParamSpec, want string) *ValidationError {
	return &ValidationError{
		Code:    CodeInvalidParamType,
		Param:   spec.Key,
		Message: fmt.Sprintf("参数 %q 类型不符，应为%s", spec.Key, want),
	}
}

// buildValidatedRequest 从规范化 map 抽取 typed 字段（MVP 已知参数集；防御性取值不 panic）。
func buildValidatedRequest(tt TaskType, normalized map[string]any) *ValidatedRequest {
	return &ValidatedRequest{
		TaskType:   tt,
		Prompt:     asString(normalized, "prompt"),
		Duration:   int(asInt64(normalized, "duration")),
		Resolution: asString(normalized, "resolution"),
		Ratio:      asString(normalized, "ratio"),
		Fps:        int(asInt64(normalized, "fps")),
		Params:     normalized,
	}
}

// =============================================================================
// 类型收敛 helpers
// =============================================================================

// coerceString 仅接受 string（不把数字 / 布尔强转成字符串，避免隐式宽松）。
func coerceString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// coerceBool 仅接受 bool。
func coerceBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// coerceInt 把 JSON 数字收敛为 int64：接受 int / int32 / int64 / 整除的 float64 / json.Number。
//
// 关键：float64 含小数（如 5.5）→ 拒绝（计划测试：duration 须整数）；NaN/Inf / 越 int64 → 拒绝；
// bool / string → 拒绝（类型不符）。gin 默认把 JSON number 解析为 float64，故 float64 分支是主路径。
func coerceInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		if n != math.Trunc(n) {
			return 0, false // 含小数 → 非整数
		}
		// float64 尾数 53 位，超 2^53 整除判定不可靠；用保守 int64 边界（留 1 防 round-to-MaxInt64+1）。
		if n < float64(math.MinInt64) || n >= float64(math.MaxInt64) {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

// asString 从规范化 map 取字符串；缺失 / 类型不符返回空串（normalized 由本包构造，恒命中）。
func asString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// asInt64 从规范化 map 取 int64；缺失 / 类型不符返回 0（normalized 由本包构造，恒命中）。
func asInt64(m map[string]any, key string) int64 {
	if v, ok := m[key].(int64); ok {
		return v
	}
	return 0
}
