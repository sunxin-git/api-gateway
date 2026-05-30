package video

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tt 是 text_to_video 支持集的简写（构造测试用 Capability）。
func tt() map[TaskType]struct{} {
	return map[TaskType]struct{}{TaskTypeTextToVideo: {}}
}

// TestCapabilityValidateSpec_Rejects 覆盖能力声明自洽校验的拒绝分支（防错误声明在运行期才暴露）。
func TestCapabilityValidateSpec_Rejects(t *testing.T) {
	cases := []struct {
		name string
		cap  *Capability
		sub  string
	}{
		{
			"bad_schema_version",
			&Capability{SchemaVersion: 99, supportedTaskTypes: tt()},
			"SchemaVersion",
		},
		{
			"empty_task_types",
			&Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: nil},
			"supportedTaskTypes",
		},
		{
			"empty_param_key",
			&Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: tt(), params: []ParamSpec{
				{Key: "", Type: ParamTypeString, Required: true},
			}},
			"空 Key",
		},
		{
			"duplicate_param_key",
			&Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: tt(), params: []ParamSpec{
				{Key: "x", Type: ParamTypeString, Required: true},
				{Key: "x", Type: ParamTypeString, Required: true},
			}},
			"重复",
		},
		{
			"integer_min_gt_max",
			&Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: tt(), params: []ParamSpec{
				{Key: "n", Type: ParamTypeInteger, Required: true, Min: 5, Max: 1},
			}},
			"Min",
		},
		{
			"integer_default_out_of_range",
			&Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: tt(), params: []ParamSpec{
				{Key: "n", Type: ParamTypeInteger, Required: false, Min: 1, Max: 5, Default: int64(9)},
			}},
			"Default",
		},
		{
			"integer_default_wrong_type",
			&Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: tt(), params: []ParamSpec{
				{Key: "n", Type: ParamTypeInteger, Required: false, Min: 1, Max: 5, Default: "5"},
			}},
			"Default",
		},
		{
			"enum_empty",
			&Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: tt(), params: []ParamSpec{
				{Key: "e", Type: ParamTypeEnum, Required: true, Enum: nil},
			}},
			"Enum",
		},
		{
			"enum_default_not_in_set",
			&Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: tt(), params: []ParamSpec{
				{Key: "e", Type: ParamTypeEnum, Required: false, Enum: []string{"a", "b"}, Default: "z"},
			}},
			"Default",
		},
		{
			"unknown_param_type",
			&Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: tt(), params: []ParamSpec{
				{Key: "z", Type: ParamType("weird"), Required: true},
			}},
			"Type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cap.validateSpec()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.sub)
		})
	}
}

// TestCapabilityValidateSpec_HappyAllTypes 各类型带正确 Default 的非必填参数应通过自洽校验。
func TestCapabilityValidateSpec_HappyAllTypes(t *testing.T) {
	c := &Capability{
		SchemaVersion:      capabilitySchemaV1,
		supportedTaskTypes: tt(),
		params: []ParamSpec{
			{Key: "s", Type: ParamTypeString, Required: false, Default: "x"},
			{Key: "n", Type: ParamTypeInteger, Required: false, Min: 1, Max: 10, Default: int64(5)},
			{Key: "e", Type: ParamTypeEnum, Required: false, Enum: []string{"a"}, Default: "a"},
			{Key: "b", Type: ParamTypeBoolean, Required: false, Default: true},
			{Key: "req", Type: ParamTypeString, Required: true},
		},
	}
	require.NoError(t, c.validateSpec())
}

func TestCapability_SupportsTaskType(t *testing.T) {
	c := &Capability{SchemaVersion: capabilitySchemaV1, supportedTaskTypes: tt()}
	assert.True(t, c.SupportsTaskType(TaskTypeTextToVideo))
	assert.False(t, c.SupportsTaskType(TaskTypeImageToVideo))
	assert.False(t, c.SupportsTaskType(TaskType("garbage")))
}
