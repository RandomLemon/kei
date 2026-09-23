package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/pkg/bot"
)

func testLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func TestBuildAdapter(t *testing.T) {
	logger, _ := testLogger()
	httpClient := &http.Client{Timeout: time.Second}

	tests := []struct {
		name       string
		cfg        config.BotConfig
		wantErr    bool
		wantPlatfo string
	}{
		{
			name:       "mock",
			cfg:        config.BotConfig{Name: "mock-main", Adapter: "mock", Settings: map[string]any{"platform": "mock"}},
			wantPlatfo: "mock",
		},
		{
			name: "onebot",
			cfg: config.BotConfig{Name: "qq-main", Adapter: "onebot", Settings: map[string]any{
				"api_url": "http://127.0.0.1:3000", "listen_addr": "127.0.0.1:0",
			}},
			wantPlatfo: "onebot",
		},
		{
			name: "feishu",
			cfg: config.BotConfig{Name: "feishu-main", Adapter: "feishu", Settings: map[string]any{
				"app_id": "cli_x", "app_secret": "s", "verification_token": "t", "listen_addr": "127.0.0.1:0",
			}},
			wantPlatfo: "feishu",
		},
		{
			name:    "缺少必填项",
			cfg:     config.BotConfig{Name: "qq-main", Adapter: "onebot"},
			wantErr: true,
		},
		{
			name:    "未实现的适配器",
			cfg:     config.BotConfig{Name: "wecom-main", Adapter: "wecom"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ad, err := buildAdapter(tc.cfg, logger, httpClient)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望错误，得到 %v", ad)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildAdapter: %v", err)
			}
			if ad.Name() != tc.wantPlatfo {
				t.Fatalf("平台名 = %q, want %q", ad.Name(), tc.wantPlatfo)
			}
		})
	}
}

func TestBuildAdapterWarnsAboutUnknownSettings(t *testing.T) {
	logger, buf := testLogger()
	cfg := config.BotConfig{Name: "mock-main", Adapter: "mock", Settings: map[string]any{
		"platform": "mock",
		"typo_key": "x",
	}}
	if _, err := buildAdapter(cfg, logger, http.DefaultClient); err != nil {
		t.Fatalf("buildAdapter: %v", err)
	}
	if !strings.Contains(buf.String(), "typo_key") {
		t.Fatalf("未提示未知配置键: %s", buf.String())
	}
}

func TestEnabledPlugins(t *testing.T) {
	logger, buf := testLogger()
	cfg := &config.Config{Plugins: map[string]config.PluginConfig{
		"echo":       {Enabled: true},
		"weather":    {Enabled: false},
		"not-loaded": {Enabled: true},
		"external-x": {Enabled: true, Settings: map[string]any{"grpc_addr": "127.0.0.1:1"}},
		"manage":     {Enabled: true},
		"unlisted":   {Enabled: false},
	}}

	plugins := enabledPlugins(cfg, logger)
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Metadata().Name)
	}
	got := strings.Join(names, ",")
	// 只有已注册且启用的内置插件被加载；weather 被显式禁用，未注册与外部插件都不加载。
	if !strings.Contains(got, "echo") || !strings.Contains(got, "manage") {
		t.Fatalf("已启用插件 = %v", names)
	}
	if strings.Contains(got, "weather") || strings.Contains(got, "not-loaded") {
		t.Fatalf("不应加载被禁用/未注册的插件: %v", names)
	}
	if !strings.Contains(buf.String(), "not registered") {
		t.Fatalf("未提示未注册的插件: %s", buf.String())
	}
}

