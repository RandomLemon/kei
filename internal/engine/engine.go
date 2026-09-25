// Package engine 串联适配器、事件总线、路由与插件，并对外提供 BotAPI。
//
// 启动顺序：注册插件 -> 连接外部插件 -> 初始化适配器 -> 启动事件总线 worker
// -> 启动插件 -> 启动适配器。
// 关闭顺序：停止接收 -> 等待已入队事件处理完 -> 停止适配器 -> 停止插件。
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/dedup"
	"github.com/RandomLemon/kei/internal/eventbus"
	"github.com/RandomLemon/kei/internal/metrics"
	"github.com/RandomLemon/kei/internal/middleware"
	"github.com/RandomLemon/kei/internal/pluginmgr"
	"github.com/RandomLemon/kei/internal/ratelimit"
	"github.com/RandomLemon/kei/internal/reply"
	"github.com/RandomLemon/kei/internal/router"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

// 默认参数。
const (
	defaultEventTimeout    = 30 * time.Second
	defaultRuleTimeout     = 10 * time.Second
	defaultShutdownTimeout = 10 * time.Second
	defaultBusWorkers      = 4
	defaultBusQueueSize    = 256
	defaultDedupTTL        = 5 * time.Minute
	defaultDedupCapacity   = 4096
	defaultSendRetries     = 2
	defaultSendBackoff     = 100 * time.Millisecond
	defaultHTTPTimeout     = 15 * time.Second
)

// AdapterBinding 把一个适配器实例绑定到配置中的 bot 名称。
//
// 同一平台可以有多个 bot（例如两个飞书应用），因此适配器必须显式绑定 BotID。
type AdapterBinding struct {
	// BotID 是配置中的 bot 名称。
	BotID string
	// Adapter 是该 bot 的适配器实例。
	Adapter bot.Adapter
	// Metadata 是适配器的注册元信息，用于管理命令展示；可为零值。
	Metadata bot.AdapterMetadata
	// External 表示该实例由外部 gRPC 适配器进程提供。
	External bool
}

// Options 是引擎的构造参数。
type Options struct {
	// Config 是全局配置，必填；用于 bot 白名单、限流、管理员与插件配置。
	Config *config.Config
	// Logger 是根日志器，nil 时使用 slog.Default()。
	Logger *slog.Logger
	// Metrics 是指标注册表，nil 时内部新建。
	Metrics *metrics.Registry
	// Storage 是插件存储，nil 时内部新建内存实现并在关闭时释放。
	Storage bot.Storage
	// HTTPClient 是插件可用的 HTTP 客户端，nil 时使用默认超时客户端。
	HTTPClient *http.Client
	// Adapters 是所有需要启动的适配器。
	Adapters []AdapterBinding
	// Plugins 是编译期内置插件，按顺序 Setup/Start、逆序 Stop。
	Plugins []bot.Plugin
	// ExternalPlugins 是外部插件装配钩子，在 New 返回前被调用一次，入参是
	// 引擎自身（实现 bot.BotAPI）。cmd/bot 用它在拿到 BotAPI 之后启动
	// BotService gRPC 服务并加载外部插件；返回的插件与 Plugins 一同管理。
	// 为 nil 表示不加载外部插件。
	ExternalPlugins func(api bot.BotAPI) ([]bot.Plugin, error)
	// CommandPrefixes 是命令前缀，默认 ["/"]。
	CommandPrefixes []string

	// EventTimeout 是单条事件的整体处理超时，默认 30s。
	EventTimeout time.Duration
	// RuleTimeout 是单条规则 Handler 的超时，默认 10s。
	RuleTimeout time.Duration
	// ShutdownTimeout 是优雅关闭的总超时，默认 10s。
	ShutdownTimeout time.Duration

	// BusWorkers 是事件总线分片 worker 数，默认 4。
	BusWorkers int
	// BusQueueSize 是每个分片队列容量，默认 256。
	BusQueueSize int
	// DedupTTL 是事件去重时间窗，默认 5m。
	DedupTTL time.Duration
	// DedupCapacity 是去重集合容量上限，默认 4096。
	DedupCapacity int

	// SendRetries 是发送失败后的重试次数（不含首次），默认 2。
	SendRetries int
	// SendBackoff 是发送重试的初始退避，默认 100ms。
	SendBackoff time.Duration
}

// Engine 是框架核心。
type Engine struct {
	opts Options
	cfg  *config.Config
	log  *slog.Logger
	met  *metrics.Registry

	store      bot.Storage
	ownStorage bool
	httpClient *http.Client

	router  *router.Router
	bus     *eventbus.Bus
	plugins *pluginmgr.Manager
	sink    *eventSink

	mu           sync.RWMutex
	adapters     map[string]bot.Adapter
	platforms    map[string][]string
	adapterInfos []bot.AdapterInfo

	sendLimits  map[string]*ratelimit.Limiter
	handleLimit *ratelimit.Limiter
	ruleDedup   *dedup.Set

	runMu   sync.Mutex
	running bool
}

