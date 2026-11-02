// 口令传输（应用层加密）的服务端逻辑。
//
// 方案与契约见 docs/design/13-password-transport.md：
//
//   - §4.1 密码学方案（X25519 + HKDF-SHA256 + AES-256-GCM）
//   - §4.2 公钥分发（GET /api/v1/auth/pubkey，不缓存）
//   - §4.3 重放防护（ts ±60s + nonce 去重）
//   - §4.4 各接口的改法（登录 / 改密 / 建用户初始口令 / setup）
//   - §4.6 渐进启用（plaintext / optional / required）
//
// ⚠️ 对外**不区分**加密层面的失败原因：那等于给攻击者一个
// "我的密文对不对"的 oracle。只有 ts 与重放这两个**用户可行动**的原因
// 会单独提示。
package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
)

// 口令传输模式（password.mode）。
const (
	passwordModePlaintext = "plaintext"
	passwordModeOptional  = "optional"
	passwordModeRequired  = "required"
)

// passwordMode 返回当前生效的口令传输模式。
//
// 取值来源：设置快照（DB 覆盖层 + 配置基线，T0 热生效）→ 配置基线 →
// 文档默认值 plaintext。
//
// ⚠️ **非法值按 required 处理（fail-closed）**：
// 设置层已按 Enum 拦掉写入，这里防的是"手改库 / 迁移出错 / 配置被改坏"。
// 那种情况下回退到 plaintext 等于**静默降级一个安全控制**——
// 口令明文过网，而日志里只有一条 warn（AGENTS.md 规则 1 / 3）。
//
// 回退到 required 的代价是"登录暂时失败并报明确错误"——
// 这是看得见的失败，修好配置即可；比"悄悄降级成明文"安全得多。
func (a *App) passwordMode() string {
	raw := ""
	if a.SettingsSvc != nil {
		if snap := a.SettingsSvc.Snapshot(); snap != nil {
			raw = snap.Password.Mode
		}
	}
	if strings.TrimSpace(raw) == "" && a.Cfg != nil {
		raw = a.Cfg.Password.Mode
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case passwordModeOptional:
		return passwordModeOptional
	case passwordModeRequired:
		return passwordModeRequired
	case passwordModePlaintext:
		return passwordModePlaintext
	case "":
		// 没配置来源 → 用内置默认值（config.Default 里就是 optional）。
		//
		// ⚠️ 不能回退到 plaintext：那是**静默降级一个安全控制**——
		//    口令明文过网，而日志里只有一条 warn（AGENTS.md 规则 1 / 3）。
		//    optional 仍然接受明文（旧客户端不受影响），但前端会发密文。
		return passwordModeOptional
	default:
		// 非法值：按 required 处理，宁可拒登也不能悄悄降级成明文。
		//
		// ⚠️ 记 error 而不是 warn：这不是"提示一下"，
		//    是"一个安全控制失效了，登录当前处于 fail-closed 状态"。
		if a.Logger != nil {
			a.Logger.Error("未知的口令传输模式，按 required 处理（fail-closed），请修正配置",
				"mode", raw)
		}
		return passwordModeRequired
	}
}

// CheckPasswordTransport 是口令传输链路的**启动自检**。
//
// 返回错误 = 这个 App 不该上线：口令传输模式启用着，
// 但口令加密密钥库没注入。
//
// ⚠️ 为什么要在启动时检查，而不是等第一个请求：
//
//	密钥库缺失会让公钥接口**每一个请求**都 500——
//	服务看起来在跑，实际上加密登录链路整个是坏的。
//	启动就失败，比上线后才发现强得多（AGENTS.md 规则 1 / 3）。
//
// 判定口径与 handlePasswordPubKey 一致：
// 模式不是 plaintext 时就必须有密钥库。
func (a *App) CheckPasswordTransport() error {
	if a.passwordMode() == passwordModePlaintext {
		return nil
	}
	if a.PasswordKeys == nil {
		return errors.New("口令传输模式已启用，但未注入口令加密密钥库")
	}
	return nil
}

// 信封与闸门

// passwordEnvelope 是请求体里的应用层加密信封
// （字段名见 docs/design/13-password-transport.md §4.1）。
type passwordEnvelope struct {
	Kid   string `json:"kid"`
	Epk   string `json:"epk"`
	Nonce string `json:"nonce"`
	Ct    string `json:"ct"`
}

// present 判断信封里带的是不是密文：四个字段里**任意一个**非空就算。
func (e passwordEnvelope) present() bool {
	return e.Kid != "" || e.Epk != "" || e.Nonce != "" || e.Ct != ""
}

