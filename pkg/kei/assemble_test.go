package kei

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/internal/adaptermgr"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"

	_ "github.com/RandomLemon/kei/adapters/feishu"
	_ "github.com/RandomLemon/kei/adapters/mock"
	_ "github.com/RandomLemon/kei/adapters/onebot"
	_ "github.com/RandomLemon/kei/plugins/echo"
	_ "github.com/RandomLemon/kei/plugins/manage"
)

// mockBotYAML 是各校验测试使用的最小合法配置：一个 mock bot。
const mockBotYAML = "bots:\n  - name: b\n    adapter: mock\n"

func mustConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml), nil)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return cfg
}

func pluginNames(plugins []bot.Plugin) []string {
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Metadata().Name)
	}
	return names
}

func TestAdaptermgrBuild(t *testing.T) {
	logger, _ := testLogger()
	httpClient := &http.Client{Timeout: time.Second}

	tests := []struct {
		name     string
		bot      config.BotConfig
		wantErr  bool
		platform string
	}{
		{
			name:     "mock",
			bot:      config.BotConfig{Name: "mock-main", Adapter: "mock", Settings: map[string]any{"platform": "mock", "listen_addr": "127.0.0.1:0"}},
			platform: "mock",
		},
		{
			name: "onebot",
			bot: config.BotConfig{Name: "qq-main", Adapter: "onebot", Settings: map[string]any{
				"api_url": "http://127.0.0.1:3000", "listen_addr": "127.0.0.1:0",
			}},
			platform: "onebot",
		},
		{
			name: "feishu",
			bot: config.BotConfig{Name: "feishu-main", Adapter: "feishu", Settings: map[string]any{
				"app_id": "cli_x", "app_secret": "s", "verification_token": "t", "listen_addr": "127.0.0.1:0",
			}},
			platform: "feishu",
		},
		{
			name:    "未注册的适配器",
			bot:     config.BotConfig{Name: "wecom-main", Adapter: "wecom"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bindings, err := adaptermgr.Build(context.Background(), &config.Config{Bots: []config.BotConfig{tc.bot}}, adaptermgr.Deps{
				Logger:     logger,
				Storage:    storage.NewMemory(),
				HTTPClient: httpClient,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望错误，得到 %+v", bindings.List())
				}
				return
			}
			if err != nil {
				t.Fatalf("adaptermgr.Build: %v", err)
			}
			t.Cleanup(func() { _ = bindings.Close() })

			list := bindings.List()
			if len(list) != 1 || list[0].BotID != tc.bot.Name {
				t.Fatalf("绑定 = %+v", list)
			}
			if got := list[0].Adapter.Name(); got != tc.platform {
				t.Fatalf("平台名 = %q, want %q", got, tc.platform)
			}
			if list[0].Info.Metadata.Name != tc.bot.Adapter {
				t.Fatalf("元信息名 = %q, want %q", list[0].Info.Metadata.Name, tc.bot.Adapter)
			}
			if list[0].Info.External {
				t.Fatal("进程内适配器不应标记为外部")
			}
		})
	}
}

func TestSelectPlugins(t *testing.T) {
	logger, buf := testLogger()
	cfg := &config.Config{Plugins: map[string]config.PluginConfig{
		"echo":       {Enabled: true},
		"disabled-x": {Enabled: false},
		"not-loaded": {Enabled: true},
		"external-x": {Enabled: true, Settings: map[string]any{"grpc_addr": "127.0.0.1:1"}},
		"manage":     {Enabled: true},
		"unlisted":   {Enabled: false},
	}}

	plugins, err := selectPlugins(cfg, nil, logger)
	if err != nil {
		t.Fatalf("selectPlugins: %v", err)
	}
	got := strings.Join(pluginNames(plugins), ",")
	// 只有已注册且启用的内置插件被加载；被禁用、未注册与外部插件都不加载。
	if !strings.Contains(got, "echo") || !strings.Contains(got, "manage") {
		t.Fatalf("已启用插件 = %v", pluginNames(plugins))
	}
	if strings.Contains(got, "disabled-x") || strings.Contains(got, "not-loaded") {
		t.Fatalf("不应加载被禁用/未注册的插件: %v", pluginNames(plugins))
	}
	if !strings.Contains(buf.String(), "not registered") {
		t.Fatalf("未提示未注册的插件: %s", buf.String())
	}
}