// 确保 Engine 满足 BotAPI、PluginCatalog 与 AdapterCatalog。
var (
	_ bot.BotAPI         = (*Engine)(nil)
	_ bot.PluginCatalog  = (*Engine)(nil)
	_ bot.AdapterCatalog = (*Engine)(nil)
)

// New 构造引擎并完成静态校验（配置、适配器绑定、插件重名）。
//
// 不做任何 IO，适配器与插件的启动都发生在 Run 中。
func New(opts Options) (*Engine, error) {
	if opts.Config == nil {
		return nil, errors.New("engine: nil config")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.New()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if len(opts.CommandPrefixes) == 0 {
		opts.CommandPrefixes = []string{"/"}
	}
	if opts.SendRetries < 0 {
		opts.SendRetries = 0
	}
	if opts.SendBackoff <= 0 {
		opts.SendBackoff = defaultSendBackoff
	}

	e := &Engine{
		opts:       opts,
		cfg:        opts.Config,
		log:        opts.Logger,
		met:        opts.Metrics,
		httpClient: opts.HTTPClient,
		adapters:   make(map[string]bot.Adapter, len(opts.Adapters)),
		platforms:  make(map[string][]string),
		sendLimits: make(map[string]*ratelimit.Limiter),
	}

	for _, b := range opts.Adapters {
		if b.Adapter == nil {
			return nil, fmt.Errorf("engine: adapter binding %q has nil adapter", b.BotID)
		}
		if b.BotID == "" {
			return nil, fmt.Errorf("engine: adapter %s binding has empty bot id", b.Adapter.Name())
		}
		if _, dup := e.adapters[b.BotID]; dup {
			return nil, fmt.Errorf("engine: duplicate bot %q", b.BotID)
		}
		e.adapters[b.BotID] = b.Adapter
		e.platforms[b.Adapter.Name()] = append(e.platforms[b.Adapter.Name()], b.BotID)
		e.adapterInfos = append(e.adapterInfos, bot.AdapterInfo{
			BotID:    b.BotID,
			Metadata: b.Metadata,
			External: b.External,
		})

		if rate := opts.Config.Limits.SendRate; rate > 0 {
			e.sendLimits[b.BotID] = ratelimit.New(rate, opts.Config.Limits.SendBurst, 0)
		}
	}
	for _, ids := range e.platforms {
		sort.Strings(ids)
	}
	if rate := opts.Config.Limits.HandlerRate; rate > 0 {
		e.handleLimit = ratelimit.New(rate, opts.Config.Limits.HandlerBurst, 0)
	}

	if opts.Storage == nil {
		e.store = storage.NewMemory()
		e.ownStorage = true
	} else {
		e.store = opts.Storage
	}

	e.ruleDedup = dedup.New(orInt(opts.DedupCapacity, defaultDedupCapacity), orDuration(opts.DedupTTL, defaultDedupTTL))
	e.router = router.New(router.Options{
		CommandPrefixes: opts.CommandPrefixes,
		Middlewares:     e.globalMiddlewares(),
		Replies: func(_ context.Context, ev *bot.Event) bot.Reply {
			return reply.New(e, ev)
		},
		Logger: e.log,
	})
	e.bus = eventbus.New(eventbus.Options{
		Workers:       orInt(opts.BusWorkers, defaultBusWorkers),
		QueueSize:     orInt(opts.BusQueueSize, defaultBusQueueSize),
		DedupTTL:      orDuration(opts.DedupTTL, defaultDedupTTL),
		DedupCapacity: orInt(opts.DedupCapacity, defaultDedupCapacity),
		Logger:        e.log,
	})
	e.sink = &eventSink{eng: e}
	if err := e.bus.Subscribe(e.handle); err != nil {
		return nil, fmt.Errorf("engine: subscribe event handler: %w", err)
	}

	pluginConfigs := make(map[string]*bot.Config, len(opts.Config.Plugins))
	for name, pc := range opts.Config.Plugins {
		pluginConfigs[name] = bot.NewConfig(pc.Settings)
	}
	var mgr *pluginmgr.Manager
	mgr = pluginmgr.New(pluginmgr.Deps{
		Storage:        e.store,
		HTTPClient:     e.httpClient,
		Logger:         e.log,
		Configs:        pluginConfigs,
		API:            e.pluginAPI,
		Catalog:        func() []bot.Metadata { return mgr.Plugins() },
		AdapterCatalog: func() []bot.AdapterInfo { return e.Adapters() },
	})
	e.plugins = mgr

	for _, p := range opts.Plugins {
		if err := e.plugins.Add(p); err != nil {
			return nil, err
		}
	}
	if opts.ExternalPlugins != nil {
		external, err := opts.ExternalPlugins(e)
		if err != nil {
			return nil, err
		}
		for _, p := range external {
			if err := e.plugins.Add(p); err != nil {
				return nil, err
			}
		}
		e.log.Info("external plugins registered", "count", len(external))
	}
	return e, nil
}

// globalMiddlewares 构造全局中间件链（最外层最先执行）。
//
// Recover 出现两次是有意为之：Timeout 会把内层链条放进新的 goroutine 执行，
// 外层 Recover 只能保护本 goroutine 上的中间件，插件 Handler 的 panic 必须由
// Timeout 之内的那个 Recover 兜住，否则会带崩整个进程。
func (e *Engine) globalMiddlewares() []bot.Middleware {
	return []bot.Middleware{
		middleware.Recover(e.log),
		middleware.Logger(e.log),
		middleware.Metrics(e.met),
		middleware.Timeout(orDuration(e.opts.RuleTimeout, defaultRuleTimeout)),
		middleware.Recover(e.log),
		middleware.Auth(e.isAdmin),
		middleware.RateLimit(e.handleLimit, middleware.ScopeUser),
		middleware.Dedup(e.ruleDedup),
	}
}

// isAdmin 判断事件发送者是否为管理员。
func (e *Engine) isAdmin(ev *bot.Event) bool {
	if ev == nil || ev.Sender == nil {
		return false
	}
	for _, id := range e.cfg.Auth.AdminUsers {
		if id == ev.Sender.ID {
			return true
		}
	}
	return false
}

// Run 启动引擎并阻塞，直到 ctx 结束或适配器发生不可恢复错误。
//
// 返回前一定完成优雅关闭（停止接收、处理完已入队事件、停止适配器与插件）。
func (e *Engine) Run(ctx context.Context) error {
	e.runMu.Lock()
	if e.running {
		e.runMu.Unlock()
		return errors.New("engine: already running")
	}
	e.running = true
	e.runMu.Unlock()
	defer func() {
		e.runMu.Lock()
		e.running = false
		e.runMu.Unlock()
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := e.plugins.Setup(runCtx, e.router.Registrar); err != nil {
		return err
	}
	if err := e.applyBotPluginFilter(); err != nil {
		return err
	}

	if err := e.plugins.Start(runCtx); err != nil {
		return err
	}
	e.log.Info("plugins started", "count", e.plugins.Len())

	var wg sync.WaitGroup
	adapterErr := make(chan error, len(e.opts.Adapters))
	for _, b := range e.opts.Adapters {
		wg.Add(1)
		go func(b AdapterBinding) {
			defer wg.Done()
			if err := b.Adapter.Start(runCtx, e.sink); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case adapterErr <- fmt.Errorf("engine: adapter %s: %w", b.BotID, err):
				default:
				}
				cancel()
			}
		}(b)
	}
	e.log.Info("engine started",
		"adapters", len(e.opts.Adapters),
		"plugins", e.plugins.Len(),
		"bots", len(e.adapters),
	)

	var runErr error
	select {
	case <-runCtx.Done():
	case runErr = <-adapterErr:
		cancel()
	}

	e.shutdown(&wg)
	return runErr
}

