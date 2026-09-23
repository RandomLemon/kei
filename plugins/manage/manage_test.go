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

func setup(t *testing.T, cfg map[string]any, catalog bot.PluginCatalog) *bot.RecordingRegistrar {
	t.Helper()
	reg := bot.NewRecordingRegistrar()
	pc := bot.PluginContext{
		Name:    "manage",
		Config:  bot.NewConfig(cfg),
		Logger:  slog.New(slog.DiscardHandler),
		Catalog: catalog,
	}
	ctx := bot.WithPluginContext(context.Background(), pc)
	if err := (&Plugin{}).Setup(ctx, reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return reg
}

func invoke(t *testing.T, reg *bot.RecordingRegistrar, cmd string) string {
	t.Helper()
	rule, ok := reg.Command(cmd)
	if !ok {
		t.Fatalf("未注册命令 %s", cmd)
	}
	ev := &bot.Event{Type: bot.EventMessage, Command: &bot.Command{Name: cmd}, Message: &bot.Message{Kind: bot.MessagePrivate}}
	reply := bot.NewNoopReply()
	if err := rule.Handler(context.Background(), ev, reply); err != nil {
		t.Fatalf("Handler(%s): %v", cmd, err)
	}
	return reply.PlainText()
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

func TestAdminRules(t *testing.T) {
	t.Run("默认仅 /admin 需要管理员", func(t *testing.T) {
		reg := setup(t, nil, nil)

		admin, ok := reg.Command("admin")
		if !ok || !admin.AdminOnly {
			t.Fatalf("/admin 规则应标记 AdminOnly: %+v", admin)
		}
		plugins, _ := reg.Command("plugins")
		if plugins.AdminOnly {
			t.Fatal("默认 /plugins 不需要管理员")
		}
	})

	t.Run("plugins_admin_only 生效", func(t *testing.T) {
		reg := setup(t, map[string]any{"plugins_admin_only": true}, nil)
		plugins, _ := reg.Command("plugins")
		if !plugins.AdminOnly {
			t.Fatal("开启 plugins_admin_only 后 /plugins 应需要管理员")
		}
	})
}

func TestPriorityBeatsFallbackPlugins(t *testing.T) {
	reg := setup(t, nil, nil)
	for _, cmd := range []string{"ping", "version", "plugins", "admin"} {
		rule, ok := reg.Command(cmd)
		if !ok {
			t.Fatalf("缺少命令 %s", cmd)
		}
		if rule.Priority <= 0 {
			t.Fatalf("%s 优先级 = %d, 应大于 0", cmd, rule.Priority)
		}
	}
}
