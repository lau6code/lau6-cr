// 口令传输（应用层加密）的公共算法。
//
// 方案见 docs/design/13-password-transport.md §4.1：
//
//	客户端 GET /api/v1/auth/pubkey → {kid, alg, x25519_pub}
//	esk = 临时 X25519 密钥对
//	shared = X25519(esk, server_pub)
//	key = HKDF-SHA256(ikm=shared, salt=nonce(16B), info="lau6-cr password v1 | " + username + kid)
//	ct  = AES-256-GCM(key, nonce, JSON{username, password, ts})
//
// ⚠️ 前端必须**逐字**复刻这里的编码约定：
//
//   - base64url **无填充**（RawURLEncoding）
//   - nonce 固定 16 字节（同时是 HKDF 的 salt 与 GCM 的 IV）
//   - ts 是 unix **毫秒**
//   - info 串的拼接顺序是 username 在前、kid 在后
//
// 改任何一处都会导致解密失败——而失败只会表现为"解密失败"，不会告诉你是哪里错了。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 口令传输的常量。
const (
	// PasswordAlg 是算法标识（公钥接口与前端按它对齐）。
	PasswordAlg = "X25519-HKDF-SHA256-AESGCM"

	// PasswordNonceSize 是 nonce 的字节数。
	//
	// 同一个 nonce 同时用作 HKDF 的 salt 与 AES-256-GCM 的 IV
	//（docs/design/13-password-transport.md §4.1）。
	PasswordNonceSize = 16

	// PasswordTimestampWindow 是明文里 ts 的允许偏移（§4.3 第一层防护）。
	PasswordTimestampWindow = 60 * time.Second

	// PasswordNonceTTL 是 nonce 记入缓存后的存活时长（§4.3 第二层防护）。
	//
	// 必须 ≥ 2×时间窗：窗口内的重放靠 nonce 挡，
	// TTL 比窗口短就会漏掉一部分重放。
	PasswordNonceTTL = 120 * time.Second
)

// 口令传输的错误。
//
// ⚠️ 这些错误**只给服务端看**：对外一律是笼统的 400，
// 不区分"信封坏了"和"密钥不对"（那等于给攻击者一个 oracle）。
var (
	// ErrPasswordEnvelope 表示信封本身不合法（字段缺失、base64 非法、长度不对）。
	ErrPasswordEnvelope = errors.New("口令加密信封不合法")

	// ErrPasswordDecrypt 表示解密失败（kid 未知、密钥不匹配、GCM 认证失败）。
	ErrPasswordDecrypt = errors.New("口令解密失败")

	// ErrPasswordTimestamp 表示明文里的 ts 超出了 ±60s 窗口。
	ErrPasswordTimestamp = errors.New("口令时间戳超出允许窗口")
)

// PasswordEnvelope 是客户端提交的应用层加密信封。
type PasswordEnvelope struct {
	Kid   string `json:"kid"`
	Epk   string `json:"epk"`
	Nonce string `json:"nonce"`
	Ct    string `json:"ct"`
}

// PasswordPayload 是信封解密后的明文。
type PasswordPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// OldPassword 只在改密时出现（与新口令一起加密）。
	OldPassword string `json:"old_password,omitempty"`
	// Ts 是客户端生成明文时的 unix 毫秒。
	Ts int64 `json:"ts"`
}

// PasswordKDFInfo 返回 HKDF 的 info 串。
//
// 绑定用户名与 kid：密文不能跨用户复用（info 不同 → 派生出的密钥不同 →
// GCM 认证失败）。
func PasswordKDFInfo(username, kid string) string {
	return "lau6-cr password v1 | " + username + kid
}

// PasswordB64 编码（base64url，无填充）。
func PasswordB64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// PasswordUnB64 解码（base64url，无填充）。
func PasswordUnB64(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: base64url 解码失败", ErrPasswordEnvelope)
	}
	return b, nil
}

// PasswordNonceKey 返回 nonce 的缓存键（重放防护用）。
//
// 返回错误 = nonce 本身不合法（不是"这个 nonce 没用过"）。
func PasswordNonceKey(nonceB64 string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(nonceB64)
	if err != nil {
		return "", fmt.Errorf("%w: nonce 不是合法的 base64url", ErrPasswordEnvelope)
	}
	if len(raw) != PasswordNonceSize {
		return "", fmt.Errorf("%w: nonce 长度不对", ErrPasswordEnvelope)
	}
	return CacheKeyPasswordNoncePrefix + hex.EncodeToString(raw), nil
}

func derivePasswordKey(shared, nonce []byte, username, kid string) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, shared, nonce, PasswordKDFInfo(username, kid), 32)
	if err != nil {
		return nil, fmt.Errorf("派生口令密钥: %w", err)
	}
	return key, nil
}

// newPasswordGCM 返回 nonce 长度固定的 AES-256-GCM。
//
// ⚠️ nonce 是 16 字节，不是 GCM 常见的 12 字节——契约写在 §4.1，
// 前后端必须一致，所以这里用 NewGCMWithNonceSize 固定住。
func newPasswordGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("创建 AES 密钥: %w", err)
	}
	return cipher.NewGCMWithNonceSize(block, PasswordNonceSize)
}

