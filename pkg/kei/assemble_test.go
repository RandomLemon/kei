package kei

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/internal/adaptermgr"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"

	_ "github.com/RandomLemon/kei/adapters/mock"
	_ "github.com/RandomLemon/kei/adapters/onebot"
	_ "github.com/RandomLemon/kei/plugins/echo"
	_ "github.com/RandomLemon/kei/plugins/manage"
)

// mockBotYAML 是各校验测试使用的最小合法配置：一个 mock bot。
const mockBotYAML = "bots:\n  - name: b\n    adapter: mock\n"

// testLogger 返回写入 buffer 的测试日志器，便于断言日志文案。
func testLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

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
			name:    "未注册的适配器",
			bot:     config.BotConfig{Name: "wecom-main", Adapter: "wecom"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bindings, err := adaptermgr.Build(&config.Config{Bots: []config.BotConfig{tc.bot}}, adaptermgr.Deps{
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
		})
	}
}

func TestSelectPlugins(t *testing.T) {
	logger, buf := testLogger()
	cfg := &config.Config{Plugins: map[string]config.PluginConfig{
		"echo":       {Enabled: true},
		"disabled-x": {Enabled: false},
		"not-loaded": {Enabled: true},
		"manage":     {Enabled: true},
		"unlisted":   {Enabled: false},
	}}

	plugins, err := selectPlugins(cfg, nil, logger)
	if err != nil {
		t.Fatalf("selectPlugins: %v", err)
	}
	got := strings.Join(pluginNames(plugins), ",")
	// 只有已注册且启用的内置插件被加载；被禁用与未注册的都不加载。
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

func TestBuildStorageInjected(t *testing.T) {
	injected := storage.NewMemory()
	defer func() { _ = injected.Close() }()

	cfg := mustConfig(t, mockBotYAML)
	got, closeStore, err := buildStorage(Options{Storage: injected}, cfg)
	if err != nil {
		t.Fatalf("buildStorage: %v", err)
	}
	if got != bot.Storage(injected) {
		t.Fatalf("应返回注入的实例")
	}
	closeStore() // 注入时清理函数为 no-op
	if err := got.Set(context.Background(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("注入的存储不应被关闭: %v", err)
	}
}

func TestBuildStorageConfiguredSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "k.db")
	cfg := mustConfig(t, "bots:\n  - name: b\n    adapter: mock\nstorage:\n  type: sqlite\n  dsn: "+dsn)
	ctx := context.Background()

	first, closeFirst, err := buildStorage(Options{}, cfg)
	if err != nil {
		t.Fatalf("buildStorage: %v", err)
	}
	if err := first.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	closeFirst()

	second, closeSecond, err := buildStorage(Options{}, cfg)
	if err != nil {
		t.Fatalf("第二次 buildStorage: %v", err)
	}
	defer closeSecond()
	got, err := second.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("Get = %q, want v（sqlite 应持久化）", got)
	}
}

func TestBuildStorageUnknownType(t *testing.T) {
	cfg := mustConfig(t, "bots:\n  - name: b\n    adapter: mock\nstorage:\n  type: nosuch\n")
	_, _, err := buildStorage(Options{}, cfg)
	if err == nil || !strings.Contains(err.Error(), "未知存储类型") {
		t.Fatalf("错误 = %v", err)
	}
}

