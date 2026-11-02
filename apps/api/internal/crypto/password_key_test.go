package crypto

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPasswordKeyLocalUsesFile 覆盖 local 模式：文件是权威源。
//
// local 是进程内的，进程一没就没了——不能当"启动源"，
// 那等于每次重启都换一把钥匙（正在用旧 kid 的客户端会突然登不上）。
func TestPasswordKeyLocalUsesFile(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "password-key.pem")

	ks, err := NewPasswordKeyStore(PasswordKeyOptions{PrivatePath: privPath})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}
	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("私钥文件没写出来: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("私钥文件权限 = %v，期望 0600", info.Mode().Perm())
	}

	// 再建一次：必须拿到**同一把**钥匙。
	ks2, err := NewPasswordKeyStore(PasswordKeyOptions{PrivatePath: privPath})
	if err != nil {
		t.Fatalf("第二次构造: %v", err)
	}
	if ks.KID() != ks2.KID() {
		t.Fatalf("重启后密钥变了：%s → %s", ks.KID(), ks2.KID())
	}
	if ks.PublicBase64() != ks2.PublicBase64() {
		t.Fatalf("重启后公钥变了")
	}
}

// TestPasswordKeyRedisCacheIsSource 覆盖 redis 模式：缓存是权威源。
//
// §3.3：缓存里有 → 载入（**不读文件**）；没有 → 生成并写回缓存。
func TestPasswordKeyRedisCacheIsSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	privPath := filepath.Join(dir, "password-key.pem")
	c := newFakeRedis()

	ks, err := NewPasswordKeyStore(PasswordKeyOptions{PrivatePath: privPath, Cache: c})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}

	// 私钥必须在缓存里，且**不设 TTL**（它是启动源，不是热点数据）。
	raw, ok, err := c.Get(ctx, CacheKeyPasswordKey)
	if err != nil || !ok {
		t.Fatalf("缓存里没有口令加密密钥: ok=%v err=%v", ok, err)
	}
	if _, err := parsePasswordPrivateKey(raw, "缓存"); err != nil {
		t.Fatalf("缓存里的私钥解析失败: %v", err)
	}
	if ks.KID() == "" {
		t.Fatalf("kid 为空")
	}
}

// TestPasswordKeyRedisReadsFromCacheNotFile 覆盖"缓存里有就别读文件"。
//
// 文件里放一把**不一样**的钥匙，缓存里的那把才是权威的。
func TestPasswordKeyRedisReadsFromCacheNotFile(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "password-key.pem")
	c := newFakeRedis()

	first, err := NewPasswordKeyStore(PasswordKeyOptions{PrivatePath: privPath, Cache: c})
	if err != nil {
		t.Fatalf("首次构造: %v", err)
	}

	other, err := generatePasswordPrivateKey()
	if err != nil {
		t.Fatalf("生成对照密钥: %v", err)
	}
	pemBytes, err := encodePasswordPrivateKey(other)
	if err != nil {
		t.Fatalf("编码对照密钥: %v", err)
	}
	if err := os.WriteFile(privPath, pemBytes, 0o600); err != nil {
		t.Fatalf("改写文件: %v", err)
	}

	second, err := NewPasswordKeyStore(PasswordKeyOptions{PrivatePath: privPath, Cache: c})
	if err != nil {
		t.Fatalf("二次构造: %v", err)
	}
	if second.KID() != first.KID() {
		t.Fatalf("redis 模式下读了文件而不是缓存：%s → %s", first.KID(), second.KID())
	}
}

// TestPasswordKeyMissingPath 覆盖"没配私钥路径 → 报错"。
//
// 静默生成一把放内存里更糟：重启就没了，所有客户端的 kid 全变。
func TestPasswordKeyMissingPath(t *testing.T) {
	if _, err := NewPasswordKeyStore(PasswordKeyOptions{}); err == nil {
		t.Fatalf("没配私钥路径却构造成功了")
	}
}

