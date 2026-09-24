package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/RandomLemon/kei/internal/adaptermgr"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/grpcsrv"
	"github.com/RandomLemon/kei/internal/pluginmgr/external"
	"github.com/RandomLemon/kei/pkg/bot"
)

const defaultExternalTimeout = 10 * time.Second

// externalPluginSpec 是一个外部插件在配置中的描述。
type externalPluginSpec struct {
	name        string
	addr        string
	token       string
	timeout     time.Duration
	permissions []bot.Permission
	settings    map[string]any
}

// externalSetup 持有外部插件通道的装配钩子与清理函数。
type externalSetup struct {
	hook  func(api bot.BotAPI) ([]bot.Plugin, error)
	close func()
}

// setupExternal 解析配置中的外部插件与外部适配器；都没有配置时返回空装配。
//
// 外部插件通过 `plugins.<name>.grpc_addr` 声明，外部适配器通过
// `adapters.<name>.grpc_addr` 声明：核心启动 BotService gRPC 服务（供两者反向
// 调用），并以客户端身份连接插件进程与适配器进程。外部适配器的事件经
// grpcsrv.EmitEvent 进入事件总线，归属校验由 bindings.EmitFunc 完成。
func setupExternal(cfg *config.Config, logger *slog.Logger, httpClient *http.Client, bindings *adaptermgr.Bindings) (*externalSetup, error) {
	specs, err := externalSpecs(cfg)
	if err != nil {
		return nil, err
	}
	// 只有「启用的外部适配器」才需要 gRPC 通道：adapters 段里仅写 enabled 的
	// 进程内声明（含 enabled: false）与通道无关，不能因为它要求 grpc.addr。
	externalAdapters := externalAdapterNames(cfg)
	if len(specs) == 0 && len(externalAdapters) == 0 {
		return &externalSetup{close: func() {}}, nil
	}
	if cfg.Grpc.Addr == "" {
		return nil, errors.New("config: grpc.addr is required when external plugins or adapters are enabled")
	}

	serverTLS, err := serverTLSConfig(cfg.Grpc)
	if err != nil {
		return nil, err
	}
	clientTLS, err := clientTLSConfig(cfg.Grpc)
	if err != nil {
		return nil, err
	}

	var (
		server  *grpcsrv.Server
		plugins []*external.Plugin
	)

	hook := func(api bot.BotAPI) ([]bot.Plugin, error) {
		tokens := make(map[string]grpcsrv.TokenInfo, len(specs)+len(externalAdapters))
		configs := make(map[string]*bot.Config, len(specs)+len(externalAdapters))
		for _, s := range specs {
			if err := bindToken(tokens, configs, s.token, s.name, grpcsrv.TokenPlugin, s.permissions, s.settings); err != nil {
				return nil, err
			}
		}
		for _, name := range externalAdapters {
			ac := cfg.Adapters[name]
			if err := bindToken(tokens, configs, ac.Token, name, grpcsrv.TokenAdapter, adaptermgr.Permissions(ac.Permissions), ac.Settings); err != nil {
				return nil, err
			}
		}

		emit, err := bindings.EmitFunc(api)
		if err != nil {
			return nil, err
		}

		server, err = grpcsrv.New(grpcsrv.Options{
			Addr:      cfg.Grpc.Addr,
			Tokens:    tokens,
			Bot:       api,
			Configs:   configs,
			Logger:    logger,
			TLS:       serverTLS,
			EmitEvent: emit,
		})
		if err != nil {
			return nil, err
		}
		if err := server.Start(); err != nil {
			return nil, err
		}
		logger.Info("bot service listening",
			"addr", server.Addr(),
			"tls", serverTLS != nil,
			"external_plugins", len(specs),
			"external_adapters", len(cfg.Adapters),
		)

		out := make([]bot.Plugin, 0, len(specs))
		for _, s := range specs {
			p, err := external.New(external.Config{
				Name:        s.name,
				Addr:        s.addr,
				CoreAddr:    server.Addr(),
				Token:       s.token,
				Timeout:     s.timeout,
				Permissions: s.permissions,
				Settings:    s.settings,
				TLS:         clientTLS,
			}, external.Deps{
				Bot:        api,
				Logger:     logger,
				Config:     configs[s.name],
				HTTPClient: httpClient,
			})
			if err != nil {
				return nil, fmt.Errorf("external plugin %s: %w", s.name, err)
			}
			plugins = append(plugins, p)
			out = append(out, p)
		}
		return out, nil
	}

	closer := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, p := range plugins {
			if err := p.Close(); err != nil {
				logger.Warn("external plugin close failed", "plugin", p.Name(), "error", err)
			}
		}
		if server != nil {
			if err := server.Stop(ctx); err != nil {
				logger.Warn("bot service stop failed", "error", err)
			}
		}
	}
	return &externalSetup{hook: hook, close: closer}, nil
}

