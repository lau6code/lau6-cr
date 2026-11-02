package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
)

// ErrLoginLocked 表示账号或来源 IP 正处在登录锁定期。
//
// ⚠️ 它**不能**被当成"口令错误"以外的任何东西暴露给调用方：
// 被锁定时的响应必须与"口令错"完全相同（状态码 + 文案），
// 否则就成了"这个账号是否存在"的探测接口
// （`13-password-transport.md` §5.4）。
var ErrLoginLocked = errors.New("登录已被锁定")

// loginBackoffBase 是指数退避的起始时长。
//
// 触发锁定后的第一次失败锁 60s，之后每多失败一次翻倍，
// 直到 login_lock_seconds 为止（那是上限，不是固定时长）。
const loginBackoffBase = 60 * time.Second

// loginFailState 是登录失败计数的状态。
type loginFailState struct {
	// Failures 是连续失败次数（成功登录会清零）。
	Failures int `json:"failures"`
	// LockedUntil 是锁定期截止的 unix 秒（0 = 未锁）。
	LockedUntil int64 `json:"locked_until"`
}

// LoginGuard 记录连续登录失败，达到阈值后锁定。
//
// **双维度**计数：
//   - 账号维度：同一个账号连续失败 → 锁定（挡定向爆破）
//   - 来源维度：同一个来源 IP 连续失败 → 锁定（挡喷洒式撞库）
//
// 计数存在缓存后端里：redis 模式下多副本共享
// （local 模式下每个副本各算各的，`12-cache.md` §7）。
type LoginGuard struct {
	cache     func() cache.Cache
	failLimit func() int
	lockFor   func() time.Duration
	logger    *slog.Logger
}

// NewLoginGuard 构造登录退避守卫。
//
// failLimit 是连续失败多少次后开始锁定（T0 热生效）。
// lockFor 是锁定的**最长**时长（退避按失败次数指数增长）。
func NewLoginGuard(
	c func() cache.Cache,
	failLimit func() int,
	lockFor func() time.Duration,
	logger *slog.Logger,
) *LoginGuard {
	return &LoginGuard{cache: c, failLimit: failLimit, lockFor: lockFor, logger: logger}
}

// ⚠️ 缓存不可用时**不拦**（返回 nil）但要记 error 日志：
//
//	缓存挂了就把所有登录都拒掉，那是更大的事故；
//	但"退避没生效"这件事必须留痕，不能悄悄过去。
//
// ⚠️ 降级语义与 RecordFailure / RecordSuccess 一致：
// 某个维度读不到就跳过它、继续看另一个维度，
// 并把错误记下来——不是"整个检查都不做"。
func (g *LoginGuard) Check(ctx context.Context, username, ip string) error {
	if g == nil || g.cache == nil {
		return nil
	}
	limit := g.failLimit()
	if limit <= 0 {
		return nil
	}
	now := time.Now().Unix()
	for _, key := range g.keys(username, ip) {
		state, err := g.read(ctx, key)
		if err != nil {
			if g.logger != nil {
				g.logger.Error("读登录失败计数失败", "err", err)
			}
			continue
		}
		if state.LockedUntil > now {
			return ErrLoginLocked
		}
	}
	return nil
}

// ⚠️ 缓存不可用时的降级语义与 Check 一致：
// 某个维度的计数**记不上**时，继续处理另一个维度，
// 而不是整个循环中止——账号维度读失败时，IP 维度照样该记，
// 否则换一个来源就能绕过退避。
//
// 记不上的那部分用 error 返回，由调用方留痕（不静默吞掉）。
func (g *LoginGuard) RecordFailure(ctx context.Context, username, ip string) error {
	if g == nil || g.cache == nil {
		return nil
	}
	limit := g.failLimit()
	if limit <= 0 {
		return nil
	}
	now := time.Now()
	var errs []error
	for _, key := range g.keys(username, ip) {
		state, err := g.read(ctx, key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		state.Failures++
		if state.Failures >= limit {
			state.LockedUntil = now.Add(g.backoff(state.Failures, limit)).Unix()
		}
		if err := g.write(ctx, key, state); err != nil {
			errs = append(errs, err)
			continue
		}
	}
	return errors.Join(errs...)
}

// 登录成功说明之前那些是手滑，不该继续算。
//
// ⚠️ 与 RecordFailure 同一条降级语义：
// 某个维度删不掉时继续处理另一个维度，最后把错误一起返回。
// 残留的计数会导致"明明登录成功了却还被锁着"，必须留痕。
func (g *LoginGuard) RecordSuccess(ctx context.Context, username, ip string) error {
	if g == nil || g.cache == nil {
		return nil
	}
	var errs []error
	for _, key := range g.keys(username, ip) {
		if err := g.cached().Delete(ctx, key); err != nil {
			errs = append(errs, fmt.Errorf("清登录失败计数失败: %w", err))
			continue
		}
	}
	return errors.Join(errs...)
}

// backoff 计算锁定时长：指数退避，上限为 lockFor。
func (g *LoginGuard) backoff(failures, limit int) time.Duration {
	cap := g.lockFor()
	if cap <= 0 {
		return loginBackoffBase
	}
	d := loginBackoffBase
	for i := limit; i < failures; i++ {
		d *= 2
		if d >= cap {
			return cap
		}
	}
	if d > cap {
		return cap
	}
	return d
}

// keys 返回账号维度与来源维度两个计数键。
//
// ⚠️ 键里**不放明文账号名**：Redis 的键是运维可见的，
// 账号名属于个人信息。改成 sha256 摘要。
func (g *LoginGuard) keys(username, ip string) []string {
	return []string{
		"cr:login-fail:acct:" + hashKey("acct", username),
		"cr:login-fail:ip:" + hashKey("ip", ip),
	}
}

func (g *LoginGuard) read(ctx context.Context, key string) (loginFailState, error) {
	raw, ok, err := g.cached().Get(ctx, key)
	if err != nil {
		return loginFailState{}, fmt.Errorf("读登录失败计数失败: %w", err)
	}
	if !ok {
		return loginFailState{}, nil
	}
	var state loginFailState
	if err := json.Unmarshal(raw, &state); err != nil {
		// 计数存坏了 → 当作 0 次（重新开始计数），但不能装作没事发生。
		if g.logger != nil {
			g.logger.Error("登录失败计数解析失败，按 0 次处理", "key", key, "err", err)
		}
		return loginFailState{}, nil
	}
	return state, nil
}

func (g *LoginGuard) write(ctx context.Context, key string, state loginFailState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("序列化登录失败计数失败: %w", err)
	}
	ttl := g.lockFor() * 2
	if ttl < time.Hour {
		ttl = time.Hour
	}
	if err := g.cached().Set(ctx, key, raw, ttl); err != nil {
		return fmt.Errorf("写登录失败计数失败: %w", err)
	}
	return nil
}

func (g *LoginGuard) cached() cache.Cache {
	return g.cache()
}

// hashKey 生成计数键里的摘要部分。
func hashKey(dim, value string) string {
	sum := sha256.Sum256([]byte(dim + "\x00" + value))
	return hex.EncodeToString(sum[:16])
}
