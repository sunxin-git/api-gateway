package channel

import "strings"

// ChannelCredentials 是渠道凭据的 5 段明文结构（设计文档 §8.2 / 计划 Unit 3）。
//
// 序列化为 JSON 后**整体** AES-GCM 加密，密文存 channel.credentials_encrypted。
// **绝不**入日志；service 仅 GetCredentialsForUpstream 返回明文，调用即用即弃。
//
// 5 段：
//  1. APIKey                —— 火山方舟模型推理 Bearer API Key（seedance submit/poll）
//  2. ARKAccessKey/SecretKey —— 火山方舟 AK/SK（部分签名接口；text_to_video MVP 走 Bearer，可空）
//  3. TOSAccessKey/SecretKey —— TOS 对象存储 AK/SK（结果转存，Unit 9）
//  4. TOSBucket（+ Endpoint/Region）—— TOS 目标 bucket 及接入点（非机密标识/配置）
//  5. ProjectID             —— 火山方舟 project_id（资产可见性 / 项目隔离）
type ChannelCredentials struct {
	APIKey       string `json:"api_key,omitempty"`
	ARKAccessKey string `json:"ark_access_key,omitempty"`
	ARKSecretKey string `json:"ark_secret_key,omitempty"`
	TOSAccessKey string `json:"tos_access_key,omitempty"`
	TOSSecretKey string `json:"tos_secret_key,omitempty"`
	TOSBucket    string `json:"tos_bucket,omitempty"`
	TOSEndpoint  string `json:"tos_endpoint,omitempty"`
	TOSRegion    string `json:"tos_region,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
}

// MaskedCredentials 是凭据的掩码视图（List/Get 回显用）。
//
// 安全红线（ADR-0006 决策 4 / 计划 Unit 3）：
//   - 密钥类字段（Secret Key）一律固定占位「已设置 / 未设置」，**绝不**含任何明文片段；
//   - 非机密标识符（bucket / project_id / endpoint / region）原样回显；
//   - 半公开标识（APIKey / Access Key）仅回显**前缀** + ***，便于运维识别用哪把凭据；
//   - 解密失败时全字段标记「解密失败」（既无明文可显，也提示运维该渠道凭据损坏）。
type MaskedCredentials struct {
	APIKey       string `json:"api_key"`
	ARKAccessKey string `json:"ark_access_key"`
	ARKSecretKey string `json:"ark_secret_key"`
	TOSAccessKey string `json:"tos_access_key"`
	TOSSecretKey string `json:"tos_secret_key"`
	TOSBucket    string `json:"tos_bucket"`
	TOSEndpoint  string `json:"tos_endpoint"`
	TOSRegion    string `json:"tos_region"`
	ProjectID    string `json:"project_id"`
}

const (
	maskSet       = "已设置"
	maskUnset     = "（未设置）"
	maskDecFailed = "（解密失败）"
	maskPrefixLen = 6 // 前缀掩码保留位数
)

// Masked 生成掩码视图。
func (c ChannelCredentials) Masked() MaskedCredentials {
	return MaskedCredentials{
		APIKey:       maskPrefix(c.APIKey),       // 前缀（识别用哪把 key）
		ARKAccessKey: maskPrefix(c.ARKAccessKey), // AK 是标识非机密 → 前缀
		ARKSecretKey: maskSecret(c.ARKSecretKey), // 机密 → 固定占位
		TOSAccessKey: maskPrefix(c.TOSAccessKey),
		TOSSecretKey: maskSecret(c.TOSSecretKey), // 机密 → 固定占位
		TOSBucket:    c.TOSBucket,                // 非机密 → 原样
		TOSEndpoint:  c.TOSEndpoint,
		TOSRegion:    c.TOSRegion,
		ProjectID:    c.ProjectID,
	}
}

// maskedDecryptFailed 返回解密失败时的占位视图（不含任何明文）。
func maskedDecryptFailed() MaskedCredentials {
	return MaskedCredentials{
		APIKey:       maskDecFailed,
		ARKAccessKey: maskDecFailed,
		ARKSecretKey: maskDecFailed,
		TOSAccessKey: maskDecFailed,
		TOSSecretKey: maskDecFailed,
		TOSBucket:    maskDecFailed,
		TOSEndpoint:  maskDecFailed,
		TOSRegion:    maskDecFailed,
		ProjectID:    maskDecFailed,
	}
}

// maskSecret 密钥类字段：固定占位，绝不回显任何明文片段。
func maskSecret(s string) string {
	if strings.TrimSpace(s) == "" {
		return maskUnset
	}
	return maskSet
}

// maskPrefix 半公开标识：保留前 maskPrefixLen 位 + ***（便于运维识别）。
// 过短（≤ maskPrefixLen）视同机密，退回固定占位，避免泄露完整短串。
func maskPrefix(s string) string {
	if strings.TrimSpace(s) == "" {
		return maskUnset
	}
	if len(s) <= maskPrefixLen {
		return maskSet
	}
	return s[:maskPrefixLen] + "***"
}
