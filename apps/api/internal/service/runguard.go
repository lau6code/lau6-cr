package service

import "sync"

// runGuard 是"同一时刻只允许一次执行"的互斥闸门。
//
// 清理（保留策略）与 blob 回收都需要它：两者都是破坏性操作，
// 并发跑会互相踩对方的执行记录与存储扫描。
type runGuard struct {
	mu      sync.Mutex
	running bool
}

func (g *runGuard) acquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return false
	}
	g.running = true
	return true
}

func (g *runGuard) release() {
	g.mu.Lock()
	g.running = false
	g.mu.Unlock()
}