// TestPasswordDecryptRoundTrip 覆盖"加密 → 解密 → 明文一致"。
func TestPasswordDecryptRoundTrip(t *testing.T) {
	ks := newTestPasswordKeyStore(t)
	pub := decodeBase64(t, ks.PublicBase64())

	env, err := EncryptPassword(pub, ks.KID(), "alice", "CorrectHorse123!", "", time.Now())
	if err != nil {
		t.Fatalf("加密: %v", err)
	}
	payload, err := ks.Decrypt(env, "alice", time.Now())
	if err != nil {
		t.Fatalf("解密: %v", err)
	}
	if payload.Username != "alice" || payload.Password != "CorrectHorse123!" {
		t.Fatalf("解密结果不对：%+v", payload)
	}
}

// TestPasswordDecryptRejectsStaleTimestamp 覆盖 ts 超出 ±60s 窗口被拒。
//
// 时间窗是重放防护的第一层（13-password-transport.md §4.3）。
func TestPasswordDecryptRejectsStaleTimestamp(t *testing.T) {
	ks := newTestPasswordKeyStore(t)
	pub := decodeBase64(t, ks.PublicBase64())

	old := time.Now().Add(-2 * time.Minute)
	env, err := EncryptPassword(pub, ks.KID(), "alice", "CorrectHorse123!", "", old)
	if err != nil {
		t.Fatalf("加密: %v", err)
	}
	if _, err := ks.Decrypt(env, "alice", time.Now()); err == nil {
		t.Fatalf("过期的时间戳被接受了")
	}
}

// TestPasswordDecryptAcceptsWindowEdge 覆盖窗口边缘仍然接受。
//
// 差 59 秒必须在窗口内——否则正常用户会被误拒。
func TestPasswordDecryptAcceptsWindowEdge(t *testing.T) {
	ks := newTestPasswordKeyStore(t)
	pub := decodeBase64(t, ks.PublicBase64())

	env, err := EncryptPassword(pub, ks.KID(), "alice", "CorrectHorse123!", "",
		time.Now().Add(-59*time.Second))
	if err != nil {
		t.Fatalf("加密: %v", err)
	}
	if _, err := ks.Decrypt(env, "alice", time.Now()); err != nil {
		t.Fatalf("窗口内的请求被拒了: %v", err)
	}
}

// TestPasswordDecryptRejectsUnknownKid 覆盖未知 kid 被拒。
func TestPasswordDecryptRejectsUnknownKid(t *testing.T) {
	ks := newTestPasswordKeyStore(t)
	pub := decodeBase64(t, ks.PublicBase64())

	env, err := EncryptPassword(pub, "pk-deadbeefdeadbeef", "alice", "CorrectHorse123!", "", time.Now())
	if err != nil {
		t.Fatalf("加密: %v", err)
	}
	if _, err := ks.Decrypt(env, "alice", time.Now()); err == nil {
		t.Fatalf("未知 kid 被接受了")
	}
}

// TestPasswordDecryptRejectsOtherUsername 覆盖"密文与用户名绑定"。
//
// HKDF 的 info 里带了用户名：换个用户名派生出的密钥不同 → GCM 认证失败。
func TestPasswordDecryptRejectsOtherUsername(t *testing.T) {
	ks := newTestPasswordKeyStore(t)
	pub := decodeBase64(t, ks.PublicBase64())

	env, err := EncryptPassword(pub, ks.KID(), "alice", "CorrectHorse123!", "", time.Now())
	if err != nil {
		t.Fatalf("加密: %v", err)
	}
	if _, err := ks.Decrypt(env, "bob", time.Now()); err == nil {
		t.Fatalf("密文被跨用户解密了")
	}
}

// TestPasswordDecryptRejectsTamperedCiphertext 覆盖密文被篡改后解密失败。
func TestPasswordDecryptRejectsTamperedCiphertext(t *testing.T) {
	ks := newTestPasswordKeyStore(t)
	pub := decodeBase64(t, ks.PublicBase64())

	env, err := EncryptPassword(pub, ks.KID(), "alice", "CorrectHorse123!", "", time.Now())
	if err != nil {
		t.Fatalf("加密: %v", err)
	}
	raw := decodeBase64(t, env.Ct)
	raw[0] ^= 0xff
	env.Ct = base64.RawURLEncoding.EncodeToString(raw)
	if _, err := ks.Decrypt(env, "alice", time.Now()); err == nil {
		t.Fatalf("被篡改的密文被接受了")
	}
}