// shutdown 按规范顺序完成优雅关闭。
func (e *Engine) shutdown(wg *sync.WaitGroup) {
	timeout := orDuration(e.opts.ShutdownTimeout, defaultShutdownTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	e.log.Info("engine shutting down")

	// 1. 停止接收：适配器停止接收新事件。
	var stopErrs []error
	for _, b := range e.opts.Adapters {
		if err := b.Adapter.Stop(ctx); err != nil {
			stopErrs = append(stopErrs, fmt.Errorf("adapter %s: %w", b.BotID, err))
		}
	}
	if len(stopErrs) > 0 {
		e.log.Warn("adapter stop failed", "error", errors.Join(stopErrs...))
	}

	// 2. 等待已入队事件处理完毕。
	if err := e.bus.Close(ctx); err != nil {
		e.log.Warn("event bus close incomplete", "error", err)
	}
	// 3. 等待适配器 goroutine 退出。
	if !waitGroup(wg, ctx) {
		e.log.Warn("adapter start goroutines did not exit before timeout")
	}
	// 4. 停止插件（逆序）。
	if err := e.plugins.Stop(ctx); err != nil {
		e.log.Warn("plugin stop failed", "error", err)
	}
	if e.ownStorage {
		if c, ok := e.store.(interface{ Close() error }); ok {
			if err := c.Close(); err != nil {
				e.log.Warn("storage close failed", "error", err)
			}
		}
	}
	e.log.Info("engine stopped",
		"published", e.bus.Stats().Published,
		"handled", e.bus.Stats().Handled,
	)
}

// applyBotPluginFilter 依据每个 bot 的插件白名单裁剪规则适用范围。
//
// 只有存在非空白名单时才生效：规则只在「允许它的 bot」上命中，任何 bot 都
// 不允许的规则会被标记为匹配不到任何事件（BotIDs 为空切片）。必须在适配器
// 启动、事件开始处理之前调用。
//
// 被 enabled: false 停用的实例不参与：它们不会收到事件，也不应影响
// 「是否收窄」的判断——否则停用唯一配置了白名单的实例会误伤其他实例。
func (e *Engine) applyBotPluginFilter() error {
	enabled := make([]string, 0, len(e.cfg.Bots))
	restricted := false
	for _, b := range e.cfg.Bots {
		if !b.IsEnabled() {
			continue
		}
		enabled = append(enabled, b.Name)
		if len(b.Plugins) > 0 {
			restricted = true
		}
	}
	if !restricted {
		return nil
	}
	err := e.router.UpdateRules(func(rule *bot.Rule) {
		ids := make([]string, 0, len(enabled))
		for _, b := range e.cfg.Bots {
			if !b.IsEnabled() {
				continue
			}
			if len(b.Plugins) == 0 || contains(b.Plugins, rule.Plugin) {
				ids = append(ids, b.Name)
			}
		}
		sort.Strings(ids)
		rule.BotIDs = ids
	})
	if err != nil {
		return fmt.Errorf("engine: apply per-bot plugin whitelist: %w", err)
	}
	e.log.Info("applied per-bot plugin whitelist", "bots", len(enabled))
	return nil
}

// handle 处理单个事件：整体超时由 EventTimeout 约束。
func (e *Engine) handle(ctx context.Context, ev *bot.Event) {
	timeout := orDuration(e.opts.EventTimeout, defaultEventTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := e.router.Dispatch(ctx, ev); err != nil {
		e.log.Warn("event dispatch failed",
			"event_id", ev.ID,
			"platform", ev.Platform,
			"bot", ev.BotID,
			"error", err,
		)
	}
}

// Router 返回路由器，供测试与扩展使用。
func (e *Engine) Router() *router.Router { return e.router }

// Bus 返回事件总线。
func (e *Engine) Bus() *eventbus.Bus { return e.bus }

// Logger 返回根日志器。
func (e *Engine) Logger() *slog.Logger { return e.log }

// Storage 返回插件存储。
func (e *Engine) Storage() bot.Storage { return e.store }

// Plugins 返回已加载插件的元信息，实现 bot.PluginCatalog。
func (e *Engine) Plugins() []bot.Metadata { return e.plugins.Plugins() }

// Adapter 返回指定 bot 的适配器。
func (e *Engine) Adapter(botID string) (bot.Adapter, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ad, ok := e.adapters[botID]
	return ad, ok
}

// Adapters 返回已加载适配器的绑定信息快照，实现 bot.AdapterCatalog。
//
// 返回值已深拷贝元信息切片，调用方修改不会影响引擎内部状态。
func (e *Engine) Adapters() []bot.AdapterInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]bot.AdapterInfo, 0, len(e.adapterInfos))
	for _, info := range e.adapterInfos {
		info.Metadata.Platforms = append([]string(nil), info.Metadata.Platforms...)
		info.Metadata.Permissions = append([]bot.Permission(nil), info.Metadata.Permissions...)
		info.Metadata.Options = append([]string(nil), info.Metadata.Options...)
		out = append(out, info)
	}
	return out
}