// 口令传输的内部错误。
//
// ⚠️ 这些错误**只用于服务端内部流转**；对外的文案见 failPassword。
var (
	// errPasswordCipherRejected：plaintext 模式收到密文。
	errPasswordCipherRejected = errors.New("当前未启用加密登录")
	// errPasswordPlaintextRejected：required 模式收到明文。
	errPasswordPlaintextRejected = errors.New("当前要求使用加密登录")
	// errPasswordEnvelopeInvalid：信封字段不合法或不完整。
	errPasswordEnvelopeInvalid = errors.New("加密登录数据格式不正确")
	// errPasswordDecryptFailed：解密失败（对外不区分具体原因）。
	errPasswordDecryptFailed = errors.New("口令解密失败")
	// errPasswordExpired：明文里的 ts 超出 ±60s 窗口。
	errPasswordExpired = errors.New("请求已过期")
	// errPasswordReplay：nonce 用过了（重放）。
	errPasswordReplay = errors.New("请求重复提交")
	// errPasswordKeyStoreMissing：服务端没初始化口令加密密钥库。
	errPasswordKeyStoreMissing = errors.New("口令加密未正确初始化")
	// errPasswordNonceUnavailable：重放防护不可用（缓存故障）。
	errPasswordNonceUnavailable = errors.New("重放防护不可用")
	// errPasswordUsernameMismatch：信封里的用户名与请求体的对不上。
	errPasswordUsernameMismatch = errors.New("用户名与加密数据不一致")
)

type passwordSecret struct {
	Username string
	Password string
	// OldPassword 只在改密场景有值。
	OldPassword string
}

// resolvePassword 按 password.mode 闸门解析口令。
//
// 模式语义（docs/design/13-password-transport.md §4.6）：
//
//	plaintext：只收明文；收到密文 → 400
//	optional ：两者都收，**优先解密**
//	required ：只收密文；收到明文 → 400
//
// 参数：
//
//   - env：请求体里的密文信封（明文模式为空）
//   - username：请求体里的用户名；加密模式下它同时是 HKDF 的绑定值
//   - plaintext / oldPlaintext：明文模式下的口令（改密有两个）
//   - wantOld：是否需要旧口令（改密场景）
func (a *App) resolvePassword(
	ctx context.Context,
	env passwordEnvelope,
	username, plaintext, oldPlaintext string,
	wantOld bool,
) (passwordSecret, error) {
	switch a.passwordMode() {
	case passwordModePlaintext:
		if env.present() {
			return passwordSecret{}, errPasswordCipherRejected
		}
		return passwordSecret{
			Username:    username,
			Password:    plaintext,
			OldPassword: oldPlaintext,
		}, nil
	case passwordModeRequired:
		if !env.present() {
			return passwordSecret{}, errPasswordPlaintextRejected
		}
	case passwordModeOptional:
		if !env.present() {
			return passwordSecret{
				Username:    username,
				Password:    plaintext,
				OldPassword: oldPlaintext,
			}, nil
		}
	}

	// 加密分支
	if username == "" {
		// 加密模式下用户名必须**明文**放在请求体里：
		// HKDF 的 info 绑定了它，服务端要先有用户名才能派生密钥。
		return passwordSecret{}, errPasswordEnvelopeInvalid
	}
	payload, err := a.decryptPasswordPayload(ctx, env, username)
	if err != nil {
		return passwordSecret{}, err
	}
	if wantOld && payload.OldPassword == "" {
		return passwordSecret{}, errPasswordEnvelopeInvalid
	}
	return passwordSecret{
		Username:    payload.Username,
		Password:    payload.Password,
		OldPassword: payload.OldPassword,
	}, nil
}

// decryptPasswordPayload 解封口令信封并完成全部校验：
// 解密 → 时间窗（§4.3 第一层）→ 用户名比对 → nonce 去重（§4.3 第二层）。
func (a *App) decryptPasswordPayload(
	ctx context.Context,
	env passwordEnvelope,
	username string,
) (crypto.PasswordPayload, error) {
	ks := a.PasswordKeys
	if ks == nil {
		// 服务端没初始化密钥库 —— 明确的配置错误，**不能**当明文放行。
		a.logPasswordError("口令加密密钥库未初始化", nil)
		return crypto.PasswordPayload{}, errPasswordKeyStoreMissing
	}
	payload, err := ks.Decrypt(
		crypto.PasswordEnvelope{Kid: env.Kid, Epk: env.Epk, Nonce: env.Nonce, Ct: env.Ct},
		username, time.Now(),
	)
	if err != nil {
		switch {
		case errors.Is(err, crypto.ErrPasswordTimestamp):
			return crypto.PasswordPayload{}, errPasswordExpired
		case errors.Is(err, crypto.ErrPasswordEnvelope):
			return crypto.PasswordPayload{}, errPasswordEnvelopeInvalid
		default:
			// 记原因（不含任何口令/明文），对外只说"解密失败"。
			a.logPasswordError("口令信封解密失败", err)
			return crypto.PasswordPayload{}, errPasswordDecryptFailed
		}
	}
	if payload.Username != username {
		a.logPasswordWarn("信封里的用户名与请求体不一致", "expected", username)
		return crypto.PasswordPayload{}, errPasswordUsernameMismatch
	}
	if err := a.claimPasswordNonce(ctx, env.Nonce); err != nil {
		return crypto.PasswordPayload{}, err
	}
	return payload, nil
}