func TestLogEnvOverrides(t *testing.T) {
	logger, buf := testLogger()
	cfg := &config.Config{EnvOverrides: []config.EnvOverride{
		{Key: "KEI_LOG_LEVEL", Path: "log.level", Value: "debug"},
		{Key: "KEI_BOTS_QQ_MAIN_SECRET", Path: "bots.qq-main.settings.secret", Value: "top-secret"},
		{Key: "KEI_STORAGE_DSN", Path: "storage.dsn", Value: "user:pa55w0rd@tcp(127.0.0.1:3306)/db?parseTime=true"},
		{Key: "KEI_PLUGINS_REPORT_OPTIONS_ENDPOINT", Path: "plugins.report.settings.endpoint", Value: "alice:s3cr3t@tcp(db:3306)/logs"},
	}}
	logEnvOverrides(logger, cfg)

	out := buf.String()
	for _, want := range []string{"msg=环境变量覆盖配置", "env=KEI_LOG_LEVEL", "config=log.level", "value=debug"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"config=bots.qq-main.settings.secret", "config=storage.dsn", "config=plugins.report.settings.endpoint"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少敏感项路径 %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"top-secret", "pa55w0rd", "s3cr3t"} {
		if strings.Contains(out, secret) {
			t.Fatalf("敏感取值 %q 未掩码:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "value=******") {
		t.Fatalf("敏感取值未掩码:\n%s", out)
	}
}

// TestRedactEnvValue 覆盖按变量名与按取值形态两道脱敏判定。
func TestRedactEnvValue(t *testing.T) {
	const mask = "******"
	cases := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"普通键原样输出", "KEI_LOG_LEVEL", "debug", "debug"},
		{"空值原样输出", "KEI_LOG_LEVEL", "", ""},
		{"DSN 键名命中", "KEI_STORAGE_DSN", "/var/lib/kei/storage.db", mask},
		{"PASS 键名命中", "KEI_BOTS_X_PASS", "abc", mask},
		{"SECRET 键名命中", "KEI_BOTS_X_APP_SECRET", "abc", mask},
		{"值形如 mysql DSN", "KEI_PLUGINS_REPORT_OPTIONS_ENDPOINT", "user:pass@tcp(db:3306)/logs", mask},
		{"值形如 URL userinfo", "KEI_PLUGINS_REPORT_OPTIONS_ENDPOINT", "postgres://alice:s3cr3t@db:5432/app", mask},
		{"值形如关键字式 DSN", "KEI_PLUGINS_REPORT_OPTIONS_ENDPOINT", "host=db user=app password=s3cr3t", mask},
		{"无凭据 URL 不掩码", "KEI_PLUGINS_REPORT_OPTIONS_ENDPOINT", "https://example.com/hook", "https://example.com/hook"},
		{"无密码 userinfo 不掩码", "KEI_PLUGINS_REPORT_OPTIONS_ENDPOINT", "ssh://git@example.com/repo", "ssh://git@example.com/repo"},
		{"sqlite 路径不掩码", "KEI_PLUGINS_REPORT_OPTIONS_FILE", "file:/var/lib/kei/storage.db", "file:/var/lib/kei/storage.db"},
		{"普通地址不掩码", "KEI_METRICS_ADDR", "127.0.0.1:9090", "127.0.0.1:9090"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactEnvValue(tc.key, tc.value); got != tc.want {
				t.Fatalf("redactEnvValue(%q, %q) = %q, want %q", tc.key, tc.value, got, tc.want)
			}
		})
	}
}

func TestLogEnvOverridesRespectsInfoLevel(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logEnvOverrides(logger, &config.Config{EnvOverrides: []config.EnvOverride{
		{Key: "KEI_LOG_LEVEL", Path: "log.level", Value: "debug"},
	}})
	if buf.Len() != 0 {
		t.Fatalf("INFO 级别不应输出环境变量覆盖日志:\n%s", buf.String())
	}
}

// freeAddr 返回一个当前空闲的 loopback 地址（先监听再释放端口）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return addr
}

// waitHealthy 轮询 /healthz，直到 addr 上的 metrics 服务可达。
func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("metrics 服务未在超时内就绪: %s", addr)
}

func TestServeMetricsStopReleasesPort(t *testing.T) {
	addr := freeAddr(t)
	logger, _ := testLogger()

	stop, err := serveMetrics(addr, http.NotFoundHandler(), logger)
	if err != nil {
		t.Fatalf("serveMetrics: %v", err)
	}
	waitHealthy(t, addr)

	stop()
	stop() // 幂等

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("stop 返回后端口未释放: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestRunReleasesMetricsPortOnEarlyFailure 断言 Run 在 metrics 启动之后装配失败时
// 同步释放监听端口：紧接着的第二次 Run 能重新绑定同一地址并正常退出。
func TestRunReleasesMetricsPortOnEarlyFailure(t *testing.T) {
	addr := freeAddr(t)
	metricsYAML := "metrics:\n  addr: " + addr + "\n"

	if err := Run(context.Background(), Options{Config: []byte(metricsYAML + "bots:\n  - name: b\n    adapter: nosuch-adapter\n")}); err == nil {
		t.Fatal("未知适配器应使 Run 失败")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Options{Config: []byte(metricsYAML + mockBotYAML)}) }()

	waitHealthy(t, addr)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("第二次 Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("第二次 Run 未在超时内退出")
	}
}
