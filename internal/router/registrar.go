package router

import (
	"fmt"
	"regexp"

	"github.com/RandomLemon/kei/pkg/bot"
)

// 规则类别，用于生成 plugin:kind:index 形式的规则 ID。
const (
	kindCommand = "command"
	kindRegex   = "regex"
	kindKeyword = "keyword"
	kindEvent   = "event"
	kindAll     = "all"
)

// registrar 是 bot.Registrar 的实现，绑定到某个插件名。
//
// 插件级的 Use 中间件保存在 Router 上并按插件名共享：同一插件名的注册器
// 看到的是同一份中间件列表，所以「先注册先执行」的语义跨注册器依然成立。
type registrar struct {
	r      *Router
	plugin string
}

// 确保 registrar 满足 bot.Registrar。
var _ bot.Registrar = (*registrar)(nil)

// OnCommand 注册命令触发。
//
// 默认事件类型为 bot.EventMessage，可通过 WithEventType 覆盖；命令名比较
// 忽略大小写，不含前缀。
func (g *registrar) OnCommand(name string, h bot.Handler, opts ...bot.Option) {
	g.add(&bot.Rule{
		Command:   name,
		EventType: bot.EventMessage,
		Handler:   h,
	}, kindCommand, opts)
}

// OnRegex 注册正则触发。
//
// 表达式编译失败时不 panic，也不会注册该规则，错误记入 RegistrationErrors，
// 其它规则不受影响。
func (g *registrar) OnRegex(expr string, h bot.Handler, opts ...bot.Option) {
	re, err := regexp.Compile(expr)
	if err != nil {
		g.r.recordErr(fmt.Errorf("router: 插件 %q 的正则 %q 编译失败: %w", g.plugin, expr, err))
		return
	}
	g.add(&bot.Rule{
		Regex:     re,
		EventType: bot.EventMessage,
		Handler:   h,
	}, kindRegex, opts)
}

// OnKeyword 注册关键词触发，命中任意关键词即触发，比较忽略大小写。
func (g *registrar) OnKeyword(words []string, h bot.Handler, opts ...bot.Option) {
	g.add(&bot.Rule{
		Keywords:  words,
		EventType: bot.EventMessage,
		Handler:   h,
	}, kindKeyword, opts)
}

// OnEvent 注册事件类型触发，不限定会话类型与平台。
func (g *registrar) OnEvent(t bot.EventType, h bot.Handler, opts ...bot.Option) {
	g.add(&bot.Rule{
		EventType: t,
		Handler:   h,
	}, kindEvent, opts)
}

// OnAll 注册兜底处理，不设置任何过滤条件，因此所有事件都会命中。
func (g *registrar) OnAll(h bot.Handler, opts ...bot.Option) {
	g.add(&bot.Rule{Handler: h}, kindAll, opts)
}

// Use 追加插件级中间件。
//
// 只影响调用 Use 之后为该插件注册的规则：每条规则在注册时把当前的中间件列表
// 复制进 Rule.Middlewares，之前已注册的规则保持不变。
func (g *registrar) Use(mw bot.Middleware) {
	if mw == nil {
		return
	}
	g.r.mu.Lock()
	g.r.pluginMW[g.plugin] = append(g.r.pluginMW[g.plugin], mw)
	g.r.mu.Unlock()
}

// add 在默认值之后应用 opts，然后交给 Router 注册；错误记入 RegistrationErrors。
func (g *registrar) add(rule *bot.Rule, kind string, opts []bot.Option) {
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(rule)
	}
	if err := g.r.register(g.plugin, rule, kind, true); err != nil {
		g.r.recordErr(err)
	}
}
