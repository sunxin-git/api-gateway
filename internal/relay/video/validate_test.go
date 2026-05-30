package video

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validCapability 构造测试用能力描述符（取自合法 catalog 的 entry.Capability）。
func validCapability(t *testing.T) *Capability {
	t.Helper()
	cat, err := NewEnvVideoCatalog(validVideoCatalogConfig())
	require.NoError(t, err)
	return cat.DefaultEntry().Capability
}

func TestValidate_Happy(t *testing.T) {
	capb := validCapability(t)
	vr, verr := capb.Validate("text_to_video", map[string]any{
		"prompt":     "一只在草地奔跑的狗",
		"duration":   float64(8), // 模拟 gin 把 JSON number 解析为 float64
		"resolution": "1080p",
	})
	require.Nil(t, verr)
	require.NotNil(t, vr)

	assert.Equal(t, TaskTypeTextToVideo, vr.TaskType)
	assert.Equal(t, "一只在草地奔跑的狗", vr.Prompt)
	assert.Equal(t, 8, vr.Duration)
	assert.Equal(t, "1080p", vr.Resolution)
	// 省略的可选参数填默认
	assert.Equal(t, "16:9", vr.Ratio)
	assert.Equal(t, 24, vr.Fps)

	// 规范化 map：整数为 int64、枚举/字符串为 string
	assert.Equal(t, int64(8), vr.Params["duration"])
	assert.Equal(t, int64(24), vr.Params["fps"])
	assert.Equal(t, "1080p", vr.Params["resolution"])
}

func TestValidate_AllDefaultsFilled(t *testing.T) {
	capb := validCapability(t)
	vr, verr := capb.Validate("text_to_video", map[string]any{"prompt": "hello"})
	require.Nil(t, verr)
	assert.Equal(t, 5, vr.Duration)
	assert.Equal(t, "720p", vr.Resolution)
	assert.Equal(t, "16:9", vr.Ratio)
	assert.Equal(t, 24, vr.Fps)
}

func TestValidate_DurationAsIntegralFloat(t *testing.T) {
	capb := validCapability(t)
	vr, verr := capb.Validate("text_to_video", map[string]any{
		"prompt":   "x",
		"duration": float64(10.0),
	})
	require.Nil(t, verr)
	assert.Equal(t, 10, vr.Duration)
}

func TestValidate_UnknownParamIgnored(t *testing.T) {
	capb := validCapability(t)
	vr, verr := capb.Validate("text_to_video", map[string]any{
		"prompt":          "x",
		"unknown_field":   float64(1),
		"another_garbage": "drop me",
	})
	require.Nil(t, verr)
	// 未声明参数不进规范化 map（fail-closed：不透传未知字段上游）
	_, ok := vr.Params["unknown_field"]
	assert.False(t, ok)
	_, ok = vr.Params["another_garbage"]
	assert.False(t, ok)
}

func TestValidate_RejectMatrix(t *testing.T) {
	cases := []struct {
		name      string
		taskType  string
		params    map[string]any
		wantCode  string
		wantParam string
	}{
		{
			"empty_task_type", "", map[string]any{"prompt": "x"},
			CodeMissingRequiredParam, "task_type",
		},
		{
			"unsupported_task_type", "image_to_video", map[string]any{"prompt": "x"},
			CodeUnsupportedTaskType, "task_type",
		},
		{
			"garbage_task_type", "totally_unknown", map[string]any{"prompt": "x"},
			CodeUnsupportedTaskType, "task_type",
		},
		{
			"missing_prompt", "text_to_video", map[string]any{"duration": float64(5)},
			CodeMissingRequiredParam, "prompt",
		},
		{
			"blank_prompt", "text_to_video", map[string]any{"prompt": "   "},
			CodeMissingRequiredParam, "prompt",
		},
		{
			"prompt_wrong_type", "text_to_video", map[string]any{"prompt": float64(1)},
			CodeInvalidParamType, "prompt",
		},
		{
			"duration_below_min", "text_to_video", map[string]any{"prompt": "x", "duration": float64(3)},
			CodeParamOutOfRange, "duration",
		},
		{
			"duration_above_max", "text_to_video", map[string]any{"prompt": "x", "duration": float64(16)},
			CodeParamOutOfRange, "duration",
		},
		{
			"duration_fractional", "text_to_video", map[string]any{"prompt": "x", "duration": float64(5.5)},
			CodeInvalidParamType, "duration",
		},
		{
			"duration_string", "text_to_video", map[string]any{"prompt": "x", "duration": "5"},
			CodeInvalidParamType, "duration",
		},
		{
			"duration_bool", "text_to_video", map[string]any{"prompt": "x", "duration": true},
			CodeInvalidParamType, "duration",
		},
		{
			"resolution_invalid", "text_to_video", map[string]any{"prompt": "x", "resolution": "240p"},
			CodeInvalidEnumValue, "resolution",
		},
		{
			"ratio_invalid", "text_to_video", map[string]any{"prompt": "x", "ratio": "21:9"},
			CodeInvalidEnumValue, "ratio",
		},
		{
			"fps_below_min", "text_to_video", map[string]any{"prompt": "x", "fps": float64(0)},
			CodeParamOutOfRange, "fps",
		},
		{
			"fps_above_max", "text_to_video", map[string]any{"prompt": "x", "fps": float64(31)},
			CodeParamOutOfRange, "fps",
		},
	}

	capb := validCapability(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vr, verr := capb.Validate(tc.taskType, tc.params)
			assert.Nil(t, vr, "拒绝时不应返回 ValidatedRequest")
			require.NotNil(t, verr, "应返回 ValidationError")
			assert.Equal(t, tc.wantCode, verr.Code, "错误码")
			assert.Equal(t, tc.wantParam, verr.Param, "出错参数名")
			assert.NotEmpty(t, verr.Message)
		})
	}
}