// EncryptPassword 用服务端公钥加密口令，生成一个信封。
//
// 这是**客户端侧**算法（服务端只解密）。放在服务端这里是为了让测试
// 能构造出合法信封来验证解密路径，避免手写信封格式写错却测不出来。
//
// ts 会被写进明文（服务端按 ±60s 窗口校验）。
func EncryptPassword(serverPub []byte, kid, username, password, oldPassword string, ts time.Time) (PasswordEnvelope, error) {
	remote, err := ecdh.X25519().NewPublicKey(serverPub)
	if err != nil {
		return PasswordEnvelope{}, fmt.Errorf("服务端公钥不合法: %w", err)
	}
	esk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return PasswordEnvelope{}, fmt.Errorf("生成临时密钥对: %w", err)
	}
	shared, err := esk.ECDH(remote)
	if err != nil {
		return PasswordEnvelope{}, fmt.Errorf("ECDH 失败: %w", err)
	}
	nonce := make([]byte, PasswordNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return PasswordEnvelope{}, fmt.Errorf("生成 nonce: %w", err)
	}
	key, err := derivePasswordKey(shared, nonce, username, kid)
	if err != nil {
		return PasswordEnvelope{}, err
	}
	aead, err := newPasswordGCM(key)
	if err != nil {
		return PasswordEnvelope{}, err
	}
	plaintext, err := json.Marshal(PasswordPayload{
		Username:    username,
		Password:    password,
		OldPassword: oldPassword,
		Ts:          ts.UnixMilli(),
	})
	if err != nil {
		return PasswordEnvelope{}, fmt.Errorf("序列化口令明文: %w", err)
	}
	defer clear(plaintext)
	ct := aead.Seal(nil, nonce, plaintext, nil)
	return PasswordEnvelope{
		Kid:   kid,
		Epk:   PasswordB64(esk.PublicKey().Bytes()),
		Nonce: PasswordB64(nonce),
		Ct:    PasswordB64(ct),
	}, nil
}

// DecryptPassword 解开一个口令信封。
//
// username 是**期望**的用户名：它参与 HKDF 的 info 派生，
// 密文因此与用户名绑定（换用户名 → 派生出的密钥不同 → GCM 认证失败）。
//
// now 用于校验明文里的 ts 是否落在 ±60s 窗口内。
//
// ⚠️ 任何失败都**不会**把原因带到返回值之外：调用方只能看到
// ErrPasswordEnvelope / ErrPasswordDecrypt / ErrPasswordTimestamp 三类。
func DecryptPassword(
	priv *ecdh.PrivateKey,
	kid string,
	env PasswordEnvelope,
	username string,
	now time.Time,
) (PasswordPayload, error) {
	if env.Kid == "" || env.Epk == "" || env.Nonce == "" || env.Ct == "" {
		return PasswordPayload{}, fmt.Errorf("%w: 字段缺失", ErrPasswordEnvelope)
	}
	if env.Kid != kid {
		// kid 未知：这个 kid 我们没有（多半是轮换期间的旧客户端）。
		return PasswordPayload{}, fmt.Errorf("%w: 未知 kid", ErrPasswordDecrypt)
	}
	epkBytes, err := PasswordUnB64(env.Epk)
	if err != nil {
		return PasswordPayload{}, err
	}
	if len(epkBytes) != 32 {
		return PasswordPayload{}, fmt.Errorf("%w: epk 长度不对", ErrPasswordEnvelope)
	}
	nonce, err := PasswordUnB64(env.Nonce)
	if err != nil {
		return PasswordPayload{}, err
	}
	if len(nonce) != PasswordNonceSize {
		return PasswordPayload{}, fmt.Errorf("%w: nonce 长度不对", ErrPasswordEnvelope)
	}
	ct, err := PasswordUnB64(env.Ct)
	if err != nil {
		return PasswordPayload{}, err
	}
	remote, err := ecdh.X25519().NewPublicKey(epkBytes)
	if err != nil {
		return PasswordPayload{}, fmt.Errorf("%w: epk 不是合法的 X25519 公钥", ErrPasswordEnvelope)
	}
	shared, err := priv.ECDH(remote)
	if err != nil {
		return PasswordPayload{}, fmt.Errorf("%w: ECDH 失败", ErrPasswordDecrypt)
	}
	key, err := derivePasswordKey(shared, nonce, username, kid)
	if err != nil {
		return PasswordPayload{}, err
	}
	aead, err := newPasswordGCM(key)
	if err != nil {
		return PasswordPayload{}, err
	}
	plaintext, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		// GCM 认证失败：密文被篡改 / 密钥不匹配 / 用户名不对。
		return PasswordPayload{}, fmt.Errorf("%w: GCM 认证失败", ErrPasswordDecrypt)
	}
	defer clear(plaintext)
	var payload PasswordPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return PasswordPayload{}, fmt.Errorf("%w: 明文不是合法 JSON", ErrPasswordEnvelope)
	}
	// 时间窗（§4.3 的第一层）：ts 超出 ±60s 直接拒。
	delta := now.UnixMilli() - payload.Ts
	if delta < -PasswordTimestampWindow.Milliseconds() || delta > PasswordTimestampWindow.Milliseconds() {
		return PasswordPayload{}, fmt.Errorf("%w: ts 超出 ±%s 窗口", ErrPasswordTimestamp, PasswordTimestampWindow)
	}
	return payload, nil
}
