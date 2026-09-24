package onebot

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
		BotID:      "qq-main",
		Config:     bot.NewConfig(raw),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func TestRegisterMetadata(t *testing.T) {
	meta, _, ok := bot.LookupAdapter(platformName)
	if !ok {
		t.Fatalf("适配器 %q 未注册", platformName)
	}
	if meta.Name != "onebot" || meta.Version != "v0.1.0" || meta.Author != "core" {
		t.Fatalf("元信息不符: %+v", meta)
	}
	if len(meta.Platforms) != 1 || meta.Platforms[0] != platformName {
		t.Fatalf("Platforms = %v, want [%s]", meta.Platforms, platformName)
	}
	if !meta.HasPermission(bot.PermNetwork) || !meta.HasPermission(bot.PermNetListen) {
		t.Fatalf("应声明 network 与 net_listen: %v", meta.Permissions)
	}
	if len(meta.Description) == 0 {
		t.Fatal("Description 不能为空")
	}
	if got, want := strings.Join(meta.Options, ","), "api_url,listen_addr,path,secret,access_token,self_id"; got != want {
		t.Fatalf("Options = %q, want %q", got, want)
	}
}

func TestFactoryBuildsAdapter(t *testing.T) {
	_, factory, ok := bot.LookupAdapter(platformName)
	if !ok {
		t.Fatalf("适配器 %q 未注册", platformName)
	}
	ad, err := factory(testContext(map[string]any{
		"api_url":      "http://127.0.0.1:3000",
		"listen_addr":  "127.0.0.1:0",
		"path":         "/onebot/event",
		"secret":       "s",
		"access_token": "t",
		"self_id":      "10000",
	}))
	if err != nil {
		t.Fatalf("工厂返回错误: %v", err)
	}
	if ad.Name() != platformName {
		t.Fatalf("Name = %q, want %s", ad.Name(), platformName)
	}
}

func TestFactoryRequiresNetworkPermission(t *testing.T) {
	_, factory, _ := bot.LookupAdapter(platformName)
	ctx := testContext(map[string]any{
		"api_url":     "http://127.0.0.1:3000",
		"listen_addr": "127.0.0.1:0",
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
