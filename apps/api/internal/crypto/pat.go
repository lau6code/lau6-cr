package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// PATPrefix 是个人访问令牌（PAT）的前缀。
//
// 前缀的作用是**分派**：认证路径先看前缀决定走 PAT 还是 JWT，
// 不用把两种凭据都试一遍——也不会拿一枚 JWT 去查散列表。
//
// 与项目令牌的前缀（`lau6_p_`）互不为前缀，一次判断就能分清两类凭据。
const PATPrefix = "lau6_u_"

// LegacyPATPrefix 是历史版本的个人令牌前缀。
//
// 它**不再被识别**：携带该前缀的凭据一律认证失败。
//
// 常量保留下来只为识别"这枚凭据用了过期格式"这一种情况——
// 认证路径命中它时必须报明确原因（见 service.ErrLegacyTokenPrefix），
// 而不是笼统的"令牌无效"。
const LegacyPATPrefix = "cr_pat_"

// GeneratePersonalToken 生成一枚个人访问令牌。
//
// 返回值：
//   - raw：原始令牌。**只此一次**——之后既取不回来，也没法从库里还原。
//   - hash：散列（SHA-256 十六进制），入库的就是它。
//   - prefix：展示用前缀（PATPrefix + 前 6 位），用来在列表里认人。
//
// 熵：32 字节随机数（256 bit），base64url 编码。
func GeneratePersonalToken() (raw, hash, prefix string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("生成个人令牌: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(b)
	raw = PATPrefix + encoded
	hash = HashPersonalToken(raw)
	// 展示前缀取前 6 位：够区分，又不至于把令牌本体泄出去。
	prefix = PATPrefix + encoded[:6]
	return raw, hash, prefix, nil
}

// HashPersonalToken 计算个人令牌的散列。
//
// 入库与查找都用它——原始令牌不进数据库。
func HashPersonalToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// IsPersonalToken 判断字符串是否像一枚个人令牌。
func IsPersonalToken(raw string) bool {
	return strings.HasPrefix(raw, PATPrefix)
}
