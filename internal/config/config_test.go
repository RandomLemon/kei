package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fullYAML 是一份覆盖全部配置段的样例配置。
const fullYAML = `
log:
  level: debug
  format: json
metrics:
  addr: ":9100"
grpc:
  addr: "127.0.0.1:9000"
limits:
  handler_rate: 5
  handler_burst: 10
  send_rate: 0.5
  send_burst: 2
auth:
  admin_users:
    - "u1"
    - "u2"
bots:
  - name: feishu-main
    adapter: feishu
    app_id: app-1
    app_secret: secret-1
    http_timeout: 3s
    plugins:
      - weather
  - name: qq-main
    adapter: onebot
    ws_url: ws://127.0.0.1:3001
plugins:
  weather:
    enabled: true
    api_key: xxx
    retry: 3
`

func mustLoadBytes(t *testing.T, data string, env ...string) *Config {
	t.Helper()
	cfg, err := LoadBytes([]byte(data), env)
	if err != nil {
		t.Fatalf("LoadBytes 失败: %v", err)
	}
	return cfg
}

func TestLoadBytesFullYAML(t *testing.T) {
	cfg := mustLoadBytes(t, fullYAML)

	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Fatalf("log = %+v", cfg.Log)
	}
	if cfg.Metrics.Addr != ":9100" {
		t.Fatalf("metrics.addr = %q", cfg.Metrics.Addr)
	}
	if len(cfg.Bots) != 2 {
		t.Fatalf("bots 数量 = %d", len(cfg.Bots))
	}

	feishu := cfg.Bots[0]
	if feishu.Name != "feishu-main" || feishu.Adapter != "feishu" {
		t.Fatalf("bots[0] = %+v", feishu)
	}
	if feishu.Settings["app_id"] != "app-1" || feishu.Settings["app_secret"] != "secret-1" {
		t.Fatalf("bots[0].Settings = %v", feishu.Settings)
	}
	if feishu.Settings["http_timeout"] != "3s" {
		t.Fatalf("http_timeout = %#v，期望字符串 %q", feishu.Settings["http_timeout"], "3s")
	}
	for _, reserved := range []string{"name", "adapter", "plugins"} {
		if _, ok := feishu.Settings[reserved]; ok {
			t.Fatalf("Settings 不应包含保留键 %q: %v", reserved, feishu.Settings)
		}
	}
	if len(feishu.Plugins) != 1 || feishu.Plugins[0] != "weather" {
		t.Fatalf("bots[0].Plugins = %v", feishu.Plugins)
	}
	if cfg.Bots[1].Plugins != nil {
		t.Fatalf("bots[1].Plugins 应为空: %v", cfg.Bots[1].Plugins)
	}
	if cfg.Bots[1].Settings["ws_url"] != "ws://127.0.0.1:3001" {
		t.Fatalf("bots[1].Settings = %v", cfg.Bots[1].Settings)
	}

	weather, ok := cfg.Plugins["weather"]
	if !ok || !weather.Enabled {
		t.Fatalf("plugins.weather = %+v", weather)
	}
	if weather.Settings["api_key"] != "xxx" {
		t.Fatalf("weather.api_key = %#v", weather.Settings["api_key"])
	}
	if v, ok := weather.Settings["retry"].(int); !ok || v != 3 {
		t.Fatalf("weather.retry = %#v，期望 int 3", weather.Settings["retry"])
	}
	if _, ok := weather.Settings["enabled"]; ok {
		t.Fatalf("插件 Settings 不应包含 enabled: %v", weather.Settings)
	}

	// Settings 必须可被 encoding/json 编解码。
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
}

func TestLoadBytesDefaults(t *testing.T) {
	cfg := mustLoadBytes(t, "bots:\n  - name: a\n    adapter: mock\n")

	if cfg.Log.Level != "info" || cfg.Log.Format != "text" {
		t.Fatalf("默认 log = %+v", cfg.Log)
	}
	if cfg.Metrics.Addr != "" {
		t.Fatalf("默认 metrics.addr = %q", cfg.Metrics.Addr)
	}
	if cfg.Plugins == nil {
		t.Fatal("Plugins 不应为 nil")
	}
	if pc := cfg.Plugin("nope"); pc.Enabled || pc.Settings != nil {
		t.Fatalf("缺失插件应返回零值: %+v", pc)
	}
	if got := cfg.Bots[0].Settings; got != nil {
		t.Fatalf("无额外键时 Settings 应为 nil: %v", got)
	}
}

