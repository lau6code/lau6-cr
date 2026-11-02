package service

import "context"

// clientIPKey 是客户端 IP 在 context 里的键。
//
// 为什么把 IP 放进 context：限流与登录退避都在**服务层**，
// 而 IP 只在 HTTP 层拿得到（请求对象）。用 context 传递，
// 就不用把 request 一路透传到服务层。
type clientIPKey struct{}

func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// 没有就是空串（调用方自己决定怎么处理：
// 限流按"空 IP"归到同一个桶，登录退避按"未知来源"计数）。
func ClientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}