func TestParsePermissions(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want []bot.Permission
	}{
		{name: "缺省为发送消息", in: nil, want: []bot.Permission{bot.PermSendMessage}},
		{name: "列表", in: []any{"send_message", "storage"}, want: []bot.Permission{bot.PermSendMessage, bot.PermStorage}},
		{name: "逗号分隔字符串", in: "network, storage", want: []bot.Permission{bot.PermNetwork, bot.PermStorage}},
		{name: "空列表回退默认", in: []any{}, want: []bot.Permission{bot.PermSendMessage}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePermissions(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parsePermissions(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parsePermissions(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestExternalSpecs(t *testing.T) {
	cfg := &config.Config{Plugins: map[string]config.PluginConfig{
		"ext-a": {Enabled: true, Settings: map[string]any{
			"grpc_addr":   "127.0.0.1:19071",
			"token":       "tok-a",
			"timeout":     "3s",
			"greeting":    "你好",
			"permissions": "send_message,network",
		}},
		"ext-disabled": {Enabled: false, Settings: map[string]any{"grpc_addr": "127.0.0.1:1", "token": "x"}},
		"in-process":   {Enabled: true, Settings: map[string]any{"api_key": "k"}},
	}}

	specs, err := externalSpecs(cfg)
	if err != nil {
		t.Fatalf("externalSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("外部插件数量 = %d, want 1", len(specs))
	}
	spec := specs[0]
	if spec.name != "ext-a" || spec.addr != "127.0.0.1:19071" || spec.token != "tok-a" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.timeout != 3*time.Second {
		t.Fatalf("timeout = %v, want 3s", spec.timeout)
	}
	if len(spec.permissions) != 2 || spec.permissions[0] != bot.PermSendMessage || spec.permissions[1] != bot.PermNetwork {
		t.Fatalf("permissions = %v", spec.permissions)
	}
	// 通道自身的配置键不得透传给插件进程。
	if _, ok := spec.settings["grpc_addr"]; ok {
		t.Fatal("grpc_addr 不应出现在插件配置里")
	}
	if _, ok := spec.settings["token"]; ok {
		t.Fatal("token 不应出现在插件配置里")
	}
	if got := spec.settings["greeting"]; got != "你好" {
		t.Fatalf("插件配置 = %v", spec.settings)
	}
}

func TestExternalSpecsRequireToken(t *testing.T) {
	cfg := &config.Config{Plugins: map[string]config.PluginConfig{
		"ext-a": {Enabled: true, Settings: map[string]any{"grpc_addr": "127.0.0.1:1"}},
	}}
	if _, err := externalSpecs(cfg); err == nil {
		t.Fatal("缺少 token 应返回错误")
	}
}

func TestSetupExternalWithoutPlugins(t *testing.T) {
	logger, _ := testLogger()
	setup, err := setupExternal(&config.Config{}, logger, http.DefaultClient)
	if err != nil {
		t.Fatalf("setupExternal: %v", err)
	}
	if setup.hook != nil {
		t.Fatal("没有外部插件时不应提供装配钩子")
	}
	setup.close() // 不得 panic
}

func TestSetupExternalRequiresGrpcAddr(t *testing.T) {
	logger, _ := testLogger()
	cfg := &config.Config{Plugins: map[string]config.PluginConfig{
		"ext-a": {Enabled: true, Settings: map[string]any{"grpc_addr": "127.0.0.1:1", "token": "t"}},
	}}
	if _, err := setupExternal(cfg, logger, http.DefaultClient); err == nil {
		t.Fatal("缺少 grpc.addr 应返回错误")
	}
}

func TestTLSConfigs(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedCert(t, dir)

	t.Run("无 TLS 配置时返回 nil", func(t *testing.T) {
		cfg, err := serverTLSConfig(config.GrpcConfig{})
		if err != nil || cfg != nil {
			t.Fatalf("serverTLSConfig = %v, %v", cfg, err)
		}
		clientCfg, err := clientTLSConfig(config.GrpcConfig{})
		if err != nil || clientCfg != nil {
			t.Fatalf("clientTLSConfig = %v, %v", clientCfg, err)
		}
	})

	t.Run("服务端 TLS", func(t *testing.T) {
		cfg, err := serverTLSConfig(config.GrpcConfig{CertFile: certPath, KeyFile: keyPath})
		if err != nil {
			t.Fatalf("serverTLSConfig: %v", err)
		}
		if cfg == nil || len(cfg.Certificates) != 1 {
			t.Fatalf("证书未加载: %+v", cfg)
		}
		if cfg.ClientAuth != 0 {
			t.Fatal("未配置 CA 时不应要求客户端证书")
		}
	})

	t.Run("mTLS 要求客户端证书", func(t *testing.T) {
		cfg, err := serverTLSConfig(config.GrpcConfig{CertFile: certPath, KeyFile: keyPath, CAFile: certPath})
		if err != nil {
			t.Fatalf("serverTLSConfig: %v", err)
		}
		if cfg.ClientCAs == nil || cfg.ClientAuth == 0 {
			t.Fatalf("mTLS 配置不完整: %+v", cfg)
		}

		clientCfg, err := clientTLSConfig(config.GrpcConfig{CAFile: certPath, CertFile: certPath, KeyFile: keyPath})
		if err != nil {
			t.Fatalf("clientTLSConfig: %v", err)
		}
		if clientCfg.RootCAs == nil || len(clientCfg.Certificates) != 1 {
			t.Fatalf("客户端 TLS 配置不完整: %+v", clientCfg)
		}
	})

	t.Run("证书文件不存在时报错", func(t *testing.T) {
		if _, err := serverTLSConfig(config.GrpcConfig{CertFile: filepath.Join(dir, "nope.pem"), KeyFile: keyPath}); err == nil {
			t.Fatal("缺失证书文件应返回错误")
		}
		if _, err := clientTLSConfig(config.GrpcConfig{CAFile: filepath.Join(dir, "nope.pem")}); err == nil {
			t.Fatal("缺失 CA 文件应返回错误")
		}
	})
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "configs", "config.yaml"))
	if err != nil {
		t.Fatalf("示例配置无法加载: %v", err)
	}
	if len(cfg.Bots) == 0 {
		t.Fatal("示例配置应至少配置一个 bot")
	}
	adapters := map[string]bool{}
	for _, b := range cfg.Bots {
		adapters[b.Adapter] = true
	}
	for _, want := range []string{"mock", "feishu", "onebot"} {
		if !adapters[want] {
			t.Fatalf("示例配置缺少 %s 适配器示例: %v", want, adapters)
		}
	}
	for _, name := range []string{"echo", "weather", "manage"} {
		if pc := cfg.Plugin(name); !pc.Enabled {
			t.Fatalf("示例配置应启用插件 %s", name)
		}
	}

	// 示例配置里的 mock bot 必须是 mock 适配器可用的设置。
	var mockBot config.BotConfig
	for _, b := range cfg.Bots {
		if b.Adapter == "mock" {
			mockBot = b
		}
	}
	// README 里的 curl 联调流程依赖 mock bot 的 listen_addr 与 platform 设置。
	if got := settingString(mockBot, "listen_addr", ""); got == "" {
		t.Fatal("示例配置的 mock bot 必须配置 listen_addr")
	}
	if got := settingString(mockBot, "platform", "mock"); got != "mock" {
		t.Fatalf("mock bot 的 platform = %q, want mock", got)
	}
}

func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kei-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("序列化密钥: %v", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("写证书: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("写私钥: %v", err)
	}
	return certPath, keyPath
}