func TestSelectPluginsInjected(t *testing.T) {
	// 注入实例与注册表实例重名（echo）：注入优先，注册表实例跳过而不是报错。
	logger, buf := testLogger()
	cfg := &config.Config{Plugins: map[string]config.PluginConfig{
		"echo": {Enabled: true, Settings: map[string]any{"greeting": "hi"}},
	}}
	injected := &bot.FuncPlugin{Meta: bot.Metadata{Name: "echo"}}

	plugins, err := selectPlugins(cfg, []bot.Plugin{injected}, logger)
	if err != nil {
		t.Fatalf("selectPlugins: %v", err)
	}
	if len(plugins) != 1 || plugins[0] != bot.Plugin(injected) {
		t.Fatalf("重名时只应保留注入实例: %v", pluginNames(plugins))
	}
	// 同名配置键的设置原样保留，不被改写。
	if got := cfg.Plugins["echo"].Settings["greeting"]; got != "hi" {
		t.Fatalf("注入实例的同名设置被改写: %v", cfg.Plugins["echo"].Settings)
	}
	if !strings.Contains(buf.String(), "跳过注册表实例") {
		t.Fatalf("未提示注册表实例被注入实例覆盖: %s", buf.String())
	}

	// 配置里没有同名键时补 enabled: true，且不触发「未注册」告警。
	empty := &config.Config{Plugins: map[string]config.PluginConfig{}}
	logger2, buf2 := testLogger()
	inline := &bot.FuncPlugin{Meta: bot.Metadata{Name: "inline"}}
	plugins2, err := selectPlugins(empty, []bot.Plugin{inline}, logger2)
	if err != nil {
		t.Fatalf("selectPlugins: %v", err)
	}
	if len(plugins2) == 0 || plugins2[0] != bot.Plugin(inline) {
		t.Fatalf("注入实例应排在最前: %v", pluginNames(plugins2))
	}
	if pc, ok := empty.Plugins["inline"]; !ok || !pc.Enabled {
		t.Fatalf("注入实例应补 enabled: true: %+v", empty.Plugins)
	}
	if strings.Contains(buf2.String(), "not registered") {
		t.Fatalf("注入实例不应触发未注册告警: %s", buf2.String())
	}
}

func TestBuildConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"两个来源同时设置", Options{ConfigFile: "a.yaml", Config: []byte(mockBotYAML)}, "ConfigFile 与 Config 不能同时设置"},
		{"都未设置", Options{}, "必须提供 ConfigFile 或 Config"},
		{"内联配置的校验错误原样透出", Options{Config: []byte("bots: []")}, "至少需要一个 bot"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildConfig(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want 包含 %q", err, tc.want)
			}
		})
	}
}

func TestSelectPluginsErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		plugins []bot.Plugin
		want    string
	}{
		{
			name:    "实例为 nil",
			yaml:    mockBotYAML,
			plugins: []bot.Plugin{nil},
			want:    "插件实例为 nil",
		},
		{
			name:    "插件名为空",
			yaml:    mockBotYAML,
			plugins: []bot.Plugin{&bot.FuncPlugin{}},
			want:    "插件名为空",
		},
		{
			name: "注入名重复",
			yaml: mockBotYAML,
			plugins: []bot.Plugin{
				&bot.FuncPlugin{Meta: bot.Metadata{Name: "dup"}},
				&bot.FuncPlugin{Meta: bot.Metadata{Name: "dup"}},
			},
			want: "插件名 \"dup\" 重复",
		},
		{
			name:    "配置禁用与注入实例冲突",
			yaml:    mockBotYAML + "plugins:\n  inline:\n    enabled: false\n",
			plugins: []bot.Plugin{&bot.FuncPlugin{Meta: bot.Metadata{Name: "inline"}}},
			want:    "enabled: false，与注入的实例冲突",
		},
		{
			name:    "注入实例与外部插件声明冲突",
			yaml:    mockBotYAML + "plugins:\n  inline:\n    enabled: true\n    grpc_addr: 127.0.0.1:1\n    token: t\n",
			plugins: []bot.Plugin{&bot.FuncPlugin{Meta: bot.Metadata{Name: "inline"}}},
			want:    "不能同时声明为外部插件",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, _ := testLogger()
			_, err := selectPlugins(mustConfig(t, tc.yaml), tc.plugins, logger)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want 包含 %q", err, tc.want)
			}
		})
	}
}
