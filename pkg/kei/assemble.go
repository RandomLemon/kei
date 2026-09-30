package kei

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

// buildStorage 解析存储：Options.Storage 显式注入优先；否则按 cfg.Storage 构造。
//
// 第二返回值是关闭自有存储的清理函数（注入时为 no-op），总是非 nil。
func buildStorage(opts Options, cfg *config.Config) (bot.Storage, func(), error) {
	if opts.Storage != nil {
		return opts.Storage, func() {}, nil
	}
	params := make(map[string]any, len(cfg.Storage.Params)+1)
	for k, v := range cfg.Storage.Params {
		params[k] = v
	}
	if cfg.Storage.DSN != "" {
		params["dsn"] = cfg.Storage.DSN
	}
	store, err := storage.Open(cfg.Storage.Type, bot.NewConfig(params))
	if err != nil {
		return nil, nil, fmt.Errorf("kei: 初始化 storage: %w", err)
	}
	closeStore := func() {}
	if c, ok := store.(interface{ Close() error }); ok {
		closeStore = func() { _ = c.Close() }
	}
	return store, closeStore, nil
}

// buildConfig 解析配置来源；两个来源互斥且必填其一。
//
// 两种来源都走 internal/config 的同一套解码/默认值/校验/环境变量覆盖，
// 因此校验错误文案原样透出，不做二次包装。
func buildConfig(opts Options) (*config.Config, error) {
	if len(opts.ConfigFile) > 0 && len(opts.Config) > 0 {
		return nil, errors.New("kei: ConfigFile 与 Config 不能同时设置")
	}
	if len(opts.Config) > 0 {
		return config.LoadBytes(opts.Config, os.Environ())
	}
	if len(opts.ConfigFile) > 0 {
		return config.Load(opts.ConfigFile)
	}
	return nil, errors.New("kei: 必须提供 ConfigFile 或 Config")
}

// selectPlugins 返回需要加载的进程内插件：先注入实例（Options 顺序），再是配置启用
// 且已注册的插件（注册顺序）。返回顺序即 Setup 顺序（Stop 逆序）。
//
// 注入实例一律启用：同名配置键必须 enabled；不存在的键补一条 enabled: true，使
// 后续「已启用但未注册」的告警不会对注入实例误报。同名时注入优先，注册表实例跳过
// 而不是报错——注入是更明确的意图。
func selectPlugins(cfg *config.Config, injected []bot.Plugin, logger *slog.Logger) ([]bot.Plugin, error) {
	injectedNames := make(map[string]bool, len(injected))
	for i, p := range injected {
		if p == nil {
			return nil, fmt.Errorf("kei: plugins[%d]: 插件实例为 nil", i)
		}
		name := p.Metadata().Name
		if name == "" {
			return nil, fmt.Errorf("kei: plugins[%d]: 插件名为空", i)
		}
		if injectedNames[name] {
			return nil, fmt.Errorf("kei: 插件名 %q 重复", name)
		}
		injectedNames[name] = true

		pc, ok := cfg.Plugins[name]
		if ok && !pc.Enabled {
			return nil, fmt.Errorf("kei: 插件 %q 在配置中 enabled: false，与注入的实例冲突", name)
		}
		if !ok {
			cfg.Plugins[name] = config.PluginConfig{Enabled: true}
		}
	}

	out := make([]bot.Plugin, 0, len(injected)+len(cfg.Plugins))
	out = append(out, injected...)

	registered := bot.RegisteredPlugins()
	seen := make(map[string]bool, len(registered))
	for _, p := range registered {
		name := p.Metadata().Name
		seen[name] = true

		if injectedNames[name] {
			logger.Info("插件已被注入实例覆盖，跳过注册表实例", "plugin", name)
			continue
		}
		pc, ok := cfg.Plugins[name]
		if !ok || !pc.Enabled {
			logger.Info("plugin disabled", "plugin", name)
			continue
		}
		out = append(out, p)
	}
	for name, pc := range cfg.Plugins {
		if pc.Enabled && !seen[name] && !injectedNames[name] {
			logger.Warn("enabled plugin is not registered", "plugin", name)
		}
	}
	return out, nil
}

// newLogger 依据配置构造结构化日志器。
func newLogger(c config.LogConfig) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(c.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if strings.EqualFold(c.Format, "json") {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

// sensitiveEnvMarkers 是判定环境变量取值是否敏感的关键字（比较前转大写）。
// 命中时 DEBUG 日志只输出掩码，避免密钥类配置被写进日志。
var sensitiveEnvMarkers = []string{
	"SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIAL",
	"API_KEY", "APIKEY", "ACCESS_KEY", "PRIVATE_KEY", "ENCRYPT_KEY",
}

// logEnvOverrides 以 DEBUG 逐条输出本次启动实际生效的环境变量覆盖，
// 便于排查某个配置项来自 YAML 还是环境变量。字段：env（变量名）、
// config（配置项路径）、value（取值；敏感变量为掩码）。
func logEnvOverrides(logger *slog.Logger, cfg *config.Config) {
	for _, o := range cfg.EnvOverrides {
		logger.Debug("环境变量覆盖配置",
			"env", o.Key,
			"config", o.Path,
			"value", redactEnvValue(o.Key, o.Value),
		)
	}
}

// redactEnvValue 对敏感环境变量的取值做掩码；其余原样返回。
func redactEnvValue(key, value string) string {
	if value == "" {
		return value
	}
	upper := strings.ToUpper(key)
	for _, marker := range sensitiveEnvMarkers {
		if strings.Contains(upper, marker) {
			return "******"
		}
	}
	return value
}

// serveMetrics 在独立 HTTP 服务上暴露 Prometheus 指标，随 ctx 结束关闭。
func serveMetrics(ctx context.Context, addr string, handler http.Handler, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics listen %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics shutdown incomplete", "error", err)
		}
	}()

	logger.Info("metrics listening", "addr", ln.Addr().String(), "path", "/metrics")
	return nil
}
