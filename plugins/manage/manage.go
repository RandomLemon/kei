// Package manage 提供管理命令：/manage ping、/manage version、/manage plugins、
// /manage adapters、/manage admin、/manage status、/manage help。
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

// subcommands 是 /manage 的子命令表（名称 + 一行说明）。
//
// 它是 Metadata 描述与 /manage help 输出的唯一来源：新增子命令只需在此追加一行，
// 并在 Setup 中注册对应规则，Metadata 与 help 自动跟随。
var subcommands = []struct {
	name string
	desc string
}{
	{name: "ping", desc: "检查插件存活并显示运行时长"},
	{name: "version", desc: "显示框架版本与插件版本"},
	{name: "plugins", desc: "列出已加载插件"},
	{name: "adapters", desc: "列出已注册适配器与已绑定实例"},
	{name: "admin", desc: "管理员校验演示"},
	{name: "status", desc: "显示主机 CPU/内存/磁盘/GPU 使用情况（仅管理员）"},
	{name: "help", desc: "显示本帮助"},
}

// commandList 把子命令表拼成 "/manage ping、/manage version、…" 形式的枚举。
func commandList() string {
	names := make([]string, 0, len(subcommands))
	for _, sc := range subcommands {
		names = append(names, "/manage "+sc.name)
	}
	return strings.Join(names, "、")
}

// helpText 把子命令表格式化为 /manage help 的多行文本。
func helpText() string {
	var b strings.Builder
	b.WriteString("可用子命令：")
	for _, sc := range subcommands {
		fmt.Fprintf(&b, "\n- %s — %s", sc.name, sc.desc)
	}
	return b.String()
}

// Metadata 返回插件元信息。
func (p *Plugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name:        "manage",
		Version:     "v0.1.0",
		Author:      "core",
		Description: "管理命令：" + commandList(),
	}
}

// Setup 注册 /manage 命令及其子命令。
//
// 七条子命令共用命令名 "manage"，各自用 withSubcommand 限定第一个参数：
// /manage ping、/manage version、/manage plugins、/manage adapters、/manage admin、
// /manage status、/manage help。
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	pc, ok := bot.PluginContextFrom(ctx)
	if !ok {
		return fmt.Errorf("manage: missing plugin context")
	}
	p.cfg = pc.Config
	p.start = time.Now()

	reg.OnCommand("manage", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		uptime := time.Since(p.start).Truncate(time.Millisecond)
		return r.Text("pong · 已运行 " + uptime.String()).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:ping"), withSubcommand("ping"))

	reg.OnCommand("manage", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		meta := p.Metadata()
		return r.Text("kei v" + bot.Version + " · " + meta.Name + " " + meta.Version).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:version"), withSubcommand("version"))

	pluginOpts := []bot.Option{bot.WithPriority(100), bot.WithID("manage:plugins"), withSubcommand("plugins")}
	if p.cfg.Bool("plugins_admin_only", false) {
		pluginOpts = append(pluginOpts, bot.WithAdmin())
	}
	reg.OnCommand("manage", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		return r.Text(p.describePlugins(pc.Catalog)).Send(ctx)
	}, pluginOpts...)

	reg.OnCommand("manage", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		return r.Text(p.describeAdapters(pc.Adapters, bot.RegisteredAdapters())).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:adapters"), withSubcommand("adapters"))

	reg.OnCommand("manage", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		return r.Text("管理员校验通过").Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:admin"), withSubcommand("admin"), bot.WithAdmin())

	reg.OnCommand("manage", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		info, err := hostStatus(ctx)
		if err != nil {
			return r.Text("主机状态采集失败：" + err.Error()).Send(ctx)
		}
		return r.Text(formatHostInfo(info)).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:status"), withSubcommand("status"), bot.WithAdmin())

	reg.OnCommand("manage", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		return r.Text(helpText()).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:help"), withSubcommand("help"))

	return nil
}

// withSubcommand 限定规则只在命令的第一个参数等于 name 时命中（忽略大小写）。
//
// 命令名已在 Rule.Command 层限定为 "manage"，因此它把 /manage 的七个子命令
// 拆成七条独立规则：优先级、规则 ID 与管理员门槛（WithAdmin）仍按子命令各自生效。
func withSubcommand(name string) bot.Option {
	return bot.WithMatch(func(e *bot.Event) bool {
		if e == nil || e.Command == nil || len(e.Command.Args) == 0 {
			return false
		}
		return strings.EqualFold(e.Command.Args[0], name)
	})
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
// （每个 bot 用哪个适配器）。
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
		fmt.Fprintf(&b, "\n- %s → %s", info.BotID, info.Metadata.Name)
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