func TestPluginScalarShorthand(t *testing.T) {
	cfg := mustLoadBytes(t, `
bots:
  - name: a
    adapter: mock
plugins:
  echo: true
  weather: false
  empty:
`)

	echo := cfg.Plugin("echo")
	if !echo.Enabled || echo.Settings != nil {
		t.Fatalf("echo = %+v", echo)
	}
	weather := cfg.Plugin("weather")
	if weather.Enabled || weather.Settings != nil {
		t.Fatalf("weather = %+v", weather)
	}
	if empty := cfg.Plugin("empty"); empty.Enabled {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestSettingsJSONRoundTrip(t *testing.T) {
	cfg := mustLoadBytes(t, `
bots:
  - name: a
    adapter: mock
    nested:
      labels:
        "1": one
        2: two
      list:
        - 1
        - name: x
          deep: true
plugins:
  p:
    enabled: true
    limits:
      per_minute: 10
`)

	// Settings 里不得残留非 string 键的 map，否则 json.Marshal 会失败。
	for _, tc := range []struct {
		name     string
		settings map[string]any
	}{
		{"bot", cfg.Bots[0].Settings},
		{"plugin", cfg.Plugin("p").Settings},
	} {
		if _, err := json.Marshal(tc.settings); err != nil {
			t.Fatalf("%s Settings 不可 JSON 编码: %v (%#v)", tc.name, err, tc.settings)
		}
	}

	nested, ok := cfg.Bots[0].Settings["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %#v", cfg.Bots[0].Settings["nested"])
	}
	labels, ok := nested["labels"].(map[string]any)
	if !ok {
		t.Fatalf("labels = %#v", nested["labels"])
	}
	if labels["1"] != "one" || labels["2"] != "two" {
		t.Fatalf("labels 键未规范化为字符串: %#v", labels)
	}
	list, ok := nested["list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("list = %#v", nested["list"])
	}
	if elem, ok := list[1].(map[string]any); !ok || elem["deep"] != true {
		t.Fatalf("list[1] = %#v", list[1])
	}
}

func TestPluginQuotedBoolEnabled(t *testing.T) {
	cfg := mustLoadBytes(t, `
bots:
  - name: a
    adapter: mock
plugins:
  on:
    enabled: "true"
  off:
    enabled: "false"
    api_key: k
`)

	if !cfg.Plugin("on").Enabled {
		t.Fatalf("enabled: \"true\" 应解析为 true: %+v", cfg.Plugin("on"))
	}
	off := cfg.Plugin("off")
	if off.Enabled {
		t.Fatalf("enabled: \"false\" 应解析为 false: %+v", off)
	}
	if off.Settings["api_key"] != "k" {
		t.Fatalf("off.Settings = %v", off.Settings)
	}
}

func TestEnvOverrides(t *testing.T) {
	env := []string{
		"KEI_LOG_LEVEL=warn",
		"kei_log_format=json",                    // 前缀与键名都不区分大小写
		"KEI_METRICS_ADDR=:9090",                 // 顶层直接字段
		"KEI_GRPC_ADDR=127.0.0.1:7000",           // grpc 段
		"KEI_LIMITS_HANDLER_RATE=7.5",            // 浮点
		"KEI_LIMITS_SEND_BURST=4",                // 整数
		"KEI_AUTH_ADMIN_USERS=u1, u2 ,",          // 逗号分隔，忽略空项
		"KEI_BOTS_FEISHU_MAIN_APP_ID=new-app-id", // bot 名 feishu-main → FEISHU_MAIN
		"KEI_BOTS_feishu-main_APP_SECRET=s2",     // "-" 与 "_" 等价
		"KEI_BOTS_FEISHU_MAIN_http_timeout=9s",   // 已有键名沿用原写法
		"KEI_BOTS_QQ_MAIN_ADAPTER=mock",
		"KEI_BOTS_QQ_MAIN_PLUGINS=echo,weather",
		"KEI_PLUGINS_WEATHER_ENABLED=false",   // 布尔覆盖
		"KEI_PLUGINS_WEATHER_API_KEY=new-key", // 已有键名沿用原写法
		"KEI_PLUGINS_WEATHER_RETRY=9",         // 覆盖为整数
		"KEI_PLUGINS_WEATHER_NEW_OPTION=1.5",  // 新键：小写下划线 + float64
		"KEI_PLUGINS_UNKNOWN_ENABLED=true",    // 未知插件：忽略
		"KEI_BOTS_UNKNOWN_BOT_KEY=v",          // 未知 bot：忽略
		"KEI_UNKNOWN_KEY=v",                   // 未知路径：忽略
		"PATH=/usr/bin",                       // 非 KEI_ 前缀：忽略
		"KEI_",                                // 空后缀：忽略
	}
	cfg := mustLoadBytes(t, fullYAML, env...)

	if cfg.Log.Level != "warn" || cfg.Log.Format != "json" {
		t.Fatalf("log = %+v", cfg.Log)
	}
	if cfg.Metrics.Addr != ":9090" {
		t.Fatalf("metrics.addr = %q", cfg.Metrics.Addr)
	}
	if cfg.Grpc.Addr != "127.0.0.1:7000" {
		t.Fatalf("grpc.addr = %q", cfg.Grpc.Addr)
	}
	if cfg.Limits.HandlerRate != 7.5 || cfg.Limits.HandlerBurst != 10 {
		t.Fatalf("limits = %+v", cfg.Limits)
	}
	if cfg.Limits.SendBurst != 4 || cfg.Limits.SendRate != 0.5 {
		t.Fatalf("limits = %+v", cfg.Limits)
	}
	if got := strings.Join(cfg.Auth.AdminUsers, "|"); got != "u1|u2" {
		t.Fatalf("auth.admin_users = %q", got)
	}

	feishu := cfg.Bots[0]
	if feishu.Settings["app_id"] != "new-app-id" {
		t.Fatalf("app_id = %#v", feishu.Settings["app_id"])
	}
	if feishu.Settings["app_secret"] != "s2" {
		t.Fatalf("app_secret = %#v", feishu.Settings["app_secret"])
	}
	if feishu.Settings["http_timeout"] != "9s" {
		t.Fatalf("http_timeout = %#v", feishu.Settings["http_timeout"])
	}
	if feishu.Adapter != "feishu" {
		t.Fatalf("feishu.adapter 不应被 QQ 的覆盖影响: %q", feishu.Adapter)
	}

	qq := cfg.Bots[1]
	if qq.Adapter != "mock" {
		t.Fatalf("qq.adapter = %q", qq.Adapter)
	}
	if got := strings.Join(qq.Plugins, "|"); got != "echo|weather" {
		t.Fatalf("qq.plugins = %q", got)
	}
	if qq.Settings["ws_url"] != "ws://127.0.0.1:3001" {
		t.Fatalf("qq.Settings 被意外修改: %v", qq.Settings)
	}

	weather := cfg.Plugin("weather")
	if weather.Enabled {
		t.Fatalf("KEI_PLUGINS_WEATHER_ENABLED=false 未生效: %+v", weather)
	}
	if weather.Settings["api_key"] != "new-key" {
		t.Fatalf("api_key = %#v", weather.Settings["api_key"])
	}
	if v, ok := weather.Settings["retry"].(int); !ok || v != 9 {
		t.Fatalf("retry = %#v，期望 int 9", weather.Settings["retry"])
	}
	if v, ok := weather.Settings["new_option"].(float64); !ok || v != 1.5 {
		t.Fatalf("new_option = %#v，期望 float64 1.5", weather.Settings["new_option"])
	}
	if _, ok := cfg.Plugins["unknown"]; ok {
		t.Fatal("未匹配的 KEI_PLUGINS_UNKNOWN_* 不应创建插件")
	}
}

func TestEnvValueTypes(t *testing.T) {
	cfg := mustLoadBytes(t, `
bots:
  - name: a
    adapter: mock
plugins:
  p:
    enabled: true
`, []string{
		"KEI_BOTS_A_FLAG=true",
		"KEI_BOTS_A_COUNT=3",
		"KEI_BOTS_A_RATIO=1.5",
		"KEI_BOTS_A_NAME_STR=hello",
		"KEI_BOTS_A_QUOTED=42",
		"KEI_PLUGINS_P_ENABLED=false",
		"KEI_PLUGINS_P_LIMIT=abc",
	}...)

	s := cfg.Bots[0].Settings
	if v, ok := s["flag"].(bool); !ok || !v {
		t.Fatalf("flag = %#v", s["flag"])
	}
	if v, ok := s["count"].(int); !ok || v != 3 {
		t.Fatalf("count = %#v", s["count"])
	}
	if v, ok := s["ratio"].(float64); !ok || v != 1.5 {
		t.Fatalf("ratio = %#v", s["ratio"])
	}
	if v, ok := s["name_str"].(string); !ok || v != "hello" {
		t.Fatalf("name_str = %#v", s["name_str"])
	}
	if v, ok := s["quoted"].(int); !ok || v != 42 {
		t.Fatalf("quoted = %#v", s["quoted"])
	}

	p := cfg.Plugin("p")
	if p.Enabled {
		t.Fatalf("enabled 应为 false: %+v", p)
	}
	if v, ok := p.Settings["limit"].(string); !ok || v != "abc" {
		// 非数字、非布尔的值保持字符串
		t.Fatalf("limit = %#v", p.Settings["limit"])
	}
}

func TestEnvInjectionNeedsKnownPlugin(t *testing.T) {
	cfg := mustLoadBytes(t, `
bots:
  - name: main
    adapter: mock
plugins:
  weather:
    enabled: true
`, []string{"KEI_PLUGINS_WEATHER_FORECAST_DAYS=5"}...)

	w := cfg.Plugin("weather")
	if v, ok := w.Settings["forecast_days"].(int); !ok || v != 5 {
		t.Fatalf("forecast_days = %#v，期望 int 5", w.Settings["forecast_days"])
	}
}

func TestEnvExistingKeyNamePreserved(t *testing.T) {
	// YAML 中的键名大小写与 "-"/"." 写法不同，但规范化后同名，
	// 因此覆盖时沿用 YAML 的原始写法，而不是新建小写下划线键。
	cfg := mustLoadBytes(t, `
bots:
  - name: main
    adapter: mock
    App-Id: app-1
plugins:
  weather:
    enabled: true
    HTTP.Timeout: 5s
`, []string{
		"KEI_BOTS_MAIN_APP_ID=app-2",
		"KEI_PLUGINS_WEATHER_HTTP_TIMEOUT=9s",
	}...)

	if got := cfg.Bots[0].Settings["App-Id"]; got != "app-2" {
		t.Fatalf("App-Id = %#v，期望沿用原键名", cfg.Bots[0].Settings)
	}
	if _, ok := cfg.Bots[0].Settings["app_id"]; ok {
		t.Fatalf("不应额外创建 app_id 键: %v", cfg.Bots[0].Settings)
	}
	w := cfg.Plugin("weather")
	if got := w.Settings["HTTP.Timeout"]; got != "9s" {
		t.Fatalf("HTTP.Timeout = %#v，期望沿用原键名", w.Settings)
	}
	if _, ok := w.Settings["http_timeout"]; ok {
		t.Fatalf("不应额外创建 http_timeout 键: %v", w.Settings)
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("缺失文件应返回错误")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("错误未包装 os.ErrNotExist: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("错误未包含路径: %v", err)
	}
}

func TestLoadFromFileAppliesOSEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "log:\n  level: error\nbots:\n  - name: a\n    adapter: mock\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("KEI_LOG_LEVEL", "debug")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Fatalf("os.Environ 覆盖未生效: %q", cfg.Log.Level)
	}
}

func TestLoadInvalidYAMLReportsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("bots: [oops\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("非法 YAML 应返回错误")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("错误未包含路径: %v", err)
	}
}

func TestEnvLongestBotNameWins(t *testing.T) {
	// 规范化后 "feishu" 是 "feishu_main" 的前缀，KEI_BOTS_FEISHU_MAIN_X
	// 应只命中名字更长的 feishu-main。
	cfg := mustLoadBytes(t, `
bots:
  - name: feishu
    adapter: mock
  - name: feishu-main
    adapter: mock
`, []string{"KEI_BOTS_FEISHU_MAIN_TOKEN=t1"}...)

	if _, ok := cfg.Bots[0].Settings["main_token"]; ok {
		t.Fatalf("短名 bot 被误命中: %v", cfg.Bots[0].Settings)
	}
	if cfg.Bots[0].Settings != nil {
		t.Fatalf("短名 bot 不应有 Settings: %v", cfg.Bots[0].Settings)
	}
	if cfg.Bots[1].Settings["token"] != "t1" {
		t.Fatalf("长名 bot 未被命中: %v", cfg.Bots[1].Settings)
	}
}

func TestEnvBotNameFieldOverride(t *testing.T) {
	cfg := mustLoadBytes(t, `
bots:
  - name: old
    adapter: mock
`, []string{
		"KEI_BOTS_OLD_NAME=new",
		"KEI_BOTS_OLD_KEY=1",
	}...)

	if cfg.Bots[0].Name != "new" {
		t.Fatalf("bot 名未被覆盖: %q", cfg.Bots[0].Name)
	}
	if v, ok := cfg.Bots[0].Settings["key"].(int); !ok || v != 1 {
		t.Fatalf("Settings = %v", cfg.Bots[0].Settings)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		substr string
	}{
		{
			name:   "无 bot",
			yaml:   "log:\n  level: info\n",
			substr: "bots",
		},
		{
			name:   "bot 名为空",
			yaml:   "bots:\n  - adapter: mock\n",
			substr: "bots[0].name",
		},
		{
			name:   "adapter 为空",
			yaml:   "bots:\n  - name: a\n",
			substr: "bots[0].adapter",
		},
		{
			name:   "adapter 非法字符",
			yaml:   "bots:\n  - name: a\n    adapter: Feishu!\n",
			substr: "bots[0].adapter",
		},
		{
			name:   "重复 bot 名",
			yaml:   "bots:\n  - name: a\n    adapter: mock\n  - name: a\n    adapter: mock\n",
			substr: "bots[1].name",
		},
		{
			name:   "未知顶层键",
			yaml:   "bots:\n  - name: a\n    adapter: mock\nunknown: 1\n",
			substr: "未知顶层键",
		},
		{
			name:   "插件名为空",
			yaml:   "bots:\n  - name: a\n    adapter: mock\nplugins:\n  \"\": true\n",
			substr: "插件名",
		},
		{
			name:   "grpc 只给 cert",
			yaml:   "bots:\n  - name: a\n    adapter: mock\ngrpc:\n  addr: \":9000\"\n  cert_file: c.pem\n",
			substr: "grpc.key_file",
		},
		{
			name:   "limits 负速率",
			yaml:   "bots:\n  - name: a\n    adapter: mock\nlimits:\n  handler_rate: -1\n",
			substr: "limits.handler_rate",
		},
		{
			name:   "limits 负突发",
			yaml:   "bots:\n  - name: a\n    adapter: mock\nlimits:\n  send_burst: -2\n",
			substr: "limits.send_burst",
		},
		{
			name:   "非法 log.level",
			yaml:   "log:\n  level: infoo\nbots:\n  - name: a\n    adapter: mock\n",
			substr: "log.level",
		},
		{
			name:   "非法 log.format",
			yaml:   "log:\n  format: yaml\nbots:\n  - name: a\n    adapter: mock\n",
			substr: "log.format",
		},
		{
			name:   "顶层非映射",
			yaml:   "- a\n- b\n",
			substr: "顶层",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.yaml), nil)
			if err == nil {
				t.Fatal("应返回错误")
			}
			if !strings.Contains(err.Error(), tc.substr) {
				t.Fatalf("错误 %q 未包含 %q", err, tc.substr)
			}
		})
	}
}

