package mock

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/RandomLemon/kei/pkg/bot"
)

// testContext 构造最小可用的适配器上下文。
func testContext(raw map[string]any) bot.AdapterContext {
	return bot.AdapterContext{
		BotID:  "mock-main",
		Config: bot.NewConfig(raw),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRegisterMetadata(t *testing.T) {
	meta, _, ok := bot.LookupAdapter(adapterName)
	if !ok {
		t.Fatalf("适配器 %q 未注册", adapterName)
	}
	if meta.Name != "mock" || meta.Version != "v0.1.0" || meta.Author != "core" {
		t.Fatalf("元信息不符: %+v", meta)
	}
	if len(meta.Platforms) != 1 || meta.Platforms[0] != defaultPlatform {
		t.Fatalf("Platforms = %v, want [%s]", meta.Platforms, defaultPlatform)
	}
	if len(meta.Permissions) != 1 || meta.Permissions[0] != bot.PermNetListen {
		t.Fatalf("Permissions = %v, want [%s]", meta.Permissions, bot.PermNetListen)
	}
	if !meta.HasPermission(bot.PermNetListen) || meta.HasPermission(bot.PermNetwork) {
		t.Fatalf("权限判定不符: %+v", meta.Permissions)
	}
	if len(meta.Description) == 0 {
		t.Fatal("Description 不能为空")
	}
	if got, want := strings.Join(meta.Options, ","), "platform,listen_addr"; got != want {
		t.Fatalf("Options = %q, want %q", got, want)
	}
}

func TestFactoryBuildsAdapter(t *testing.T) {
	_, factory, ok := bot.LookupAdapter(adapterName)
	if !ok {
		t.Fatalf("适配器 %q 未注册", adapterName)
	}
	ad, err := factory(testContext(map[string]any{
		"platform":    "mock",
		"listen_addr": "127.0.0.1:0",
	}))
	if err != nil {
		t.Fatalf("工厂返回错误: %v", err)
	}
	if ad.Name() != "mock" {
		t.Fatalf("Name = %q, want mock", ad.Name())
	}
	caps := ad.Capabilities()
	if !caps.Text || !caps.Markdown || !caps.Private || !caps.Group {
		t.Fatalf("默认能力应为全能力: %+v", caps)
	}
}

func TestFactoryDefaultsPlatform(t *testing.T) {
	_, factory, _ := bot.LookupAdapter(adapterName)
	ad, err := factory(testContext(nil))
	if err != nil {
		t.Fatalf("工厂返回错误: %v", err)
	}
	if ad.Name() != defaultPlatform {
		t.Fatalf("缺省 platform 时 Name = %q, want %q", ad.Name(), defaultPlatform)
	}
}

func TestFactoryWithoutHTTPClient(t *testing.T) {
	_, factory, _ := bot.LookupAdapter(adapterName)
	ctx := testContext(nil)
	ctx.HTTPClient = nil
	if _, err := factory(ctx); err != nil {
		t.Fatalf("Mock 不要求 network 权限, 不应失败: %v", err)
	}
}
