// Package video 实现异步视频中继的「模型适配抽象」层（Phase 2 / Unit 4-5）。
//
// 计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 4 + 5
// 决策：docs/adr/0006-async-execution-asynq-redis.md
//
// 包内职责（Unit 4 范围）：
//   - Capability / ParamSpec：声明式能力描述符（**只驱动参数校验，不碰路由**）
//   - VideoCatalog / EnvVideoCatalog：env 单条视频字典（gateway-model → seedance channel
//     绑定 + 分辨率档 pricing + capability）
//   - Validate：按 capability 校验 text_to_video 请求，拒绝映射为 OpenAI 兼容 400
//
// 包内职责（Unit 5 范围，后续 commit）：
//   - AsyncProviderAdapter / seedance_adapter：Submit/Poll 对接火山 seedance 视频 API
//
// 与同步 relay（internal/relay）的关系：**平行不复用**（计划 §决策）。同步 relay 的
// ProviderAdapter 仅 ChatCompletion；异步视频是另一套 Submit/Poll 形态，故独立子包。
//
// 设计原则映射（CLAUDE.md 五）：
//   - OCP：换上游 / 调价只改 env catalog，不改 Go 代码；能力档位声明式，新增参数加 ParamSpec。
//   - 显式优于隐式：reserve 可证上界要求 catalog 按分辨率档暴露 W×H + 单价（单一价无法
//     保证 settle ≤ reserve，会撞 ledger 的 ErrCommitExceedsReserved）。
package video

import "fmt"

// TaskType 是视频生成任务类型枚举（计划 §Scope Boundaries）。
//
// MVP 第一刀**仅 text_to_video 在支持集**；其余 task_type 枚举占位但校验拒绝
// （image_to_video / start_end_frame / storyboard 等推后）。把它们列为常量是为了
// 让「不支持」的拒绝路径返回明确错误码，而非把任意字符串都当未知值。
type TaskType string

const (
	// TaskTypeTextToVideo 文生视频：无输入媒体，仅 prompt 文本驱动。MVP 唯一支持。
	TaskTypeTextToVideo TaskType = "text_to_video"
	// TaskTypeImageToVideo 图生视频（占位，MVP 不支持）。
	TaskTypeImageToVideo TaskType = "image_to_video"
	// TaskTypeStartEndFrame 首尾帧（占位，MVP 不支持）。
	TaskTypeStartEndFrame TaskType = "start_end_frame"
	// TaskTypeStoryboard 分镜多帧（占位，MVP 不支持）。
	TaskTypeStoryboard TaskType = "storyboard"
)

// ParamType 是请求参数的声明类型（驱动 validate 的类型收敛与取值校验）。
type ParamType string

const (
	// ParamTypeString 任意非空字符串（如 prompt）。
	ParamTypeString ParamType = "string"
	// ParamTypeInteger 整数，受 Min/Max 闭区间约束（如 duration / fps）。
	// JSON number 解析为 float64 时须整除校验（小数 → 拒绝）。
	ParamTypeInteger ParamType = "integer"
	// ParamTypeEnum 枚举字符串，取值须 ∈ Enum（如 resolution / ratio）。
	ParamTypeEnum ParamType = "enum"
	// ParamTypeBoolean 布尔（如 generate_audio / remove_watermark）。
	ParamTypeBoolean ParamType = "boolean"
)

// ParamSpec 是单个请求参数的声明（计划 §Unit 4 Approach：key/type/enum/min-max/default/required）。
//
// 不可变：catalog 构造后只读，可安全并发校验。
type ParamSpec struct {
	// Key 参数名（业务请求 body 中的字段名，如 "duration"）。
	Key string
	// Type 参数类型。
	Type ParamType
	// Required 是否必填。required 且缺失 → 校验拒绝；非 required 缺失 → 填 Default。
	Required bool

	// Enum ParamTypeEnum 的合法取值集（其余类型忽略）。
	Enum []string
	// Min / Max ParamTypeInteger 的闭区间 [Min, Max]（其余类型忽略）。
	Min int64
	Max int64

	// Default 非 required 参数缺省时填充的值（类型须与 Type 对应：
	// string→string、integer→int64、enum→string、boolean→bool）。
	// required 参数无 Default（缺失即拒绝）。
	Default any

	// Title 人读标签（运维 / 文档驱动预留；MVP 不强制）。
	Title string
}

