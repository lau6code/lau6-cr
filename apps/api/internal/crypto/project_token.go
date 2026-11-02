package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// ProjectTokenPrefix 是项目访问令牌的前缀。
//
// 分派依据：认证路径先看前缀决定查哪张表——
// `lau6_u_` 查 personal_tokens，`lau6_p_` 查 project_tokens。
// 两个前缀互不为前缀，一次判断就能分清。
const ProjectTokenPrefix = "lau6_p_"

// GenerateProjectToken 生成一枚项目访问令牌。
//
// 返回值：
//   - raw：原始令牌。**只此一次**——之后既取不回来，也没法从库里还原。
//   - hash：散列（SHA-256 十六进制），入库的就是它。
//   - prefix：展示用前缀（ProjectTokenPrefix + 前 6 位），用来在列表里认人。
//
// 熵：32 字节随机数（256 bit），base64url 编码。
func GenerateProjectToken() (raw, hash, prefix string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("生成项目令牌: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(b)
	raw = ProjectTokenPrefix + encoded
	hash = HashProjectToken(raw)
	// 展示前缀取前 6 位：够区分，又不至于把令牌本体泄出去。
	prefix = ProjectTokenPrefix + encoded[:6]
	return raw, hash, prefix, nil
}

// HashProjectToken 计算项目令牌的散列。
//
// 入库与查找都用它——原始令牌不进数据库。
func HashProjectToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// IsProjectToken 判断字符串是否像一枚项目令牌。
func IsProjectToken(raw string) bool {
	return strings.HasPrefix(raw, ProjectTokenPrefix)
}

// IsLegacyPersonalToken 判断字符串是否用了历史版本的个人令牌前缀。
//
// 命中它的凭据**一律不可用**——散列是原子的前像，改前缀就一定改散列，
// 库里查不到。识别它是为了给出"前缀已变更"的明确原因，
// 而不是笼统的"令牌无效"。
func IsLegacyPersonalToken(raw string) bool {
	return strings.HasPrefix(raw, LegacyPATPrefix)
}