func TestLogValuesAccepted(t *testing.T) {
	// 合法取值（含大小写混合）必须通过，且原样保留不做归一化。
	for _, tc := range []struct {
		yaml   string
		level  string
		format string
	}{
		{"log:\n  level: debug\n  format: json\n", "debug", "json"},
		{"log:\n  level: INFO\n  format: JSON\n", "INFO", "JSON"},
		{"log:\n  level: warn\n", "warn", "text"},
		{"log:\n  level: error\n", "error", "text"},
	} {
		body := tc.yaml + "bots:\n  - name: a\n    adapter: mock\n"
		cfg, err := LoadBytes([]byte(body), nil)
		if err != nil {
			t.Fatalf("合法 log 配置被拒绝 (%s): %v", strings.ReplaceAll(tc.yaml, "\n", " "), err)
		}
		if cfg.Log.Level != tc.level || cfg.Log.Format != tc.format {
			t.Fatalf("log = %+v，期望 level=%q format=%q", cfg.Log, tc.level, tc.format)
		}
	}

	// 环境变量覆盖同样受校验约束。
	if _, err := LoadBytes([]byte("bots:\n  - name: a\n    adapter: mock\n"),
		[]string{"KEI_LOG_LEVEL=trace"}); err == nil || !strings.Contains(err.Error(), "log.level") {
		t.Fatalf("非法 KEI_LOG_LEVEL 应报 log.level 错误: %v", err)
	}
	if _, err := LoadBytes([]byte("bots:\n  - name: a\n    adapter: mock\n"),
		[]string{"KEI_LOG_FORMAT=logfmt"}); err == nil || !strings.Contains(err.Error(), "log.format") {
		t.Fatalf("非法 KEI_LOG_FORMAT 应报 log.format 错误: %v", err)
	}
}