// Capability 是视频模型的声明式能力描述符（计划 §Unit 4）。
//
// **只驱动参数校验，不碰路由**（路由在 catalog 单条 + Unit 6 task service）。
// 不可变：catalog 构造后只读。
type Capability struct {
	// SchemaVersion 描述符 schema 版本，预留扩展位（供后续 UI / 文档驱动演进）。
	// MVP 恒为 capabilitySchemaV1。
	SchemaVersion int

	// supportedTaskTypes 支持的 task_type 集合（MVP 仅 text_to_video）。
	// 用 map 做 O(1) 命中判定；不在集合内 → 校验拒绝（unsupported_task_type）。
	supportedTaskTypes map[TaskType]struct{}

	// params text_to_video 的参数声明集（按声明顺序校验，错误可定位）。
	// MVP 单 task_type 故单一 params 列表；P1+ 多 task_type 时改为 per-task-type。
	params []ParamSpec
}

// capabilitySchemaV1 当前能力描述符 schema 版本。
const capabilitySchemaV1 = 1

// SupportsTaskType 报告 task_type 是否在支持集内。
func (c *Capability) SupportsTaskType(t TaskType) bool {
	_, ok := c.supportedTaskTypes[t]
	return ok
}

// paramByKey 按 key 查 ParamSpec（validate 内部用）。
func (c *Capability) paramByKey(key string) (ParamSpec, bool) {
	for _, p := range c.params {
		if p.Key == key {
			return p, true
		}
	}
	return ParamSpec{}, false
}

// validateSpec 在 catalog 构造期对 Capability 自身做 fail-fast 自洽校验
// （声明本身不能矛盾：integer 须 Min≤Max、enum 须非空、default 须落在约束内、key 唯一）。
//
// 这是「显式优于隐式」：错误的能力声明在启动期暴露，而非运行期才发现
// 某参数的 default 落在 Min/Max 之外这类隐藏地雷。
func (c *Capability) validateSpec() error {
	if c.SchemaVersion != capabilitySchemaV1 {
		return fmt.Errorf("video capability: SchemaVersion=%d 非法（MVP 仅支持 %d）",
			c.SchemaVersion, capabilitySchemaV1)
	}
	if len(c.supportedTaskTypes) == 0 {
		return fmt.Errorf("video capability: supportedTaskTypes 不能为空")
	}
	seen := make(map[string]struct{}, len(c.params))
	for _, p := range c.params {
		if p.Key == "" {
			return fmt.Errorf("video capability: 存在空 Key 的 ParamSpec")
		}
		if _, dup := seen[p.Key]; dup {
			return fmt.Errorf("video capability: 参数 Key=%q 重复声明", p.Key)
		}
		seen[p.Key] = struct{}{}
		if err := p.validateSpec(); err != nil {
			return err
		}
	}
	return nil
}

// validateSpec 对单个 ParamSpec 做声明自洽校验。
func (p ParamSpec) validateSpec() error {
	switch p.Type {
	case ParamTypeInteger:
		if p.Min > p.Max {
			return fmt.Errorf("video capability: 参数 %q 的 Min(%d) > Max(%d)", p.Key, p.Min, p.Max)
		}
		if !p.Required {
			d, ok := p.Default.(int64)
			if !ok {
				return fmt.Errorf("video capability: 参数 %q 非 required 但 Default 不是 int64（%T）", p.Key, p.Default)
			}
			if d < p.Min || d > p.Max {
				return fmt.Errorf("video capability: 参数 %q 的 Default(%d) 越出 [%d, %d]", p.Key, d, p.Min, p.Max)
			}
		}
	case ParamTypeEnum:
		if len(p.Enum) == 0 {
			return fmt.Errorf("video capability: 枚举参数 %q 的 Enum 不能为空", p.Key)
		}
		if !p.Required {
			d, ok := p.Default.(string)
			if !ok {
				return fmt.Errorf("video capability: 参数 %q 非 required 但 Default 不是 string（%T）", p.Key, p.Default)
			}
			if !containsString(p.Enum, d) {
				return fmt.Errorf("video capability: 参数 %q 的 Default(%q) 不在 Enum %v 内", p.Key, d, p.Enum)
			}
		}
	case ParamTypeString:
		if !p.Required {
			if _, ok := p.Default.(string); !ok {
				return fmt.Errorf("video capability: 参数 %q 非 required 但 Default 不是 string（%T）", p.Key, p.Default)
			}
		}
	case ParamTypeBoolean:
		if !p.Required {
			if _, ok := p.Default.(bool); !ok {
				return fmt.Errorf("video capability: 参数 %q 非 required 但 Default 不是 bool（%T）", p.Key, p.Default)
			}
		}
	default:
		return fmt.Errorf("video capability: 参数 %q 的 Type=%q 非法", p.Key, p.Type)
	}
	return nil
}

// containsString 报告 v 是否在 set 内（小集合线性扫描，避免为 < 10 元素建 map）。
func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