// Emit 把外部适配器投递的事件写入事件总线。
//
// 这是外部适配器上行通道（BotService.EmitEvent）的落点，与 eventSink.Emit
// 同语义：返回值只表示事件是否成功入队，不表示已被处理。
func (e *Engine) Emit(ctx context.Context, ev *bot.Event) error {
	return e.sink.Emit(ctx, ev)
}

// eventSink 是适配器投递事件的入口，负责埋点后写入事件总线。
type eventSink struct {
	eng *Engine
}

// 确保 eventSink 满足 bot.EventSink。
var _ bot.EventSink = (*eventSink)(nil)

// Emit 投递事件；实现必须快速返回，业务处理在事件总线的 worker 中进行。
func (s *eventSink) Emit(ctx context.Context, ev *bot.Event) error {
	if ev == nil {
		return errors.New("engine: nil event")
	}
	s.eng.met.EventPublished(ev.Platform)
	if err := s.eng.bus.Publish(ctx, ev); err != nil {
		reason := "error"
		switch {
		case errors.Is(err, eventbus.ErrQueueFull):
			reason = "queue_full"
		case errors.Is(err, eventbus.ErrClosed):
			reason = "closed"
		}
		s.eng.met.EventDropped(ev.Platform, reason)
		return err
	}
	return nil
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// waitGroup 等待 wg 结束，超时返回 false。
func waitGroup(wg *sync.WaitGroup, ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