func TestGrpcYAMLAndTLS(t *testing.T) {
	cfg := mustLoadBytes(t, `
bots:
  - name: a
    adapter: mock
grpc:
  addr: "0.0.0.0:9000"
  cert_file: server.pem
  key_file: server-key.pem
  ca_file: ca.pem
`)
	want := GrpcConfig{
		Addr:     "0.0.0.0:9000",
		CertFile: "server.pem",
		KeyFile:  "server-key.pem",
		CAFile:   "ca.pem",
	}
	if cfg.Grpc != want {
		t.Fatalf("grpc = %+v，期望 %+v", cfg.Grpc, want)
	}

	// 三个文件未给齐（任意一个缺失）都必须报错，且错误里带上缺失字段路径。
	for _, keys := range []struct{ cert, key, ca, missing string }{
		{"c", "k", "", "grpc.ca_file"},
		{"c", "", "a", "grpc.key_file"},
		{"", "k", "a", "grpc.cert_file"},
	} {
		y := "bots:\n  - name: a\n    adapter: mock\ngrpc:\n"
		if keys.cert != "" {
			y += "  cert_file: " + keys.cert + "\n"
		}
		if keys.key != "" {
			y += "  key_file: " + keys.key + "\n"
		}
		if keys.ca != "" {
			y += "  ca_file: " + keys.ca + "\n"
		}
		_, err := LoadBytes([]byte(y), nil)
		if err == nil {
			t.Fatalf("TLS 文件不全应报错: %+v", keys)
		}
		if !strings.Contains(err.Error(), keys.missing) {
			t.Fatalf("错误 %q 未指出缺失字段 %q", err, keys.missing)
		}
	}
}

