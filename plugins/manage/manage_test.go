package manage

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/RandomLemon/kei/pkg/bot"
)

type fakeCatalog struct{ metas []bot.Metadata }

func (f fakeCatalog) Plugins() []bot.Metadata { return f.metas }

type fakeAdapterCatalog struct{ infos []bot.AdapterInfo }

func (f fakeAdapterCatalog) Adapters() []bot.AdapterInfo { return f.infos }

func setup(t *testing.T, cfg map[string]any, catalog bot.PluginCatalog) *bot.RecordingRegistrar {
	return setupWithAdapters(t, cfg, catalog, nil)
}

func setupWithAdapters(t *testing.T, cfg map[string]any, catalog bot.PluginCatalog, adapters bot.AdapterCatalog) *bot.RecordingRegistrar {
	t.Helper()
	reg := bot.NewRecordingRegistrar()
	pc := bot.PluginContext{
		Name:     "manage",
		Config:   bot.NewConfig(cfg),
		Logger:   slog.New(slog.DiscardHandler),
		Catalog:  catalog,
		Adapters: adapters,
	}
	ctx := bot.WithPluginContext(context.Background(), pc)
	if err := (&Plugin{}).Setup(ctx, reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return reg
}

// bySubcommand 返回 /manage 指定子命令对应的规则。
func bySubcommand(t *testing.T, reg *bot.RecordingRegistrar, sub string) *bot.Rule {
	t.Helper()
	rule, ok := reg.Find(func(r *bot.Rule) bool { return r.ID == "manage:"+sub })
	if !ok {
		t.Fatalf("未注册 /manage 子命令 %s", sub)
	}
	return rule
}

func invoke(t *testing.T, reg *bot.RecordingRegistrar, sub string) string {
	t.Helper()
	rule := bySubcommand(t, reg, sub)
	ev := &bot.Event{
		Type:    bot.EventMessage,
		Command: &bot.Command{Name: "manage", Args: []string{sub}},
		Message: &bot.Message{Kind: bot.MessagePrivate},
	}
	reply := bot.NewNoopReply()
	if err := rule.Handler(context.Background(), ev, reply); err != nil {
		t.Fatalf("Handler(/manage %s): %v", sub, err)
	}
	return reply.PlainText()
}

func TestSubcommandMatching(t *testing.T) {
	reg := setup(t, nil, nil)
	cases := []struct {
		sub   string
		args  []string
		match bool
	}{
		{"ping", []string{"ping"}, true},
		{"ping", []string{"PING", "extra"}, true},
		{"ping", []string{"version"}, false},
		{"ping", nil, false},
		{"plugins", []string{"plugins"}, true},
		{"plugins", []string{"plug"}, false},
		{"help", []string{"HELP"}, true},
		{"help", []string{"help", "ping"}, true},
		{"help", []string{"h"}, false},
	}
	for _, tc := range cases {
		rule := bySubcommand(t, reg, tc.sub)
		if rule.Command != "manage" {
			t.Fatalf("%s 子命令规则的命令名 = %q, 应为 manage", tc.sub, rule.Command)
		}
		ev := &bot.Event{
			Type:    bot.EventMessage,
			Command: &bot.Command{Name: "manage", Args: tc.args},
			Message: &bot.Message{Kind: bot.MessagePrivate},
		}
		if got := rule.Matches(ev); got != tc.match {
			t.Fatalf("/manage %s 对 args=%v 匹配 = %v, 期望 %v", tc.sub, tc.args, got, tc.match)
		}
	}
}

func TestPingAndVersion(t *testing.T) {
	reg := setup(t, nil, nil)

	if got := invoke(t, reg, "ping"); !strings.HasPrefix(got, "pong") {
		t.Fatalf("ping 回复 = %q", got)
	}
	if got := invoke(t, reg, "version"); !strings.Contains(got, bot.Version) {
		t.Fatalf("version 回复 = %q, 应包含框架版本 %s", got, bot.Version)
	}
}

func TestPluginListing(t *testing.T) {
	t.Run("有插件目录", func(t *testing.T) {
		catalog := fakeCatalog{metas: []bot.Metadata{
			{Name: "weather", Version: "v1", Author: "core", Description: "天气", Permissions: []bot.Permission{bot.PermNetwork}},
			{Name: "echo", Version: "v1"},
		}}
		reg := setup(t, nil, catalog)

		got := invoke(t, reg, "plugins")
		if !strings.Contains(got, "已加载 2 个插件") {
			t.Fatalf("回复 = %q", got)
		}
		// 按名字排序，echo 在 weather 前。
		if strings.Index(got, "echo") > strings.Index(got, "weather") {
			t.Fatalf("插件列表未按名字排序: %q", got)
		}
		if !strings.Contains(got, "network") {
			t.Fatalf("未展示权限: %q", got)
		}
	})

	t.Run("无插件目录", func(t *testing.T) {
		reg := setup(t, nil, nil)
		if got := invoke(t, reg, "plugins"); !strings.Contains(got, "不可用") {
			t.Fatalf("回复 = %q", got)
		}
	})

	t.Run("空插件列表", func(t *testing.T) {
		reg := setup(t, nil, fakeCatalog{})
		if got := invoke(t, reg, "plugins"); !strings.Contains(got, "没有已加载") {
			t.Fatalf("回复 = %q", got)
		}
	})
}

func TestAdapterListing(t *testing.T) {
	const name = "manage-test-adapter"
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        name,
		Version:     "v9.9",
		Author:      "core",
		Description: "测试适配器",
		Platforms:   []string{"testplat"},
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermNetListen},
	}, func(bot.AdapterContext) (bot.Adapter, error) { return nil, nil })

	t.Run("注册表与绑定信息", func(t *testing.T) {
		catalog := fakeAdapterCatalog{infos: []bot.AdapterInfo{
			{BotID: "test-internal", Metadata: bot.AdapterMetadata{Name: name, Platforms: []string{"testplat"}}},
			{BotID: "test-other", Metadata: bot.AdapterMetadata{Name: "other-adapter"}},
		}}
		reg := setupWithAdapters(t, nil, nil, catalog)
		got := invoke(t, reg, "adapters")

		for _, want := range []string{name, "testplat", "network,net_listen", "测试适配器",
			"test-internal → " + name, "test-other → other-adapter"} {
			if !strings.Contains(got, want) {
				t.Fatalf("回复缺少 %q:\n%s", want, got)
			}
		}
	})

	t.Run("无适配器目录", func(t *testing.T) {
		reg := setupWithAdapters(t, nil, nil, nil)
		if got := invoke(t, reg, "adapters"); !strings.Contains(got, "绑定信息不可用") {
			t.Fatalf("回复 = %q", got)
		}
	})

	t.Run("空绑定列表", func(t *testing.T) {
		reg := setupWithAdapters(t, nil, nil, fakeAdapterCatalog{})
		if got := invoke(t, reg, "adapters"); !strings.Contains(got, "没有已绑定的适配器实例") {
			t.Fatalf("回复 = %q", got)
		}
	})
}

