// Package kei 是 kei 的装配门面：加载 YAML 配置、装配适配器与插件、启动引擎并完成
// 优雅退出。它是 cmd/bot 的全部装配逻辑，供嵌入式使用（不写 YAML 之外的第二套配置）：
//
//	import (
//		"context"
//		kei "github.com/RandomLemon/kei/pkg/kei"
//		_ "github.com/RandomLemon/kei/adapters/onebot" // 空导入注册适配器
//		_ "github.com/RandomLemon/kei/plugins/echo"    // 空导入注册插件
//	)
//
//	func main() {
//		if err := kei.Run(context.Background(), kei.Options{ConfigFile: "configs/bot.yaml"}); err != nil {
//			log.Fatal(err)
//		}
//	}
//
// 启停顺序与配置语义与 cmd/bot 完全一致：适配器与插件都经各自注册表装配，启用由配置
// 决定；本包只做装配，不含平台名分支与业务逻辑，也不空导入任何适配器或插件。
package kei

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/RandomLemon/kei/internal/adaptermgr"
	"github.com/RandomLemon/kei/internal/engine"
	"github.com/RandomLemon/kei/internal/metrics"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

// defaultHTTPTimeout 是门面为适配器与插件创建的默认 HTTP 客户端超时。
const defaultHTTPTimeout = 15 * time.Second

// Options 是 Run 的启动参数。
type Options struct {
	// ConfigFile 是 YAML 配置路径；与 Config 二选一必填。
	ConfigFile string
	// Config 是 YAML 配置内容（适合 go:embed）；与 ConfigFile 二选一必填。
	Config []byte
	// Plugins 是直接注入的插件实例（如 *bot.FuncPlugin）。注入的实例一律启用：
	// 配置里没有同名键时补一条 enabled: true；同名键必须 enabled，其设置键原样
	// 传给实例。
	Plugins []bot.Plugin
	// Logger 是根日志器；nil 时按配置的 log.level/log.format 新建（并设为 slog 默认）。
	Logger *slog.Logger
	// HTTPClient 是适配器与插件共用的 HTTP 客户端；nil 时为 15s 超时的客户端。
	HTTPClient *http.Client
	// Storage 是插件存储；nil 时新建内存实现并在 Run 返回前关闭。
	// 传入的 Storage 由调用方拥有，Run 不关闭它。
	Storage bot.Storage
}

// Run 装配并运行 chatbot，阻塞直到 ctx 结束或适配器发生不可恢复错误。
// 返回前完成优雅关闭（停止接收 → 排空已入队事件 → 停止适配器 → 停止插件）。
func Run(ctx context.Context, opts Options) error {
	cfg, err := buildConfig(opts)
	if err != nil {
		return err
	}

	logger := opts.Logger
	if logger == nil {
		logger = newLogger(cfg.Log)
	}
	slog.SetDefault(logger)

	store := opts.Storage
	if store == nil {
		owned := storage.NewMemory()
		defer func() { _ = owned.Close() }()
		store = owned
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	registry := metrics.New()
	if cfg.Metrics.Addr != "" {
		if err := serveMetrics(ctx, cfg.Metrics.Addr, registry.Handler(), logger); err != nil {
			return err
		}
	}

	// 适配器一律经注册表装配：本包不出现任何平台名分支。
	bindings, err := adaptermgr.Build(cfg, adaptermgr.Deps{
		Logger:     logger,
		Storage:    store,
		HTTPClient: httpClient,
	})
	if err != nil {
		return err
	}

	list := bindings.List()
	if len(list) == 0 {
		logger.Warn("没有启用的适配器，核心将只运行插件")
	}
	adapters := make([]engine.AdapterBinding, 0, len(list))
	for _, b := range list {
		adapters = append(adapters, engine.AdapterBinding{
			BotID:    b.BotID,
			Adapter:  b.Adapter,
			Metadata: b.Info.Metadata,
		})
	}

	plugins, err := selectPlugins(cfg, opts.Plugins, logger)
	if err != nil {
		return err
	}

	eng, err := engine.New(engine.Options{
		Config:     cfg,
		Logger:     logger,
		Metrics:    registry,
		Storage:    store,
		HTTPClient: httpClient,
		Adapters:   adapters,
		Plugins:    plugins,
	})
	if err != nil {
		return err
	}

	source := opts.ConfigFile
	if source == "" {
		source = "<inline>"
	}
	logger.Info("starting kei",
		"version", bot.Version,
		"config", source,
		"bots", len(adapters),
		"plugins", len(plugins),
	)
	return eng.Run(ctx)
}