func TestLimitsAndAuthEnvOverride(t *testing.T) {
	cfg := mustLoadBytes(t, `
bots:
  - name: a
    adapter: mock
limits:
  handler_rate: 1
  handler_burst: 1
  send_rate: 1
  send_burst: 1
auth:
  admin_users: ["old"]
`, []string{
		"KEI_LIMITS_HANDLER_BURST=0",
		"KEI_LIMITS_SEND_RATE=20",
		"KEI_AUTH_ADMIN_USERS=root,admin",
	}...)

	if cfg.Limits.HandlerBurst != 0 || cfg.Limits.SendRate != 20 {
		t.Fatalf("limits = %+v", cfg.Limits)
	}
	if got := strings.Join(cfg.Auth.AdminUsers, "|"); got != "root|admin" {
		t.Fatalf("auth = %v", cfg.Auth.AdminUsers)
	}
}

func TestEnvBadNumericValue(t *testing.T) {
	_, err := LoadBytes([]byte("bots:\n  - name: a\n    adapter: mock\n"),
		[]string{"KEI_LIMITS_HANDLER_RATE=abc"})
	if err == nil {
		t.Fatal("非法数字应返回错误")
	}
	if !strings.Contains(err.Error(), "KEI_LIMITS_HANDLER_RATE") {
		t.Fatalf("错误未包含变量名: %v", err)
	}
}

// adaptersYAML 是一份含外部适配器声明的配置。
const adaptersYAML = `
grpc:
  addr: "127.0.0.1:9000"
bots:
  - name: myim-main
    adapter: myim
    api_base: https://im.example.com
adapters:
  myim:
    grpc_addr: "127.0.0.1:50071"
    token: change-me
    platform: myim
    timeout: 10s
    permissions: [receive_event, network, net_listen]
    listen_addr: "127.0.0.1:19081"
`

func TestAdaptersSection(t *testing.T) {
	cfg := mustLoadBytes(t, adaptersYAML)

	ac, ok := cfg.Adapters["myim"]
	if !ok {
		t.Fatalf("缺少适配器配置: %+v", cfg.Adapters)
	}
	if ac.GrpcAddr != "127.0.0.1:50071" || ac.Token != "change-me" || ac.Platform != "myim" {
		t.Fatalf("适配器字段 = %+v", ac)
	}
	if ac.Timeout != 10*time.Second {
		t.Fatalf("timeout = %v, 期望 10s", ac.Timeout)
	}
	if got := strings.Join(ac.Permissions, ","); got != "receive_event,network,net_listen" {
		t.Fatalf("permissions = %q", got)
	}
	// 非保留键进入 Settings，供核心下发给适配器进程。
	if got := ac.Settings["listen_addr"]; got != "127.0.0.1:19081" {
		t.Fatalf("listen_addr = %v", got)
	}
	if _, leaked := ac.Settings["grpc_addr"]; leaked {
		t.Fatal("grpc_addr 不应进入 Settings")
	}
}

