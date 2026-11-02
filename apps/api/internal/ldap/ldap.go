// Package ldap 实现 LDAP simple bind 认证与 RFC 4514 DN 转义。
package ldap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// 目录"连不上"和"口令不对"是**两件完全不同的事**，调用方必须能分开：
//
//	凭据无效（ErrInvalidCredentials）→ 这是用户没输对口令，算登录失败
//	目录不可达（ErrUnavailable）      → 这是服务故障，**不是**用户的错
//
// 分不开的后果很实在：目录侧一次网络抖动会被当成口令错，
// 连续失败累计到 access.login_fail_limit 就把账号连同来源 IP 一起锁死，
// 而用户看到的还是"口令错误"——运维会去查谁在暴力破解，真正的原因没人看。
var (
	// ErrInvalidCredentials 表示凭据无效（口令错 / 该 DN 不存在）。
	ErrInvalidCredentials = errors.New("ldap: 凭据无效")
	// ErrUnavailable 表示目录不可达（连不上 / 超时 / 服务器忙 / 协议层错误）。
	ErrUnavailable = errors.New("ldap: 目录不可达")
)

// Authenticator 是口令校验的抽象。
//
// 生产实现是 Client；测试用 Fake。
type Authenticator interface {
	// Bind 校验口令。成功返回 nil，失败返回错误。
	//
	// 失败时**必须**能用 errors.Is 区分出 ErrInvalidCredentials 与 ErrUnavailable。
	Bind(ctx context.Context, username, password string) error
}

// Config 是 LDAP 连接配置。
type Config struct {
	URL         string
	DC          string
	UserSegment string
	StartTLS    bool
	Timeout     time.Duration
}

// Client 是基于 go-ldap 的实现。
type Client struct {
	cfg Config
}

// New 构造客户端。
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Client{cfg: cfg}
}

// Bind 用 simple bind 校验口令。
func (c *Client) Bind(ctx context.Context, username, password string) error {
	// 空口令必须在客户端先拦：不少 LDAP 服务器对空口令会执行
	// "匿名 bind"并返回成功，那等于认证绕过。
	// （go-ldap 的 SimpleBind 自身也会拒，但双保险不依赖库的默认行为。）
	if password == "" {
		return fmt.Errorf("%w: 空口令", ErrInvalidCredentials)
	}
	dn, err := BuildBindDN(c.cfg.UserSegment, c.cfg.DC, username)
	if err != nil {
		return err
	}
	conn, err := c.dial()
	if err != nil {
		// 连不上 = 目录不可达，不是"口令错"。
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer conn.Close()

	// 不设置 AllowEmptyPassword（默认即 false，是安全侧），
	// 也不走 UnauthenticatedBind。
	if _, err := conn.SimpleBind(ldap.NewSimpleBindRequest(dn, password, nil)); err != nil {
		return classifyBindError(err)
	}
	return nil
}

// classifyBindError 把 bind 失败分成"凭据无效"与"目录不可达"两类。
//
// 判据是 LDAP 的 result code：
//
//	49 invalidCredentials / 32 noSuchObject / 48 inappropriateAuthentication
//	  → 凭据无效（用户没输对口令，或那个 DN 根本不存在）
//	其余一切（网络断了、超时、服务器忙、协议层错误）
//	  → 目录不可达
//
// ⚠️ 没有 result code 的（net.Error 之类）一律算"不可达"：
//
//	拿不准的时候宁可算故障——算成凭据无效会误伤用户的登录退避计数。
func classifyBindError(err error) error {
	switch {
	case ldap.IsErrorAnyOf(err,
		ldap.LDAPResultInvalidCredentials,
		ldap.LDAPResultNoSuchObject,
		ldap.LDAPResultInappropriateAuthentication,
	):
		return fmt.Errorf("%w: %w", ErrInvalidCredentials, err)
	default:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}

// Probe 建立连接（必要时升级 TLS），可选地做一次 simple bind。
//
// 供配置向导的「测试连接」使用：只验证"连得上、协议说得通"，
// 不查目录数据、不建影子账号。
//
//   - bindDN 为空   → 只测连接（TCP / TLS / StartTLS 握手）。
//   - bindDN 非空   → 额外用该 DN 与口令做一次 simple bind，
//     验证凭据确实可用（口令错、DN 不存在都会在这里暴露）。
//
// ⚠️ 超时由 cfg.Timeout 决定（dial 与 conn.SetTimeout 都用它）；
// ctx 只在连接前查一次取消状态——go-ldap 的连接过程不吃 ctx。
func (c *Client) Probe(ctx context.Context, bindDN, bindPassword string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	if bindDN == "" {
		return nil
	}
	if bindPassword == "" {
		return fmt.Errorf("ldap: 绑定口令不能为空")
	}
	if _, err := conn.SimpleBind(ldap.NewSimpleBindRequest(bindDN, bindPassword, nil)); err != nil {
		return fmt.Errorf("ldap bind 失败: %w", err)
	}
	return nil
}

func (c *Client) dial() (*ldap.Conn, error) {
	dialer := &net.Dialer{Timeout: c.cfg.Timeout}
	opts := []ldap.DialOpt{ldap.DialWithDialer(dialer)}
	if strings.HasPrefix(c.cfg.URL, "ldaps://") {
		opts = append(opts, ldap.DialWithTLSConfig(
			&tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostOf(c.cfg.URL)}))
	}
	conn, err := ldap.DialURL(c.cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("ldap 连接 %s: %w", c.cfg.URL, err)
	}
	conn.SetTimeout(c.cfg.Timeout)
	if !strings.HasPrefix(c.cfg.URL, "ldaps://") && c.cfg.StartTLS {
		if err := conn.StartTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: hostOf(c.cfg.URL),
		}); err != nil {
			conn.Close()
			return nil, fmt.Errorf("ldap starttls: %w", err)
		}
	}
	return conn, nil
}

