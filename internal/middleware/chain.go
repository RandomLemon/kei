package middleware

import (
	"slices"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Chain 把多个中间件按洋葱模型组合为单个中间件。
//
// mws[0] 位于最外层：它最先执行、最后返回；mws 为空时返回透传中间件。
// 入参会先复制，调用方后续修改切片不会影响已构造的链。
func Chain(mws ...bot.Middleware) bot.Middleware {
	// 复制一层，避免调用方复用底层数组造成数据竞态。
	chain := slices.Clone(mws)
	return func(next bot.Handler) bot.Handler {
		// 从最内层向外包裹：倒序遍历后 chain[0] 成为最外层。
		for i := range slices.Backward(chain) {
			if chain[i] == nil {
				// nil 中间件按透传处理，方便按开关拼接链。
				continue
			}
			next = chain[i](next)
		}
		return next
	}
}

// Apply 把中间件链包裹在 handler 外层，返回可直接注册的处理函数。
//
// 等价于 Chain(mws...)(h)。
func Apply(h bot.Handler, mws ...bot.Middleware) bot.Handler {
	return Chain(mws...)(h)
}
