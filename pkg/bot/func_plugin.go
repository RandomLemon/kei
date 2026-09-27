package bot

import "context"

// FuncPlugin 是用函数实现的插件：只需写 OnSetup 注册触发规则，
// OnStart/OnStop 为 nil 时是空实现。用于「不单独建包、单文件定义业务逻辑」。
//
// 用法：
//
//	p := &bot.FuncPlugin{
//		Meta: bot.Metadata{Name: "hello", Version: "v0.1.0"},
//		OnSetup: func(ctx context.Context, reg bot.Registrar) error {
//			reg.OnCommand("hello", func(ctx context.Context, e *bot.Event, r bot.Reply) error {
//				return r.Text("hi").Send(ctx)
//			})
//			return nil
//		},
//	}
//
// 它不会自动进入编译期插件注册表（不调用 RegisterPlugin），只能经
// pkg/kei 门面的 Options.Plugins 注入。
//
// 所有方法对 nil 接收者安全（与 Config 的取值方法一致），因此字段缺失时
// 只会得到空实现，而不是 panic。
type FuncPlugin struct {
	// Meta 是插件元信息；Name 必须非空，且与其它插件不重名。
	Meta Metadata
	// OnSetup 注册触发规则，语义同 Plugin.Setup。
	OnSetup func(ctx context.Context, reg Registrar) error
	// OnStart 启动后台任务；nil 表示无后台任务。
	OnStart func(ctx context.Context) error
	// OnStop 释放资源；nil 表示无需释放。
	OnStop func(ctx context.Context) error
}

var _ Plugin = (*FuncPlugin)(nil)

// Metadata 返回插件元信息；p 为 nil 时返回零值（空名会被 pluginmgr.Add 拒绝）。
func (p *FuncPlugin) Metadata() Metadata {
	if p == nil {
		return Metadata{}
	}
	return p.Meta
}

// Setup 调用 OnSetup 注册触发规则；p 为 nil 或 OnSetup 为 nil 时是空实现。
func (p *FuncPlugin) Setup(ctx context.Context, reg Registrar) error {
	if p == nil || p.OnSetup == nil {
		return nil
	}
	return p.OnSetup(ctx, reg)
}

// Start 调用 OnStart 启动后台任务；p 为 nil 或 OnStart 为 nil 时是空实现。
func (p *FuncPlugin) Start(ctx context.Context) error {
	if p == nil || p.OnStart == nil {
		return nil
	}
	return p.OnStart(ctx)
}

// Stop 调用 OnStop 释放资源；p 为 nil 或 OnStop 为 nil 时是空实现。
func (p *FuncPlugin) Stop(ctx context.Context) error {
	if p == nil || p.OnStop == nil {
		return nil
	}
	return p.OnStop(ctx)
}
