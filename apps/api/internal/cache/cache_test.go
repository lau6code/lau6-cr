package cache

import "testing"

// TestHolderSwap 覆盖热切换的指针语义。
//
// 热切换的关键是：换指针不能影响已经在跑的请求——
// 它们手里拿的还是旧后端，所以旧后端不能立刻关。
func TestHolderSwap(t *testing.T) {
	first := NewLocal(10)
	second := NewLocal(10)

	h := NewHolder(first)
	if got := h.Get(); got != Cache(first) {
		t.Fatalf("初始后端不对：%v", got)
	}

	old := h.Swap(second)
	if old != Cache(first) {
		t.Fatal("Swap 应返回旧后端，供调用方择时关闭")
	}
	if got := h.Get(); got != Cache(second) {
		t.Fatalf("切换后应指向新后端：%v", got)
	}
}

// TestNewRedisRequiresAddr 保证 driver=redis 时必须给地址。
//
// 这是设置层那条跨字段校验的兜底——两层都拦，才不会出现
// "改了设置，重启时服务起不来"。
func TestNewRedisRequiresAddr(t *testing.T) {
	if _, err := New(Options{Driver: "redis"}); err == nil {
		t.Fatal("driver=redis 缺 addr 时应当报错")
	}
	if _, err := New(Options{Driver: "redis", Redis: RedisOptions{Addr: "127.0.0.1:6379"}}); err != nil {
		t.Fatalf("driver=redis 带 addr 时不应报错: %v", err)
	}
}