func TestAdminRules(t *testing.T) {
	t.Run("默认仅 /manage admin 需要管理员", func(t *testing.T) {
		reg := setup(t, nil, nil)

		admin := bySubcommand(t, reg, "admin")
		if !admin.AdminOnly {
			t.Fatalf("/manage admin 规则应标记 AdminOnly: %+v", admin)
		}
		if plugins := bySubcommand(t, reg, "plugins"); plugins.AdminOnly {
			t.Fatal("默认 /manage plugins 不需要管理员")
		}
	})

	t.Run("plugins_admin_only 生效", func(t *testing.T) {
		reg := setup(t, map[string]any{"plugins_admin_only": true}, nil)
		if plugins := bySubcommand(t, reg, "plugins"); !plugins.AdminOnly {
			t.Fatal("开启 plugins_admin_only 后 /manage plugins 应需要管理员")
		}
	})
}

func TestHelpListing(t *testing.T) {
	reg := setup(t, nil, nil)
	got := invoke(t, reg, "help")

	if !strings.HasPrefix(got, "可用子命令：") {
		t.Fatalf("help 回复 = %q", got)
	}
	for _, sub := range []string{"ping", "version", "plugins", "adapters", "admin", "help"} {
		if !strings.Contains(got, "- "+sub+" ") {
			t.Fatalf("help 未列出子命令 %s:\n%s", sub, got)
		}
	}
	if help := bySubcommand(t, reg, "help"); help.AdminOnly {
		t.Fatal("/manage help 不需要管理员")
	}
}

func TestPriorityBeatsFallbackPlugins(t *testing.T) {
	reg := setup(t, nil, nil)
	for _, sub := range []string{"ping", "version", "plugins", "adapters", "admin", "help"} {
		rule := bySubcommand(t, reg, sub)
		if rule.Priority <= 0 {
			t.Fatalf("/manage %s 优先级 = %d, 应大于 0", sub, rule.Priority)
		}
	}
}
