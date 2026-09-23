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

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/pluginmgr/external"
	"github.com/RandomLemon/kei/internal/pluginmgr/grpcsrv"
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

// setupExternal 解析配置中的外部插件；没有配置时返回空装配。
//
// 外部插件通过 `plugins.<name>.grpc_addr` 声明：核心启动 BotService gRPC 服务
// （供插件回调发送消息），并以客户端身份连接插件进程（调用其 HandleEvent）。
func setupExternal(cfg *config.Config, logger *slog.Logger, httpClient *http.Client) (*externalSetup, error) {
	specs, err := externalSpecs(cfg)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return &externalSetup{close: func() {}}, nil
	}
	if cfg.Grpc.Addr == "" {
		return nil, errors.New("config: grpc.addr is required when external plugins are enabled")
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
		tokens := make(map[string]grpcsrv.TokenInfo, len(specs))
		configs := make(map[string]*bot.Config, len(specs))
		for _, s := range specs {
			tokens[s.token] = grpcsrv.TokenInfo{Plugin: s.name, Permissions: s.permissions}
			configs[s.name] = bot.NewConfig(s.settings)
		}

		server, err = grpcsrv.New(grpcsrv.Options{
			Addr:    cfg.Grpc.Addr,
			Tokens:  tokens,
			Bot:     api,
			Configs: configs,
			Logger:  logger,
			TLS:     serverTLS,
		})
		if err != nil {
			return nil, err
		}
		if err := server.Start(); err != nil {
			return nil, err
		}
		logger.Info("bot service listening", "addr", server.Addr(), "tls", serverTLS != nil)

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