// bindToken 登记一个外部进程（插件或适配器）的令牌与配置。
//
// 令牌重复或插件/适配器重名都会让启动失败：令牌是核心侧唯一的身份凭据，
// 静默覆盖会让某个外部进程冒充另一个。
func bindToken(tokens map[string]grpcsrv.TokenInfo, configs map[string]*bot.Config, token, name string, kind grpcsrv.TokenKind, perms []bot.Permission, settings map[string]any) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("config: %s %s 需要非空 token", kind, name)
	}
	if _, dup := tokens[token]; dup {
		return fmt.Errorf("config: token 重复（%s）", name)
	}
	if _, dup := configs[name]; dup {
		return fmt.Errorf("config: 插件与适配器同名 %q", name)
	}
	tokens[token] = grpcsrv.TokenInfo{Name: name, Kind: kind, Permissions: perms}
	configs[name] = bot.NewConfig(settings)
	return nil
}

// externalAdapterNames 返回「启用的外部适配器」名字（排序，保证装配顺序确定）。
//
// 判定外部通道的唯一依据是 grpc_addr 非空；仅有 enabled 的进程内声明不在此列，
// 被 enabled: false 禁用的适配器同样不在此列（既不需要通道，也不该拿到令牌）。
func externalAdapterNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Adapters))
	for name, ac := range cfg.Adapters {
		if ac.GrpcAddr == "" || !ac.IsEnabled() {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// externalSpecs 从插件配置中提取外部插件描述，并按名字排序保证启动顺序确定。
func externalSpecs(cfg *config.Config) ([]externalPluginSpec, error) {
	names := make([]string, 0, len(cfg.Plugins))
	for name, pc := range cfg.Plugins {
		if !pc.Enabled {
			continue
		}
		addr, _ := pc.Settings["grpc_addr"].(string)
		if strings.TrimSpace(addr) == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	specs := make([]externalPluginSpec, 0, len(names))
	for _, name := range names {
		pc := cfg.Plugins[name]
		spec := externalPluginSpec{
			name:     name,
			token:    settingStringOf(pc.Settings, "token", ""),
			addr:     settingStringOf(pc.Settings, "grpc_addr", ""),
			timeout:  settingDurationOf(pc.Settings, "timeout", defaultExternalTimeout),
			settings: filterSettings(pc.Settings, "grpc_addr", "token", "permissions", "timeout"),
		}
		if spec.token == "" {
			return nil, fmt.Errorf("config: plugin %s needs a non-empty token", name)
		}
		spec.permissions = parsePermissions(pc.Settings["permissions"])
		specs = append(specs, spec)
	}
	return specs, nil
}

// parsePermissions 解析外部插件权限，缺省为 send_message。
func parsePermissions(v any) []bot.Permission {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		if str, ok := v.(string); ok && strings.TrimSpace(str) != "" {
			list = []any{}
			for _, part := range strings.Split(str, ",") {
				list = append(list, strings.TrimSpace(part))
			}
		}
	}
	if len(list) == 0 {
		return []bot.Permission{bot.PermSendMessage}
	}
	out := make([]bot.Permission, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, bot.Permission(strings.TrimSpace(s)))
		}
	}
	if len(out) == 0 {
		return []bot.Permission{bot.PermSendMessage}
	}
	return out
}

// filterSettings 去掉外部通道自身的配置键，其余作为插件配置传给插件。
func filterSettings(settings map[string]any, drop ...string) map[string]any {
	out := make(map[string]any, len(settings))
	for k, v := range settings {
		skip := false
		for _, d := range drop {
			if k == d {
				skip = true
				break
			}
		}
		if !skip {
			out[k] = v
		}
	}
	return out
}

func settingStringOf(settings map[string]any, key, def string) string {
	v, ok := settings[key]
	if !ok || v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func settingDurationOf(settings map[string]any, key string, def time.Duration) time.Duration {
	v, ok := settings[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case time.Duration:
		return t
	case string:
		if d, err := time.ParseDuration(t); err == nil {
			return d
		}
	case int:
		return time.Duration(t) * time.Second
	case float64:
		return time.Duration(t * float64(time.Second))
	}
	return def
}

// serverTLSConfig 依据 grpc 配置构造服务端 TLS（配置了 ca_file 时启用 mTLS）。
func serverTLSConfig(c config.GrpcConfig) (*tls.Config, error) {
	if c.CertFile == "" || c.KeyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("config: load grpc server keypair: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pool, err := loadCertPool(c.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// clientTLSConfig 构造连接外部插件时的客户端 TLS。
func clientTLSConfig(c config.GrpcConfig) (*tls.Config, error) {
	if c.CAFile == "" && c.CertFile == "" {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pool, err := loadCertPool(c.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" && c.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("config: load grpc client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read ca file %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("config: ca file %s contains no certificate", path)
	}
	return pool, nil
}
