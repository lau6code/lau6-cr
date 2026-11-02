package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/ldap"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// ErrInvalidProbeParam 表示连通性测试的**入参**不合法（缺 URL、驱动不支持等）。
//
// 它与"连不上"是两回事：前者是 400，后者是 503。
// 两者必须分开报——否则用户分不清是自己填错了，还是服务真的没起来。
//
// ⚠️ 错误文本要直接面向用户看：handler 会把 err.Error() 原样回给前端，
// 所以这里不能留英文哨兵名。
var ErrInvalidProbeParam = errors.New("连通性测试参数不合法")

// ErrCurrentDatabaseUnreadable 表示**当前生效的**数据库连接串反解不出来。
//
// ⚠️ 这不是用户填错了参数，是**服务端自己的配置坏了**
// （config.validate 只检查 DSN 非空，不解析它）。
//
// 必须与 ErrInvalidProbeParam 分开：归到参数错误会让用户去改自己填的表单，
// 而真正要改的是服务端配置——指向错误的方向比不报还糟。
var ErrCurrentDatabaseUnreadable = errors.New("当前生效的数据库连接无法解析")

// connectivityProbeTimeout 是连通性测试的默认超时。
//
// 5 秒：够穿过正常的跨机房网络，又不至于让页面干等太久。
const connectivityProbeTimeout = 5 * time.Second

// DatabaseTestInput 是数据库连通性测试的入参。
//
// ⚠️ 这里**不包含** bootstrap_token 之外的任何凭据——
// 口令只在内存里转发给驱动，不落库、不写日志。
type DatabaseTestInput struct {
	BootstrapToken string        `json:"bootstrap_token"`
	Database       DatabaseInput `json:"database"`
}

// LDAPTestInput 是 LDAP 连通性测试的入参。
//
// Username / Password 可选：填了就按 user_segment + dc 拼出 DN 做一次
// simple bind（验证完整登录路径）；不填只测连接。
type LDAPTestInput struct {
	BootstrapToken string `json:"bootstrap_token"`
	URL            string `json:"url"`
	DC             string `json:"dc"`
	UserSegment    string `json:"user_segment"`
	StartTLS       bool   `json:"start_tls"`
	TimeoutSec     int    `json:"timeout_sec"`
	Username       string `json:"username"`
	Password       string `json:"password"`
}

// ConnectivityResult 是连通性测试的结果。
type ConnectivityResult struct {
	// Target 是被测目标（DSN / URL），回显给用户确认测的是哪个。
	Target string `json:"target"`
	// LatencyMS 是握手 + ping 往返耗时（毫秒）。
	LatencyMS int64 `json:"latency_ms"`
	// Detail 是人类可读的结果说明。
	Detail string `json:"detail"`
	// Schema 是**实际生效**的 schema（postgres 的 search_path / mysql 的当前库）。
	Schema string `json:"schema,omitempty"`
}

// requireSetupPhase 校验「实例未初始化」+「引导令牌」。
//
// 连通性测试端点在 setup 阶段是**开放面**：那时还没有任何账号，
// 登录态护不住它。所以这里把口子收到最小：
//
//   - 实例已初始化 → 一律拒绝（ErrSetupAlreadyDone）。
//     否则这个"能连任意目标的探测接口"会长期留在生产实例上（SSRF 面）。
//   - 库里已有令牌摘要（setup reset 之后）→ 必须匹配该令牌。
//   - 首次初始化（库里没有摘要）→ 只校验格式。
//     令牌本来就由客户端生成（见 08-setup.md §4），这一条挡不住主动攻击，
//     真正的防线是"初始化期间别把端口暴露出去"（§4.6）。
func (s *SetupService) requireSetupPhase(ctx context.Context, token string) error {
	initialized, err := s.isInitialized(ctx)
	if err != nil {
		return err
	}
	if initialized {
		return ErrSetupAlreadyDone
	}
	tok := strings.TrimSpace(token)
	stored, err := s.StoredBootstrapToken(ctx)
	if err != nil {
		return err
	}
	if stored != "" {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(hashBootstrapToken(tok))) != 1 {
			return ErrBadBootstrapToken
		}
		return nil
	}
	if err := validateBootstrapToken(tok); err != nil {
		// 包一层哨兵错误，让 handler 能把它和"连不上"分开报：
		// 格式不对是 400，不是 503。
		return fmt.Errorf("%w: %s", ErrBadBootstrapTokenFormat, err.Error())
	}
	return nil
}

