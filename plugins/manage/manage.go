// Package manage 提供管理命令：/ping、/version、/plugins、/admin。
//
// 它同时演示了插件目录（PluginContext.Catalog）与管理员规则（WithAdmin +
// Auth 中间件）的用法。
package manage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Plugin 是管理插件。
type Plugin struct {
	start time.Time
	cfg   *bot.Config
}

// 确保 Plugin 满足 bot.Plugin。
var _ bot.Plugin = (*Plugin)(nil)

// Metadata 返回插件元信息。
func (p *Plugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name:        "manage",
		Version:     "v0.1.0",
		Author:      "core",
		Description: "管理命令：/ping、/version、/plugins、/admin",
	}
}

// Setup 注册管理命令。
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	pc, ok := bot.PluginContextFrom(ctx)
	if !ok {
		return fmt.Errorf("manage: missing plugin context")
	}
	p.cfg = pc.Config
	p.start = time.Now()

	reg.OnCommand("ping", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		uptime := time.Since(p.start).Truncate(time.Millisecond)
		return r.Text("pong · 已运行 " + uptime.String()).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:ping"))

	reg.OnCommand("version", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		meta := p.Metadata()
		return r.Text("kei v" + bot.Version + " · " + meta.Name + " " + meta.Version).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:version"))

	pluginOpts := []bot.Option{bot.WithPriority(100), bot.WithID("manage:plugins")}
	if p.cfg.Bool("plugins_admin_only", false) {
		pluginOpts = append(pluginOpts, bot.WithAdmin())
	}
	reg.OnCommand("plugins", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		return r.Text(p.describePlugins(pc.Catalog)).Send(ctx)
	}, pluginOpts...)

	reg.OnCommand("admin", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		return r.Text("管理员校验通过").Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:admin"), bot.WithAdmin())

	return nil
}

// Start 无后台任务。
func (p *Plugin) Start(context.Context) error { return nil }

// Stop 无资源需要释放。
func (p *Plugin) Stop(context.Context) error { return nil }

// describePlugins 把插件目录格式化为多行文本。
func (p *Plugin) describePlugins(catalog bot.PluginCatalog) string {
	if catalog == nil {
		return "插件目录不可用"
	}
	metas := catalog.Plugins()
	if len(metas) == 0 {
		return "没有已加载的插件"
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Name < metas[j].Name })

	var b strings.Builder
	fmt.Fprintf(&b, "已加载 %d 个插件：", len(metas))
	for _, m := range metas {
		b.WriteString("\n- ")
		b.WriteString(m.Name)
		if m.Version != "" {
			b.WriteString(" " + m.Version)
		}
		if m.Author != "" {
			b.WriteString(" by " + m.Author)
		}
		if len(m.Permissions) > 0 {
			perms := make([]string, 0, len(m.Permissions))
			for _, p := range m.Permissions {
				perms = append(perms, string(p))
			}
			b.WriteString(" [" + strings.Join(perms, ",") + "]")
		}
		if m.Description != "" {
			b.WriteString(" — " + m.Description)
		}
	}
	return b.String()
}

func init() {
	bot.RegisterPlugin(&Plugin{})
}