func TestAdaptersDefaults(t *testing.T) {
	cfg := mustLoadBytes(t, `
grpc:
  addr: "127.0.0.1:9000"
bots:
  - name: myim-main
    adapter: myim
adapters:
  myim:
    grpc_addr: "127.0.0.1:50071"
    token: t
`)
	ac := cfg.Adapters["myim"]
	if got := strings.Join(ac.Permissions, ","); got != defaultAdapterPermission {
		t.Fatalf("permissions = %q, 期望默认 %q", got, defaultAdapterPermission)
	}
}

func TestAdaptersEnabled(t *testing.T) {
	t.Run("缺省启用", func(t *testing.T) {
		cfg := mustLoadBytes(t, adaptersYAML)
		if !cfg.AdapterEnabled("myim") {
			t.Fatal("未写 enabled 时应启用")
		}
		if !cfg.AdapterEnabled("never-declared") {
			t.Fatal("未声明的适配器应视为启用")
		}
	})

	t.Run("显式禁用外部适配器", func(t *testing.T) {
		cfg := mustLoadBytes(t, `
bots:
  - name: myim-main
    adapter: myim
adapters:
  myim:
    enabled: false
`)
		if cfg.AdapterEnabled("myim") {
			t.Fatal("enabled: false 应禁用")
		}
		if cfg.Adapters["myim"].GrpcAddr != "" {
			t.Fatalf("禁用条目不应要求 grpc_addr: %+v", cfg.Adapters["myim"])
		}
	})

	t.Run("禁用后不校验通道字段", func(t *testing.T) {
		cfg := mustLoadBytes(t, `
bots:
  - name: myim-main
    adapter: myim
adapters:
  myim:
    enabled: false
    grpc_addr: "127.0.0.1:50071"
    token: ""
`)
		if cfg.AdapterEnabled("myim") {
			t.Fatal("应保持禁用")
		}
	})

	t.Run("进程内声明只允许 enabled", func(t *testing.T) {
		cfg := mustLoadBytes(t, `
bots:
  - name: mock-main
    adapter: mock
adapters:
  mock:
    enabled: false
`)
		if cfg.AdapterEnabled("mock") {
			t.Fatal("进程内适配器也应可禁用")
		}
		if len(cfg.Adapters["mock"].Permissions) != 0 {
			t.Fatalf("进程内声明不应被填充默认权限: %v", cfg.Adapters["mock"].Permissions)
		}
	})

	t.Run("环境变量覆盖", func(t *testing.T) {
		cfg := mustLoadBytes(t, adaptersYAML, "KEI_ADAPTERS_MYIM_ENABLED=false")
		if cfg.AdapterEnabled("myim") {
			t.Fatal("环境变量应能禁用适配器")
		}

		cfg = mustLoadBytes(t, `
bots:
  - name: myim-main
    adapter: myim
adapters:
  myim:
    enabled: false
`, "KEI_ADAPTERS_MYIM_ENABLED=true")
		if !cfg.AdapterEnabled("myim") {
			t.Fatal("环境变量应能重新启用适配器")
		}
	})

	t.Run("enabled 非布尔报错", func(t *testing.T) {
		_, err := LoadBytes([]byte(`
bots:
  - name: myim-main
    adapter: myim
adapters:
  myim:
    enabled: 也许
`), nil)
		if err == nil || !strings.Contains(err.Error(), "adapters.myim.enabled") {
			t.Fatalf("错误 = %v", err)
		}
	})
}

func TestAdaptersEnvOverride(t *testing.T) {
	cfg := mustLoadBytes(t, adaptersYAML, []string{
		"KEI_ADAPTERS_MYIM_TOKEN=env-token",
		"KEI_ADAPTERS_MYIM_TIMEOUT=5",
		"KEI_ADAPTERS_MYIM_PERMISSIONS=receive_event,storage",
		"KEI_ADAPTERS_MYIM_EXTRA_KEY=1",
	}...)

	ac := cfg.Adapters["myim"]
	if ac.Token != "env-token" {
		t.Fatalf("token = %q", ac.Token)
	}
	if ac.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v, 期望 5s", ac.Timeout)
	}
	if got := strings.Join(ac.Permissions, ","); got != "receive_event,storage" {
		t.Fatalf("permissions = %q", got)
	}
	if got := ac.Settings["extra_key"]; got != 1 {
		t.Fatalf("extra_key = %v(%T), 期望 int 1", got, got)
	}
}

func TestAdaptersValidateErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "无 grpc_addr 时只允许 enabled",
			yaml: "bots:\n  - name: a\n    adapter: myim\nadapters:\n  myim:\n    token: t\n",
			want: "只允许 enabled",
		},
		{
			name: "缺少 token",
			yaml: "bots:\n  - name: a\n    adapter: myim\nadapters:\n  myim:\n    grpc_addr: \"127.0.0.1:1\"\n",
			want: "adapters.myim.token",
		},
		{
			name: "缺 grpc.addr",
			yaml: "bots:\n  - name: a\n    adapter: myim\nadapters:\n  myim:\n    grpc_addr: \"127.0.0.1:1\"\n    token: t\n",
			want: "grpc.addr",
		},
		{
			name: "适配器名非法",
			yaml: "grpc:\n  addr: \":1\"\nbots:\n  - name: a\n    adapter: myim\nadapters:\n  MyIM:\n    grpc_addr: \"127.0.0.1:1\"\n    token: t\n",
			want: "只允许 [a-z0-9_-]",
		},
		{
			name: "权限名为空",
			yaml: "grpc:\n  addr: \":1\"\nbots:\n  - name: a\n    adapter: myim\nadapters:\n  myim:\n    grpc_addr: \"127.0.0.1:1\"\n    token: t\n    permissions: [\"  \"]\n",
			want: "permissions",
		},
		{
			name: "timeout 非法",
			yaml: "grpc:\n  addr: \":1\"\nbots:\n  - name: a\n    adapter: myim\nadapters:\n  myim:\n    grpc_addr: \"127.0.0.1:1\"\n    token: t\n    timeout: 很快\n",
			want: "adapters.myim.timeout",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.yaml), nil)
			if err == nil {
				t.Fatal("期望报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v, 期望包含 %q", err, tc.want)
			}
		})
	}
}

// TestBotEnabled 验证实例级 enabled 开关的解码、缺省值与语义。
func TestBotEnabled(t *testing.T) {
	t.Run("缺省启用且不进入 Settings", func(t *testing.T) {
		cfg := mustLoadBytes(t, `
bots:
  - name: mock-main
    adapter: mock
`)
		if !cfg.Bots[0].IsEnabled() {
			t.Fatal("未写 enabled 时应启用")
		}
		if _, ok := cfg.Bots[0].Settings["enabled"]; ok {
			t.Fatalf("enabled 是核心保留键，不应落入 Settings: %+v", cfg.Bots[0].Settings)
		}
	})

	t.Run("显式禁用", func(t *testing.T) {
		cfg := mustLoadBytes(t, `
bots:
  - name: mock-main
    adapter: mock
    enabled: false
  - name: qq-main
    adapter: onebot
    enabled: true
`)
		if cfg.Bots[0].IsEnabled() {
			t.Fatal("enabled: false 应停用实例")
		}
		if !cfg.Bots[1].IsEnabled() {
			t.Fatal("enabled: true 应启用实例")
		}
		if _, ok := cfg.Bots[0].Settings["enabled"]; ok {
			t.Fatalf("enabled 不应落入 Settings: %+v", cfg.Bots[0].Settings)
		}
	})

	t.Run("环境变量覆盖", func(t *testing.T) {
		cfg := mustLoadBytes(t, `
bots:
  - name: mock-main
    adapter: mock
`, "KEI_BOTS_MOCK_MAIN_ENABLED=false")
		if cfg.Bots[0].IsEnabled() {
			t.Fatal("环境变量应能停用实例")
		}

		cfg = mustLoadBytes(t, `
bots:
  - name: mock-main
    adapter: mock
    enabled: false
`, "KEI_BOTS_MOCK_MAIN_ENABLED=true")
		if !cfg.Bots[0].IsEnabled() {
			t.Fatal("环境变量应能重新启用实例")
		}
	})

	t.Run("enabled 非布尔报错", func(t *testing.T) {
		_, err := LoadBytes([]byte(`
bots:
  - name: mock-main
    adapter: mock
    enabled: 也许
`), nil)
		if err == nil || !strings.Contains(err.Error(), "bots[0].enabled") {
			t.Fatalf("错误 = %v", err)
		}
	})
}
