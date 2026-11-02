package crypto

import (
	"crypto/ecdh"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

// passwordEnvelopeVector 是前后端**共用**的协议向量。
//
// 前端用它守住自己的实现（scripts/check-password-crypto.mjs），
// 服务端用它守住解密路径——两边读同一个文件，改坏一边另一边立刻红。
//
//go:embed testdata/password_envelope.json
var passwordEnvelopeVector []byte

// passwordVector 是向量文件的结构。
type passwordVector struct {
	Alg              string `json:"alg"`
	Kid              string `json:"kid"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	Ts               int64  `json:"ts"`
	ServerPrivateHex string `json:"server_private_hex"`
	ServerPublicB64U string `json:"server_public_base64url"`
	Expected         struct {
		Epk   string `json:"epk"`
		Nonce string `json:"nonce"`
		Ct    string `json:"ct"`
	} `json:"expected"`
}

// TestPasswordDecryptsSharedVector 覆盖"前端按共享向量算出的信封能被服务端解开"。
//
// 这是与前端（apps/web/src/api/passwordCrypto.ts）的互操作测试：
// 前端改了 info 串、nonce 长度、base64 变体、明文 JSON 结构……
// 这里会立刻红。同一向量的另一半是 scripts/check-password-crypto.mjs。
func TestPasswordDecryptsSharedVector(t *testing.T) {
	var v passwordVector
	if err := json.Unmarshal(passwordEnvelopeVector, &v); err != nil {
		t.Fatalf("解析共享向量: %v", err)
	}
	if v.Alg != PasswordAlg {
		t.Fatalf("向量 alg = %q，期望 %q", v.Alg, PasswordAlg)
	}

	privBytes, err := hex.DecodeString(v.ServerPrivateHex)
	if err != nil {
		t.Fatalf("解码服务端私钥: %v", err)
	}
	priv, err := ecdh.X25519().NewPrivateKey(privBytes)
	if err != nil {
		t.Fatalf("构造服务端私钥: %v", err)
	}
	// 向量里的服务端公钥必须与私钥配对，否则前端是拿另一个公钥算的。
	if got := base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()); got != v.ServerPublicB64U {
		t.Fatalf("服务端公钥 = %q，期望 %q", got, v.ServerPublicB64U)
	}

	env := PasswordEnvelope{
		Kid:   v.Kid,
		Epk:   v.Expected.Epk,
		Nonce: v.Expected.Nonce,
		Ct:    v.Expected.Ct,
	}
	payload, err := DecryptPassword(priv, env.Kid, env, v.Username, time.UnixMilli(v.Ts))
	if err != nil {
		t.Fatalf("解密共享向量: %v", err)
	}
	if payload.Username != v.Username || payload.Password != v.Password {
		t.Fatalf("解密结果不对：%+v", payload)
	}
	if payload.Ts != v.Ts {
		t.Fatalf("ts = %d，期望 %d", payload.Ts, v.Ts)
	}
}
