// Package crypto 提供凭据信封加密（AES-256-GCM + 多版本 KEK keyring）。
//
// 设计依据 ADR-0006 决策 4：
//   - 多版本 KEK keyring（GATEWAY_KEK_V1 / V2 / ...）；
//   - 加密恒用当前**最高版本** KEK，并记录所用 key_version；
//   - 解密按记录的 key_version 选对应 KEK；
//   - 解密 **fail-closed**：KEK 版本缺失 / GCM 认证失败 / 密文损坏 → 返 sentinel error，
//     **绝不**返回明文、**绝不**降级。
//
// 密文布局：`nonce(12B) || ciphertext+tag`。key_version **不入密文**（存 DB 独立列），
// Decrypt 时由调用方显式传入——避免密文自描述版本被篡改后绕过（版本错 → GCM Open 失败 → fail-closed）。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KEKBytes 是 KEK 的字节长度：AES-256 → 32 字节。
const KEKBytes = 32

// Sentinel errors（解密 fail-closed；调用方据此拒绝使用凭据 / 拒绝提交）。
var (
	// ErrUnknownKeyVersion 记录的 key_version 在 keyring 中无对应 KEK。
	ErrUnknownKeyVersion = errors.New("crypto: 未知 KEK 版本（解密 fail-closed）")
	// ErrDecryptFailed GCM 认证不通过 / 密文被篡改或损坏。
	ErrDecryptFailed = errors.New("crypto: 解密失败（GCM 认证不通过或密文损坏）")
	// ErrCiphertextTooShort 密文短于 nonce 长度，不可能合法。
	ErrCiphertextTooShort = errors.New("crypto: 密文长度不足（缺 nonce）")
)

// Keyring 持有多版本 KEK 的 AEAD：加密用最高版本，解密按版本选。
//
// 不可变：构造后只读，并发安全（cipher.AEAD 的 Seal/Open 并发安全）。
type Keyring struct {
	aeads   map[int32]cipher.AEAD
	current int32 // 最高版本号（加密使用）
}

// NewKeyring 构造 keyring。
//
// 校验：至少 1 个版本；每个版本号 ≥ 1；每个 KEK 恰为 32 字节（AES-256）。
// 入参 keys 的字节会被 aes.NewCipher 内部拷贝，调用方可自行 zeroize。
func NewKeyring(keys map[int32][]byte) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("crypto: keyring 至少需要 1 个 KEK 版本")
	}
	kr := &Keyring{aeads: make(map[int32]cipher.AEAD, len(keys))}
	for v, k := range keys {
		if v < 1 {
			return nil, fmt.Errorf("crypto: KEK 版本号必须 ≥ 1，得到 %d", v)
		}
		if len(k) != KEKBytes {
			return nil, fmt.Errorf("crypto: KEK v%d 长度 %d != %d（需 AES-256）", v, len(k), KEKBytes)
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, fmt.Errorf("crypto: KEK v%d aes.NewCipher: %w", v, err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("crypto: KEK v%d cipher.NewGCM: %w", v, err)
		}
		kr.aeads[v] = gcm
		if v > kr.current {
			kr.current = v
		}
	}
	return kr, nil
}

// CurrentVersion 返回加密将使用的最高 KEK 版本。
func (kr *Keyring) CurrentVersion() int32 { return kr.current }

// Encrypt 用当前最高版本 KEK 加密 plaintext，返回 `nonce || ciphertext+tag` 与所用 key_version。
func (kr *Keyring) Encrypt(plaintext []byte) (ciphertext []byte, keyVersion int32, err error) {
	gcm := kr.aeads[kr.current] // current 必存在（构造保证）
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, 0, fmt.Errorf("crypto: 生成 nonce 失败: %w", err)
	}
	// Seal 把 ciphertext+tag 追加到 nonce 之后：返回 nonce || ciphertext+tag
	return gcm.Seal(nonce, nonce, plaintext, nil), kr.current, nil
}

// Decrypt 按 keyVersion 选 KEK 解密；任何失败均 fail-closed（返 sentinel，不返明文）。
func (kr *Keyring) Decrypt(ciphertext []byte, keyVersion int32) ([]byte, error) {
	gcm, ok := kr.aeads[keyVersion]
	if !ok {
		return nil, fmt.Errorf("%w: v%d", ErrUnknownKeyVersion, keyVersion)
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns {
		return nil, ErrCiphertextTooShort
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// 不透传底层错误细节（避免侧信道）；统一 fail-closed sentinel。
		return nil, ErrDecryptFailed
	}
	return pt, nil
}

// DecodeKEK 把 KEK 字符串（hex 或 base64）解码为原始字节，并校验恰为 32 字节（AES-256）。
//
// **按长度路由消歧义**（评审 #8）：32 字节 KEK 的 hex 形式恰为 64 字符、base64 形式为
// 43/44 字符，两者长度不重叠。故 len==64 走 hex，否则走 base64——避免「base64 串恰好全是
// hex 字符」被误当 hex 解码后长度校验失败、误导运维。供 main.go 装配 keyring 时调用。
func DecodeKEK(encoded string) ([]byte, error) {
	raw := strings.TrimSpace(encoded)
	if raw == "" {
		return nil, errors.New("crypto: KEK 为空")
	}
	var b []byte
	var err error
	if len(raw) == KEKBytes*2 { // 64 字符 → 必为 hex 形式的 32 字节
		b, err = hex.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("crypto: KEK 长 64 字符但非合法 hex: %w", err)
		}
	} else { // 其余长度按 base64 解
		b, err = decodeBase64Any(raw)
		if err != nil {
			return nil, errors.New("crypto: KEK 既非 64 字符 hex 也非合法 base64（应为 openssl rand -hex 32 或 -base64 32 输出）")
		}
	}
	if len(b) != KEKBytes {
		return nil, fmt.Errorf(
			"crypto: KEK 解码后 %d 字节 != %d（AES-256；应为 64 hex 字符或 base64 编码的 32 字节）",
			len(b), KEKBytes,
		)
	}
	return b, nil
}

// DecodeHexOrBase64 把 hex 或 base64 字符串解码为原始字节，不校验长度（长度由调用方判定）。
//
// hex 优先（openssl rand -hex 输出），再尝试 base64 多形态（std / raw / urlsafe）。
// 供 config 的 pepper 解码复用（评审 #11：消除与本函数的重复实现）。
// 注意：对长度敏感、需消歧义的固定长密钥（如 KEK）应改用 DecodeKEK 的按长度路由。
func DecodeHexOrBase64(encoded string) ([]byte, error) {
	raw := strings.TrimSpace(encoded)
	if raw == "" {
		return nil, errors.New("crypto: 输入为空")
	}
	if b, err := hex.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := decodeBase64Any(raw); err == nil {
		return b, nil
	}
	return nil, errors.New("crypto: 既非合法 hex 也非合法 base64")
}

// decodeBase64Any 依次尝试 std / raw-std / urlsafe / raw-urlsafe 四种 base64 形态。
func decodeBase64Any(raw string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	return nil, errors.New("crypto: 非合法 base64")
}