// claimPasswordNonce 抢占 nonce（防重放，§4.3 第二层）。
//
// ⚠️ fail-closed：缓存不可用时**拒绝**该请求。
//
//	放行等于没有重放防护——"查不到"被当成"没用过"，
//	攻击者就能在 60s 窗口内无限重放。
func (a *App) claimPasswordNonce(ctx context.Context, nonceB64 string) error {
	key, err := crypto.PasswordNonceKey(nonceB64)
	if err != nil {
		return errPasswordEnvelopeInvalid
	}
	if a.Cache == nil {
		return errPasswordNonceUnavailable
	}
	c := a.Cache.Get()
	if c == nil {
		return errPasswordNonceUnavailable
	}
	// 先查后写：查得到 = 见过 → 重放。
	if _, ok, err := c.Get(ctx, key); err != nil {
		a.logPasswordError("读 nonce 缓存失败（重放防护不可用）", err)
		return errPasswordNonceUnavailable
	} else if ok {
		return errPasswordReplay
	}
	// SetNX 是原子的：并发下只有一个请求抢得到。
	ok, err := c.SetNX(ctx, key, []byte("1"), crypto.PasswordNonceTTL)
	if err != nil {
		a.logPasswordError("写 nonce 缓存失败（重放防护不可用）", err)
		return errPasswordNonceUnavailable
	}
	if !ok {
		return errPasswordReplay
	}
	return nil
}

// failPassword 把口令传输错误映射成对外文案——
// 对外**不区分**具体原因（那是给攻击者的 oracle）。
func (a *App) failPassword(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPasswordCipherRejected):
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "当前未启用加密登录")
	case errors.Is(err, errPasswordPlaintextRejected):
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "当前要求使用加密登录")
	case errors.Is(err, errPasswordExpired):
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "请求已过期，请重新提交")
	case errors.Is(err, errPasswordReplay):
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "请求重复提交，请重新提交")
	case errors.Is(err, errPasswordEnvelopeInvalid):
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "加密登录数据格式不正确")
	case errors.Is(err, errPasswordUsernameMismatch):
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户名与加密数据不一致")
	case errors.Is(err, errPasswordDecryptFailed):
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "口令解密失败，请重新提交")
	case errors.Is(err, errPasswordKeyStoreMissing):
		Fail(w, http.StatusInternalServerError, CodeInternal, "口令加密未正确初始化")
	case errors.Is(err, errPasswordNonceUnavailable):
		Fail(w, http.StatusServiceUnavailable, CodeUnavailable, "重放防护暂不可用，请稍后重试")
	default:
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "口令校验失败")
	}
}

// 公钥分发

// handlePasswordPubKey 返回口令加密公钥。
//
// 契约见 docs/design/13-password-transport.md §4.2：
//
//	{"keys":[{"kid":"pk-…","alg":"X25519-HKDF-SHA256-AESGCM","x25519_pub":"…"}],"enabled":true}
//
// ⚠️ 响应**不缓存**（Cache-Control: no-store）：轮换后客户端要立刻拿到新 kid。
//
// ⚠️ 这个端点是**公开**的（且不受 setup 拦截）：
//
//	setup 阶段前端就要用它加密管理员口令。
func (a *App) handlePasswordPubKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.passwordMode() == passwordModePlaintext {
		// 没启用加密：不发公钥（发了也会让人以为可以用）。
		OK(w, map[string]any{"keys": []map[string]string{}, "enabled": false})
		return
	}
	ks := a.PasswordKeys
	if ks == nil {
		// 模式要求加密，但密钥库没初始化 —— 明确的配置错误。
		// ⚠️ 不能返回 enabled=false：那会让前端降级成明文提交，
		//    然后被 required 拒掉，用户看到的是一个莫名其妙的 400。
		a.logPasswordError("口令加密模式已启用但密钥库未初始化", nil)
		Fail(w, http.StatusInternalServerError, CodeInternal, "口令加密未正确初始化")
		return
	}
	OK(w, map[string]any{
		"keys": []map[string]string{{
			"kid":        ks.KID(),
			"alg":        crypto.PasswordAlg,
			"x25519_pub": ks.PublicBase64(),
		}},
		"enabled": true,
	})
}

// 日志助手

// logPasswordWarn 记一条口令传输相关的告警。
//
// Logger 可能未注入（测试里），此时静默跳过——日志不是这条链路的主输出。
func (a *App) logPasswordWarn(msg string, args ...any) {
	if a.Logger == nil {
		return
	}
	a.Logger.Warn(msg, args...)
}

// logPasswordError 记一条口令传输相关的错误。
//
// ⚠️ 传入的 err **绝不能**包含口令或明文。
func (a *App) logPasswordError(msg string, err error) {
	if a.Logger == nil {
		return
	}
	if err != nil {
		a.Logger.Error(msg, "err", err)
		return
	}
	a.Logger.Error(msg)
}