// hostOf 从 URL 里取出主机名（用于 TLS SNI）。
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// BuildBindDN 按 RFC 4514 拼出用户 DN。
//
// userSegment 形如 "uid={0},ou=people"，{0} 会被替换成转义后的用户名；
// dc 形如 "dc=lau6code,dc=com"，可空。
func BuildBindDN(userSegment, dc, username string) (string, error) {
	if !strings.Contains(userSegment, "{0}") {
		return "", fmt.Errorf("ldap: user_segment 必须包含 {0} 占位符")
	}
	escaped := EscapeDNValue(username)
	dn := strings.Replace(userSegment, "{0}", escaped, 1)
	if dc = strings.TrimSpace(dc); dc != "" {
		dn += "," + dc
	}
	return dn, nil
}

// EscapeDNValue 按 RFC 4514 §2.4 转义一个属性值。
//
// 必须转义的字符：
//
//	, + " \ < > ; =        —— 特殊字符
//	#（仅在开头）
//	空格（仅在开头或结尾）    —— 否则会被服务器当 insignificant 剥掉，
//	                            导致实际绑定的 DN 与你以为的不是同一个条目
//	非 ASCII / 控制字符      —— 逐字节 hexpair
//
// 十六进制形式是 \XX（两个十六进制数字）。
func EscapeDNValue(v string) string {
	if v == "" {
		return ""
	}
	var b strings.Builder
	for i, r := range v {
		switch r {
		case ',', '+', '"', '\\', '<', '>', ';', '=':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '#':
			if i == 0 {
				b.WriteString("\\#")
			} else {
				b.WriteRune(r)
			}
		case ' ':
			// 前导或尾随空格必须转义，中间的不必。
			if i == 0 || i == len(v)-1 {
				b.WriteString("\\ ")
			} else {
				b.WriteByte(' ')
			}
		default:
			// 控制字符与非 ASCII 一律 hexpair（按 UTF-8 字节）。
			if r < 0x20 || r > 0x7e {
				for _, c := range []byte(string(r)) {
					fmt.Fprintf(&b, "\\%02X", c)
				}
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Fake 是测试用的假实现。
type Fake struct {
	// Users 是"用户名 → 口令"的映射；用户不存在时 bind 失败。
	Users map[string]string
	// Err 非 nil 时所有 bind 都返回该错误。
	//
	// 用来模拟"目录不可达"时，请直接传 ErrUnavailable——
	// 真实实现就是这样区分的，测试替身不能比真实现更宽松。
	Err error
}

// Bind 模拟校验。
func (f *Fake) Bind(_ context.Context, username, password string) error {
	if f.Err != nil {
		return f.Err
	}
	want, ok := f.Users[username]
	if !ok || want != password {
		return fmt.Errorf("%w: 用户不存在或口令错误", ErrInvalidCredentials)
	}
	return nil
}