// TestPasswordKDFInfoBindsUsername 覆盖 info 串绑定用户名与 kid。
func TestPasswordKDFInfoBindsUsername(t *testing.T) {
	got := PasswordKDFInfo("alice", "pk-123")
	want := "lau6-cr password v1 | alicepk-123"
	if got != want {
		t.Fatalf("info = %q，期望 %q", got, want)
	}
}

// TestPasswordNonceKeyRejectsBadNonce 覆盖非法 nonce 不给缓存键。
func TestPasswordNonceKeyRejectsBadNonce(t *testing.T) {
	if _, err := PasswordNonceKey("not-base64!!"); err == nil {
		t.Fatalf("非法 nonce 拿到了缓存键")
	}
	if _, err := PasswordNonceKey(base64.RawURLEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatalf("长度不对的 nonce 拿到了缓存键")
	}
	key, err := PasswordNonceKey(base64.RawURLEncoding.EncodeToString(make([]byte, PasswordNonceSize)))
	if err != nil {
		t.Fatalf("合法 nonce 拿不到缓存键: %v", err)
	}
	if key == "" || key[:len(CacheKeyPasswordNoncePrefix)] != CacheKeyPasswordNoncePrefix {
		t.Fatalf("缓存键前缀不对: %s", key)
	}
}

// TestPasswordDecryptsFrontendEnvelope 覆盖"前端（纯 JS X25519）产出的信封能被解开"。
//
// 这是一条**互操作**测试：向量由前端的实现生成（apps/web/src/api/passwordCrypto.ts），
// 固定了服务端私钥（RFC 7748 §6.1 的 alice 私钥）、临时密钥与 ts。
// 前端改了算法（info 串、nonce 长度、base64 变体……）时，这条测试会立刻红。
func TestPasswordDecryptsFrontendEnvelope(t *testing.T) {
	privBytes, err := hex.DecodeString("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	if err != nil {
		t.Fatalf("解码服务端私钥: %v", err)
	}
	priv, err := ecdh.X25519().NewPrivateKey(privBytes)
	if err != nil {
		t.Fatalf("构造服务端私钥: %v", err)
	}
	wantPub := "hSDwCYkwp1R0i33ctD73Wg2_Og0mOBr066SpjqqbTmo"
	if got := base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()); got != wantPub {
		t.Fatalf("服务端公钥 = %q，期望 %q", got, wantPub)
	}

	env := PasswordEnvelope{
		Kid:   "pk-fixture00000000",
		Epk:   "3p7bfXt9wbTTW2HC7OQ1Nz-DQ8hbeGdNrfx-FG-IK08",
		Nonce: "5fzueso_j9oe9fgg1P9C4Q",
		Ct:    "iOtqJUeEftPSOKS3y6fh5d47EabHft5cnGwJ5GnV6fNwkO9Iz-01W92FVyssCSvug__m1fX2EKnDiVynd8Ci5Gt4yc_qQ_Hc89qE_OoJofF7Vt2J2A",
	}
	// 向量里的 ts 是 1767225600000（2026-01-01T00:00:00Z）。
	payload, err := DecryptPassword(priv, env.Kid, env, "alice", time.UnixMilli(1767225600000))
	if err != nil {
		t.Fatalf("解密前端信封: %v", err)
	}
	if payload.Username != "alice" || payload.Password != "CorrectHorse123!" {
		t.Fatalf("解密结果不对：%+v", payload)
	}
	if payload.Ts != 1767225600000 {
		t.Fatalf("ts = %d，期望 1767225600000", payload.Ts)
	}
}

// newTestPasswordKeyStore 造一个只落在临时目录里的口令加密密钥库。
func newTestPasswordKeyStore(t *testing.T) *PasswordKeyStore {
	t.Helper()
	ks, err := NewPasswordKeyStore(PasswordKeyOptions{
		PrivatePath: filepath.Join(t.TempDir(), "password-key.pem"),
	})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}
	return ks
}

// decodeBase64 解 base64url（测试助手）。
func decodeBase64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("解码 %q: %v", s, err)
	}
	return b
}
