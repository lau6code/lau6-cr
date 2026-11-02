package ldap

import (
	"context"
	"sync"
)

// Dynamic 是"配置改了立即生效"的 LDAP 客户端。
//
// ldap.* 这几项在设置页上的级别是 T0（热生效），
// 文档 14-settings.md §3.4 明确写着"切换后立即影响登录"——
// 如果客户端在启动时构造好就冻住，那这个 T0 就是个假开关：
// 设置页能改、能显示"已保存"，运行期却还在用旧配置登录。
//
// 它按需重建：配置没变就复用旧客户端，变了就换一个。
// 并发安全：多个登录请求同时撞上重建时只有一个真正重建，其余复用结果。
type Dynamic struct {
	mu     sync.Mutex
	get    func() Config
	cur    Config
	client *Client
	built  bool
}

// NewDynamic 构造动态客户端。get 返回**当前生效**的配置。
//
// get 必须每次都读最新值（典型实现是读设置快照），
// 不能自己缓存——否则热生效就失效了。
func NewDynamic(get func() Config) *Dynamic {
	return &Dynamic{get: get}
}

// Client 返回当前配置对应的客户端；配置变了就重建。
func (d *Dynamic) Client() *Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.get()
	if !d.built || cfg != d.cur {
		d.cur = cfg
		d.client = New(cfg)
		d.built = true
	}
	return d.client
}

// Bind 用当前配置的客户端校验口令。
//
// 它实现 Authenticator——认证服务拿到的就是这个"会跟着设置走"的客户端。
func (d *Dynamic) Bind(ctx context.Context, username, password string) error {
	return d.Client().Bind(ctx, username, password)
}

// Probe 用当前配置测连接（设置向导的"测试连接"用）。
func (d *Dynamic) Probe(ctx context.Context, bindDN, bindPassword string) error {
	return d.Client().Probe(ctx, bindDN, bindPassword)
}
