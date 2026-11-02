package gc

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Barrier 是 GC 期间的写屏障。
//
// 官方 `registry garbage-collect` 是 stop-the-world 的：
// 它要求 registry 处于只读或完全停止状态，否则"上传中的镜像层可能被误删"。
//
// lau6-cr 是 distribution 的**唯一入口**（compose 里 registry 不对外暴露端口），
// 所以"停写"这件事可以由 lau6-cr 自己做：
//
//	Block() → 关上写入口 + 等在途写请求排空 → GC 扫盘 → release()
//
// 只读路径（pull）不受影响，GC 期间照常服务。
type Barrier struct {
	mu sync.Mutex
	// blocked 为 true 时新写请求一律被拒。
	blocked bool
	// inflight 是当前在途的写请求数。
	inflight int
}

// ErrBarrierBusy 表示已有一次 GC 占着屏障。
var ErrBarrierBusy = fmt.Errorf("已有 blob 回收在运行")

// NewBarrier 构造写屏障。
func NewBarrier() *Barrier {
	return &Barrier{}
}

// 返回 false 表示屏障已关闭——调用方**必须**拒绝这个请求，
// 不能放它继续写。
func (b *Barrier) Enter() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blocked {
		return false
	}
	b.inflight++
	return true
}

func (b *Barrier) Leave() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inflight > 0 {
		b.inflight--
	}
}

func (b *Barrier) Blocked() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.blocked
}

// Block 关上写入口并等待在途写请求排空。
//
// 返回的 release 必须被调用（否则写入口永远关着）。
// 等待超时会**自动开闸**并返回错误——宁可 GC 不跑，也不能把 registry 卡死。
func (b *Barrier) Block(ctx context.Context, timeout time.Duration) (func(), error) {
	b.mu.Lock()
	if b.blocked {
		b.mu.Unlock()
		return nil, ErrBarrierBusy
	}
	b.blocked = true
	b.mu.Unlock()

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		b.mu.Lock()
		n := b.inflight
		b.mu.Unlock()
		if n == 0 {
			var once sync.Once
			return func() { once.Do(b.unblock) }, nil
		}
		if err := ctx.Err(); err != nil {
			b.unblock()
			return nil, fmt.Errorf("等待在途写入排空被取消（仍有 %d 个请求在途）: %w", n, err)
		}
		if !time.Now().Before(deadline) {
			b.unblock()
			return nil, fmt.Errorf("等待在途写入排空超时（%s，仍有 %d 个请求在途）", timeout, n)
		}
		select {
		case <-ctx.Done():
			b.unblock()
			return nil, fmt.Errorf("等待在途写入排空被取消: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (b *Barrier) unblock() {
	b.mu.Lock()
	b.blocked = false
	b.mu.Unlock()
}