func TestValidate_DurationBoundaryInclusive(t *testing.T) {
	capb := validCapability(t)
	for _, d := range []int{4, 15} {
		vr, verr := capb.Validate("text_to_video", map[string]any{
			"prompt":   "x",
			"duration": float64(d),
		})
		require.Nil(t, verr, "duration=%d 应通过（闭区间边界）", d)
		assert.Equal(t, d, vr.Duration)
	}
}

func TestValidationError_ErrorString(t *testing.T) {
	e := &ValidationError{Code: CodeParamOutOfRange, Param: "duration", Message: "越界"}
	s := e.Error()
	assert.Contains(t, s, CodeParamOutOfRange)
	assert.Contains(t, s, "duration")
}

// TestCoerceInt 直测整数收敛全路径（gin 走 float64 主路径，但 native int / json.Number 等亦须正确）。
func TestCoerceInt(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
		ok   bool
	}{
		{"int", int(7), 7, true},
		{"int32", int32(7), 7, true},
		{"int64", int64(7), 7, true},
		{"float64_integral", float64(7), 7, true},
		{"float64_zero", float64(0), 0, true},
		{"float64_negative", float64(-3), -3, true},
		// 大但可精确表示且 < 2^63 → 接受（证明边界检查不会过度拒绝合法大整数）
		{"float64_large_valid", float64(int64(1) << 53), 1 << 53, true},
		{"float64_fractional", float64(7.5), 0, false},
		{"float64_nan", math.NaN(), 0, false},
		{"float64_posinf", math.Inf(1), 0, false},
		{"float64_neginf", math.Inf(-1), 0, false},
		// 2^63 不可表示为 int64（int64 上界 2^63-1）→ 必须拒绝，否则 int64() 回绕成 MinInt64（负）
		{"float64_overflow", float64(math.MaxInt64), 0, false},
		{"jsonnumber_int", json.Number("42"), 42, true},
		{"jsonnumber_fractional", json.Number("4.2"), 0, false},
		{"jsonnumber_garbage", json.Number("abc"), 0, false},
		{"string", "7", 0, false},
		{"bool", true, 0, false},
		{"nil", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := coerceInt(tc.in)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// TestValidateParamValue_Types 直测四类型分支（含 MVP capability 未声明但框架支持的 boolean）。
func TestValidateParamValue_Types(t *testing.T) {
	// boolean：MVP capability 暂未声明布尔参数（generate_audio 等推后 Unit 5），直测覆盖该分支。
	boolSpec := ParamSpec{Key: "generate_audio", Type: ParamTypeBoolean, Required: false, Default: true}
	v, verr := validateParamValue(boolSpec, false)
	require.Nil(t, verr)
	assert.Equal(t, false, v)
	_, verr = validateParamValue(boolSpec, "yes")
	require.NotNil(t, verr)
	assert.Equal(t, CodeInvalidParamType, verr.Code)

	// integer
	intSpec := ParamSpec{Key: "n", Type: ParamTypeInteger, Min: 1, Max: 10}
	v, verr = validateParamValue(intSpec, float64(5))
	require.Nil(t, verr)
	assert.Equal(t, int64(5), v)

	// enum
	enumSpec := ParamSpec{Key: "r", Type: ParamTypeEnum, Enum: []string{"a", "b"}}
	v, verr = validateParamValue(enumSpec, "a")
	require.Nil(t, verr)
	assert.Equal(t, "a", v)
	_, verr = validateParamValue(enumSpec, "z")
	require.NotNil(t, verr)
	assert.Equal(t, CodeInvalidEnumValue, verr.Code)

	// string（非必填，空白允许、原样返回）
	strSpec := ParamSpec{Key: "s", Type: ParamTypeString, Required: false, Default: ""}
	v, verr = validateParamValue(strSpec, "hi")
	require.Nil(t, verr)
	assert.Equal(t, "hi", v)
}