// TestDatabase 测试数据库连通性。
//
// 只连、只 ping、只查当前 schema：不跑迁移、不建表、不写数据。
// 失败时返回驱动原始错误（连接被拒 / 认证失败 / 库不存在……），
// 不做任何兜底或默认值。
func (s *SetupService) TestDatabase(ctx context.Context, in DatabaseTestInput) (*ConnectivityResult, error) {
	if err := s.requireSetupPhase(ctx, in.BootstrapToken); err != nil {
		return nil, err
	}
	res, ok, err := s.resolveDatabaseTarget(in.Database)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: 没有填写数据库连接信息", ErrInvalidProbeParam)
	}

	// 先看文件在不在——探测本身会把 SQLite 文件建出来，
	// 探完再查就永远是"已存在"了。
	sqliteExisted := true
	if res.target.Driver == "sqlite" {
		if _, statErr := os.Stat(res.target.SQLitePath); statErr != nil {
			sqliteExisted = false
		}
	}

	ctx, cancel := context.WithTimeout(ctx, connectivityProbeTimeout)
	defer cancel()
	start := time.Now()
	schema, err := store.Probe(ctx, res.target.Driver, res.dsn, res.probeSchema())
	if err != nil {
		return nil, err
	}
	latency := time.Since(start).Milliseconds()

	detail := "连接成功"
	if res.target.Driver == "sqlite" && !sqliteExisted {
		detail = "连接成功（数据库文件已创建）"
	}
	if res.PasswordSource == "current" {
		detail += "（口令留空，沿用了当前生效配置里的口令）"
	}
	return &ConnectivityResult{
		// ⚠️ 回显的连接串必须脱敏——它含数据库口令。
		Target:    store.RedactDSN(res.target.Driver, res.dsn),
		LatencyMS: latency,
		Detail:    detail,
		Schema:    schema,
	}, nil
}

// TestLDAP 测试 LDAP 连通性。
//
// 连不上 / 绑不上就返回原始错误，不兜底成"看起来成功"。
func (s *SetupService) TestLDAP(ctx context.Context, in LDAPTestInput) (*ConnectivityResult, error) {
	if err := s.requireSetupPhase(ctx, in.BootstrapToken); err != nil {
		return nil, err
	}
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" {
		return nil, fmt.Errorf("%w: ldap url 不能为空", ErrInvalidProbeParam)
	}
	if !strings.HasPrefix(rawURL, "ldap://") && !strings.HasPrefix(rawURL, "ldaps://") {
		return nil, fmt.Errorf("%w: ldap url 必须以 ldap:// 或 ldaps:// 开头", ErrInvalidProbeParam)
	}
	timeout := time.Duration(in.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = connectivityProbeTimeout
	}
	client := ldap.New(ldap.Config{
		URL:         rawURL,
		DC:          strings.TrimSpace(in.DC),
		UserSegment: strings.TrimSpace(in.UserSegment),
		StartTLS:    in.StartTLS,
		Timeout:     timeout,
	})

	// 填了账号就按模板拼 DN，多做一次 simple bind——
	// 这是"配完能不能真登录"的最强信号。
	bindDN := ""
	if username := strings.TrimSpace(in.Username); username != "" {
		dn, err := ldap.BuildBindDN(strings.TrimSpace(in.UserSegment), strings.TrimSpace(in.DC), username)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidProbeParam, err.Error())
		}
		bindDN = dn
	}

	start := time.Now()
	if err := client.Probe(ctx, bindDN, in.Password); err != nil {
		return nil, err
	}
	latency := time.Since(start).Milliseconds()

	detail := "连接成功"
	if bindDN != "" {
		detail = "连接成功，绑定成功"
	}
	return &ConnectivityResult{Target: rawURL, LatencyMS: latency, Detail: detail}, nil
}
