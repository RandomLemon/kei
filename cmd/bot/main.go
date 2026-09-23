// Command bot 是 kei 框架的入口程序。
//
// 职责：加载配置 -> 初始化日志/指标/存储 -> 构造适配器 -> 收集已启用的
// 编译期插件与外部插件 -> 启动引擎 -> 收到信号后优雅退出。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/RandomLemon/kei/adapters/feishu"
	"github.com/RandomLemon/kei/adapters/mock"
	"github.com/RandomLemon/kei/adapters/onebot"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/engine"
	"github.com/RandomLemon/kei/internal/metrics"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"

	// 内置插件通过空导入注册到全局插件表，是否启用由配置决定。
	_ "github.com/RandomLemon/kei/plugins/echo"
	_ "github.com/RandomLemon/kei/plugins/manage"
	_ "github.com/RandomLemon/kei/plugins/weather"
)

const httpTimeout = 15 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bot:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("bot", flag.ContinueOnError)
	configPath := fs.String("config", "configs/config.yaml", "配置文件路径")
	showVersion := fs.Bool("version", false, "打印版本并退出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Printf("kei v%s\n", bot.Version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := storage.NewMemory()
	defer func() { _ = store.Close() }()

	registry := metrics.New()
	if cfg.Metrics.Addr != "" {
		if err := serveMetrics(ctx, cfg.Metrics.Addr, registry.Handler(), logger); err != nil {
			return err
		}
	}

	httpClient := &http.Client{Timeout: httpTimeout}

	bindings := make([]engine.AdapterBinding, 0, len(cfg.Bots))
	for _, bc := range cfg.Bots {
		ad, err := buildAdapter(bc, logger, httpClient)
		if err != nil {
			return fmt.Errorf("bot %s: %w", bc.Name, err)
		}
		bindings = append(bindings, engine.AdapterBinding{BotID: bc.Name, Adapter: ad})
	}

	plugins := enabledPlugins(cfg, logger)

	external, err := setupExternal(cfg, logger, httpClient)
	if err != nil {
		return err
	}
	defer external.close()

	eng, err := engine.New(engine.Options{
		Config:          cfg,
		Logger:          logger,
		Metrics:         registry,
		Storage:         store,
		HTTPClient:      httpClient,
		Adapters:        bindings,
		Plugins:         plugins,
		ExternalPlugins: external.hook,
	})
	if err != nil {
		return err
	}

	logger.Info("starting kei",
		"version", bot.Version,
		"config", *configPath,
		"bots", len(cfg.Bots),
		"plugins", len(plugins),
	)
	return eng.Run(ctx)
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

// mockSettings 等常量定义各适配器接受的配置键，用于拼写检查。
var adapterSettings = map[string][]string{
	"mock":   {"platform", "listen_addr"},
	"onebot": {"api_url", "listen_addr", "path", "secret", "access_token", "self_id"},
	"feishu": {"app_id", "app_secret", "verification_token", "encrypt_key", "listen_addr", "path", "base_url"},
}

// buildAdapter 依据配置构造平台适配器。
func buildAdapter(bc config.BotConfig, logger *slog.Logger, httpClient *http.Client) (bot.Adapter, error) {
	st := func(key, def string) string { return settingString(bc, key, def) }
	known, ok := adapterSettings[bc.Adapter]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q", bc.Adapter)
	}
	warnUnknownSettings(bc, known, logger)

	switch bc.Adapter {
	case "mock":
		return mock.New(mock.Options{
			Name:       bc.Name,
			Platform:   st("platform", "mock"),
			ListenAddr: st("listen_addr", ""),
			Logger:     logger,
		}), nil
	case "onebot":
		return onebot.New(onebot.Options{
			Name:        bc.Name,
			APIURL:      st("api_url", ""),
			ListenAddr:  st("listen_addr", ""),
			Path:        st("path", ""),
			Secret:      st("secret", ""),
			AccessToken: st("access_token", ""),
			SelfID:      st("self_id", ""),
			HTTPClient:  httpClient,
			Logger:      logger,
		})
	case "feishu":
		return feishu.New(feishu.Options{
			Name:              bc.Name,
			AppID:             st("app_id", ""),
			AppSecret:         st("app_secret", ""),
			VerificationToken: st("verification_token", ""),
			EncryptKey:        st("encrypt_key", ""),
			ListenAddr:        st("listen_addr", ""),
			Path:              st("path", ""),
			BaseURL:           st("base_url", ""),
			HTTPClient:        httpClient,
			Logger:            logger,
		})
	default:
		return nil, fmt.Errorf("adapter %q is not implemented", bc.Adapter)
	}
}

// enabledPlugins 返回配置中启用、且已经通过空导入注册的编译期插件。
func enabledPlugins(cfg *config.Config, logger *slog.Logger) []bot.Plugin {
	registered := bot.RegisteredPlugins()
	seen := make(map[string]bool, len(registered))

	var out []bot.Plugin
	for _, p := range registered {
		meta := p.Metadata()
		seen[meta.Name] = true

		pc, ok := cfg.Plugins[meta.Name]
		if external, _ := pc.Settings["grpc_addr"].(string); ok && external != "" {
			logger.Info("plugin is configured as external, skipping in-process", "plugin", meta.Name)
			continue
		}
		if !ok || !pc.Enabled {
			logger.Info("plugin disabled", "plugin", meta.Name)
			continue
		}
		out = append(out, p)
	}
	for name, pc := range cfg.Plugins {
		if pc.Enabled && !seen[name] {
			if external, _ := pc.Settings["grpc_addr"].(string); external == "" {
				logger.Warn("enabled plugin is not registered", "plugin", name)
			}
		}
	}
	return out
}

func settingString(bc config.BotConfig, key, def string) string {
	v, ok := bc.Settings[key]
	if !ok || v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func warnUnknownSettings(bc config.BotConfig, known []string, logger *slog.Logger) {
	for key := range bc.Settings {
		found := false
		for _, k := range known {
			if k == key {
				found = true
				break
			}
		}
		if !found {
			logger.Warn("unknown adapter setting", "bot", bc.Name, "adapter", bc.Adapter, "key", key)
		}
	}
}
