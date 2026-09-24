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
		Description: "管理命令：/ping、/version、/plugins、/adapters、/admin",
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

	reg.OnCommand("adapters", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		return r.Text(p.describeAdapters(pc.Adapters, bot.RegisteredAdapters())).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:adapters"))

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
			b.WriteString(" [" + joinPermissions(m.Permissions) + "]")
		}
		if m.Description != "" {
			b.WriteString(" — " + m.Description)
		}
	}
	return b.String()
}

// describeAdapters 把适配器注册表与已绑定实例格式化为多行文本。
//
// 注册表来自 pkg/bot（编译期注册，含第三方进程内适配器），绑定信息来自引擎
// （每个 bot 用哪个适配器、是否走外部 gRPC 进程）。
func (p *Plugin) describeAdapters(catalog bot.AdapterCatalog, registered []bot.AdapterMetadata) string {
	sort.Slice(registered, func(i, j int) bool { return registered[i].Name < registered[j].Name })

	var b strings.Builder
	if len(registered) == 0 {
		b.WriteString("没有已注册的适配器")
	} else {
		fmt.Fprintf(&b, "已注册 %d 个适配器：", len(registered))
		for _, m := range registered {
			b.WriteString("\n- ")
			b.WriteString(m.Name)
			if m.Version != "" {
				b.WriteString(" " + m.Version)
			}
			if len(m.Platforms) > 0 {
				b.WriteString(" (" + strings.Join(m.Platforms, ",") + ")")
			}
			if len(m.Permissions) > 0 {
				b.WriteString(" [" + joinPermissions(m.Permissions) + "]")
			}
			if m.Description != "" {
				b.WriteString(" — " + m.Description)
			}
		}
	}
	if catalog == nil {
		b.WriteString("\n绑定信息不可用")
		return b.String()
	}

	infos := catalog.Adapters()
	if len(infos) == 0 {
		b.WriteString("\n没有已绑定的适配器实例")
		return b.String()
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].BotID < infos[j].BotID })

	fmt.Fprintf(&b, "\n已绑定 %d 个实例：", len(infos))
	for _, info := range infos {
		kind := "进程内"
		if info.External {
			kind = "外部 gRPC"
		}
		fmt.Fprintf(&b, "\n- %s → %s（%s）", info.BotID, info.Metadata.Name, kind)
	}
	return b.String()
}

// joinPermissions 把权限列表拼成逗号分隔文本。
func joinPermissions(perms []bot.Permission) string {
	parts := make([]string, 0, len(perms))
	for _, p := range perms {
		parts = append(parts, string(p))
	}
	return strings.Join(parts, ",")
}

func init() {
	bot.RegisterPlugin(&Plugin{})
}
