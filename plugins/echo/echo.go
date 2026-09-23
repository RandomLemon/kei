// Package echo 提供最简示例插件：回显命令参数。
package echo

import (
	"context"
	"strings"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Plugin 是 echo 插件，无状态、无外部依赖。
type Plugin struct{}

// 确保 Plugin 满足 bot.Plugin。
var _ bot.Plugin = (*Plugin)(nil)

// Metadata 返回插件元信息。
func (p *Plugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name:        "echo",
		Version:     "v0.1.0",
		Author:      "core",
		Description: "回显命令参数：/echo <文本>",
	}
}

// Setup 注册 /echo 命令。
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	reg.OnCommand("echo", func(ctx context.Context, e *bot.Event, r bot.Reply) error {
		if e.Command == nil || len(e.Command.Args) == 0 {
			return r.Text("用法: /echo <文本>").Send(ctx)
		}
		return r.Text(strings.Join(e.Command.Args, " ")).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("echo:echo"))
	return nil
}

// Start 无后台任务。
func (p *Plugin) Start(context.Context) error { return nil }

// Stop 无资源需要释放。
func (p *Plugin) Stop(context.Context) error { return nil }

func init() {
	bot.RegisterPlugin(&Plugin{})
}
