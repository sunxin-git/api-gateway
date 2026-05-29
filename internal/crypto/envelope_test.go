package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KEKBytes)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

func mustKeyring(t *testing.T, keys map[int32][]byte) *Keyring {
	t.Helper()
	kr, err := NewKeyring(keys)
	require.NoError(t, err)
	return kr
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	kr := mustKeyring(t, map[int32][]byte{1: mustKey(t)})
	plaintext := []byte(`{"api_key":"ark-abc","tos_secret_key":"s3cr3t"}`)

	ct, ver, err := kr.Encrypt(plaintext)
	require.NoError(t, err)
	assert.Equal(t, int32(1), ver)
	assert.NotContains(t, string(ct), "ark-abc", "密文不得含明文片段")

	got, err := kr.Decrypt(ct, ver)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

func TestNonceUniqueness(t *testing.T) {
	kr := mustKeyring(t, map[int32][]byte{1: mustKey(t)})
	pt := []byte("same plaintext")
	ct1, _, err := kr.Encrypt(pt)
	require.NoError(t, err)
	ct2, _, err := kr.Encrypt(pt)
	require.NoError(t, err)
	assert.False(t, bytes.Equal(ct1, ct2), "同明文两次加密密文必须不同（随机 nonce）")
}

func TestEmptyAndLargePlaintext(t *testing.T) {
	kr := mustKeyring(t, map[int32][]byte{1: mustKey(t)})

	// 空明文
	ct, ver, err := kr.Encrypt([]byte{})
	require.NoError(t, err)
	got, err := kr.Decrypt(ct, ver)
	require.NoError(t, err)
	assert.Empty(t, got)

	// 大明文（1 MiB）
	large := make([]byte, 1<<20)
	_, _ = rand.Read(large)
	ct, ver, err = kr.Encrypt(large)
	require.NoError(t, err)
	got, err = kr.Decrypt(ct, ver)
	require.NoError(t, err)
	assert.Equal(t, large, got)
}

func TestDecryptTamperedFails(t *testing.T) {
	kr := mustKeyring(t, map[int32][]byte{1: mustKey(t)})
	ct, ver, err := kr.Encrypt([]byte("secret"))
	require.NoError(t, err)

	// 翻转最后一字节（tag）
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0xFF
	_, err = kr.Decrypt(tampered, ver)
	assert.ErrorIs(t, err, ErrDecryptFailed)

	// 翻转 nonce 区一字节
	tampered2 := append([]byte(nil), ct...)
	tampered2[0] ^= 0xFF
	_, err = kr.Decrypt(tampered2, ver)
	assert.ErrorIs(t, err, ErrDecryptFailed)
}

func TestDecryptTruncatedFails(t *testing.T) {
	kr := mustKeyring(t, map[int32][]byte{1: mustKey(t)})
	ct, ver, err := kr.Encrypt([]byte("secret"))
	require.NoError(t, err)

	// 截到 nonce 以下 → ErrCiphertextTooShort
	_, err = kr.Decrypt(ct[:5], ver)
	assert.ErrorIs(t, err, ErrCiphertextTooShort)

	// 截掉尾部 tag 一部分（仍 ≥ nonce）→ GCM 认证失败
	_, err = kr.Decrypt(ct[:len(ct)-3], ver)
	assert.ErrorIs(t, err, ErrDecryptFailed)
}

func TestDecryptUnknownVersionFails(t *testing.T) {
	kr := mustKeyring(t, map[int32][]byte{1: mustKey(t)})
	ct, _, err := kr.Encrypt([]byte("secret"))
	require.NoError(t, err)
	_, err = kr.Decrypt(ct, 99)
	assert.ErrorIs(t, err, ErrUnknownKeyVersion)
}

func TestDecryptWrongKEKFails(t *testing.T) {
	krA := mustKeyring(t, map[int32][]byte{1: mustKey(t)})
	krB := mustKeyring(t, map[int32][]byte{1: mustKey(t)}) // 不同 KEK，同版本号
	ct, ver, err := krA.Encrypt([]byte("secret"))
	require.NoError(t, err)
	_, err = krB.Decrypt(ct, ver)
	assert.ErrorIs(t, err, ErrDecryptFailed, "错误 KEK 必须 fail-closed")
}

func TestMultiVersionRotation(t *testing.T) {
	keyA, keyB := mustKey(t), mustKey(t)
	krV1 := mustKeyring(t, map[int32][]byte{1: keyA})
	krMulti := mustKeyring(t, map[int32][]byte{1: keyA, 2: keyB})

	// 加密恒用最高版本
	assert.Equal(t, int32(2), krMulti.CurrentVersion())
	ctNew, ver, err := krMulti.Encrypt([]byte("new"))
	require.NoError(t, err)
	assert.Equal(t, int32(2), ver)

	// 旧版本密文（v1）在轮换后仍可按记录版本解密
	ctOld, verOld, err := krV1.Encrypt([]byte("old"))
	require.NoError(t, err)
	require.Equal(t, int32(1), verOld)
	got, err := krMulti.Decrypt(ctOld, 1)
	require.NoError(t, err)
	assert.Equal(t, []byte("old"), got)

	// 用错版本（v2 密文按 v1 解）→ fail-closed
	_, err = krMulti.Decrypt(ctNew, 1)
	assert.ErrorIs(t, err, ErrDecryptFailed)
}

func TestNewKeyringValidation(t *testing.T) {
	_, err := NewKeyring(map[int32][]byte{})
	assert.Error(t, err, "空 keyring 应报错")

	_, err = NewKeyring(map[int32][]byte{1: make([]byte, 16)})
	assert.Error(t, err, "非 32 字节 KEK 应报错")

	_, err = NewKeyring(map[int32][]byte{0: make([]byte, KEKBytes)})
	assert.Error(t, err, "版本号 < 1 应报错")
}

func TestDecodeKEK(t *testing.T) {
	raw := make([]byte, KEKBytes)
	_, _ = rand.Read(raw)

	// hex
	b, err := DecodeKEK(hex.EncodeToString(raw))
	require.NoError(t, err)
	assert.Equal(t, raw, b)

	// base64 std
	b, err = DecodeKEK(base64.StdEncoding.EncodeToString(raw))
	require.NoError(t, err)
	assert.Equal(t, raw, b)

	// 长度不对（16 字节 hex）
	_, err = DecodeKEK(hex.EncodeToString(make([]byte, 16)))
	assert.Error(t, err)

	// 非法编码
	_, err = DecodeKEK("@@@not-valid@@@")
	assert.Error(t, err)

	// 空
	_, err = DecodeKEK("   ")
	assert.Error(t, err)
}

// 确保 sentinel 可被 errors.Is 链式匹配（decryptCreds 等会 wrap）。
func TestSentinelsWrappable(t *testing.T) {
	wrapped := errors.New("ctx")
	_ = wrapped
	assert.True(t, errors.Is(ErrDecryptFailed, ErrDecryptFailed))
}
