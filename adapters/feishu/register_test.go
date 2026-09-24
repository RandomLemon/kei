package feishu

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// testContext 构造一个已授予所声明权限的适配器上下文。
func testContext(raw map[string]any) bot.AdapterContext {
	return bot.AdapterContext{
		BotID:      "feishu-main",
		Config:     bot.NewConfig(raw),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func TestRegisterMetadata(t *testing.T) {
	meta, _, ok := bot.LookupAdapter(PlatformName)
	if !ok {
		t.Fatalf("适配器 %q 未注册", PlatformName)
	}
	if meta.Name != "feishu" || meta.Version != "v0.1.0" || meta.Author != "core" {
		t.Fatalf("元信息不符: %+v", meta)
	}
	if len(meta.Platforms) != 1 || meta.Platforms[0] != PlatformName {
		t.Fatalf("Platforms = %v, want [%s]", meta.Platforms, PlatformName)
	}
	if !meta.HasPermission(bot.PermNetwork) || !meta.HasPermission(bot.PermNetListen) {
		t.Fatalf("应声明 network 与 net_listen: %v", meta.Permissions)
	}
	if len(meta.Description) == 0 {
		t.Fatal("Description 不能为空")
	}
	if got, want := strings.Join(meta.Options, ","), "app_id,app_secret,verification_token,encrypt_key,listen_addr,path,base_url"; got != want {
		t.Fatalf("Options = %q, want %q", got, want)
	}
}

func TestFactoryBuildsAdapter(t *testing.T) {
	_, factory, ok := bot.LookupAdapter(PlatformName)
	if !ok {
		t.Fatalf("适配器 %q 未注册", PlatformName)
	}
	ad, err := factory(testContext(map[string]any{
		"app_id":             "cli_xxx",
		"app_secret":         "xxx",
		"verification_token": "xxx",
		"encrypt_key":        "",
		"listen_addr":        "127.0.0.1:0",
		"path":               "/feishu/event",
		"base_url":           "",
	}))
	if err != nil {
		t.Fatalf("工厂返回错误: %v", err)
	}
	if ad.Name() != PlatformName {
		t.Fatalf("Name = %q, want %s", ad.Name(), PlatformName)
	}
}

func TestFactoryRequiresNetworkPermission(t *testing.T) {
	_, factory, _ := bot.LookupAdapter(PlatformName)
	ctx := testContext(map[string]any{
		"app_id":             "cli_xxx",
		"app_secret":         "xxx",
		"verification_token": "xxx",
		"listen_addr":        "127.0.0.1:0",
	})
	ctx.HTTPClient = nil
	_, err := factory(ctx)
	if err == nil {
		t.Fatal("未授予 network 权限时工厂应返回错误")
	}
	if !strings.Contains(err.Error(), "network") {
		t.Fatalf("错误应指出缺少 network 权限: %v", err)
	}
}
