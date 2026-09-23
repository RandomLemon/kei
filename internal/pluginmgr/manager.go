// Package pluginmgr 管理插件的注册与生命周期。
//
// 管理器负责按顺序执行 Setup/Start、逆序执行 Stop，为每个插件注入
// PluginContext，并提供 panic 隔离与超时保护，单个插件失败不会拖垮进程。
package pluginmgr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

const (
	defaultSetupTimeout = 15 * time.Second
	defaultStartTimeout = 15 * time.Second
	defaultStopTimeout  = 15 * time.Second
)

// Deps 是 Manager 的外部依赖。
type Deps struct {
	// Storage 是插件可用的存储，插件需声明 PermStorage 才能访问。
	Storage bot.Storage
	// HTTPClient 是插件可用的 HTTP 客户端，插件需声明 PermNetwork 才能拿到。
	HTTPClient *http.Client
	// Logger 是根日志器，管理器会为每个插件派生带 plugin 字段的子日志器。
	Logger *slog.Logger
	// Configs 是插件名到独立配置的映射，缺失的插件得到空配置。
	Configs map[string]*bot.Config
	// API 为插件名与元信息返回该插件专属的 BotAPI 门面（权限在此收口）。
	API func(name string, meta bot.Metadata) bot.BotAPI
	// Catalog 返回已加载插件的元信息，供 PluginContext.Catalog 使用。
	Catalog func() []bot.Metadata
	// SetupTimeout/StartTimeout/StopTimeout 为各阶段超时，<=0 时用默认值。
	SetupTimeout time.Duration
	StartTimeout time.Duration
	StopTimeout  time.Duration
}

type state int

const (
	stateNew state = iota
	stateReady
	stateStarted
	stateStopped
)

type entry struct {
	plugin bot.Plugin
	meta   bot.Metadata
	state  state
}

// Manager 是插件生命周期管理器，并发安全。
type Manager struct {
	mu     sync.Mutex
	deps   Deps
	log    *slog.Logger
	order  []*entry
	byName map[string]*entry
}

// 确保 Manager 提供插件目录能力。
var _ bot.PluginCatalog = (*Manager)(nil)

// New 构造插件管理器。
func New(deps Deps) *Manager {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Manager{
		deps:   deps,
		log:    deps.Logger,
		byName: make(map[string]*entry),
	}
}

// Add 登记一个插件；名称为空或重名时返回错误。
func (m *Manager) Add(p bot.Plugin) error {
	if p == nil {
		return errors.New("pluginmgr: nil plugin")
	}
	meta := p.Metadata()
	if meta.Name == "" {
		return errors.New("pluginmgr: plugin metadata name is empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.byName[meta.Name]; ok {
		return fmt.Errorf("pluginmgr: duplicate plugin %q", meta.Name)
	}
	e := &entry{plugin: p, meta: meta}
	m.order = append(m.order, e)
	m.byName[meta.Name] = e
	return nil
}

// Setup 依次调用每个插件的 Setup，并为其注入 PluginContext。
//
// regFor 按插件名返回该插件专属的注册器（由路由器提供）。任一插件 Setup
// 失败或 panic 时立即返回错误，已成功 Setup 的插件保持原状由调用方决定后续。
func (m *Manager) Setup(ctx context.Context, regFor func(plugin string) bot.Registrar) error {
	if regFor == nil {
		return errors.New("pluginmgr: nil registrar factory")
	}
	entries := m.snapshot()

	for _, e := range entries {
		reg := regFor(e.meta.Name)
		if reg == nil {
			return fmt.Errorf("pluginmgr: registrar for %q is nil", e.meta.Name)
		}
		if err := m.run(ctx, e, "setup", m.setupTimeout(), func(ctx context.Context) error {
			return e.plugin.Setup(contextWith(ctx, m.contextFor(e.meta)), reg)
		}); err != nil {
			return err
		}
		e.state = stateReady
	}
	return nil
}

// Start 依次启动插件。启动过程中失败时，会逆序停止已启动的插件。
func (m *Manager) Start(ctx context.Context) error {
	entries := m.snapshot()

	var started []*entry
	for _, e := range entries {
		if e.state != stateReady {
			continue
		}
		pc := m.contextFor(e.meta)
		if err := m.run(ctx, e, "start", m.startTimeout(), func(ctx context.Context) error {
			return e.plugin.Start(contextWith(ctx, pc))
		}); err != nil {
			m.stopEntries(ctx, started)
			return err
		}
		e.state = stateStarted
		started = append(started, e)
	}
	return nil
}

// Stop 逆序停止所有已启动的插件，可重复调用。
func (m *Manager) Stop(ctx context.Context) error {
	entries := m.snapshot()

	// 收集为注册顺序，由 stopEntries 逆序停止。
	toStop := make([]*entry, 0, len(entries))
	for _, e := range entries {
		if e.state == stateStarted {
			toStop = append(toStop, e)
		}
	}
	return errors.Join(m.stopEntries(ctx, toStop)...)
}

func (m *Manager) stopEntries(ctx context.Context, entries []*entry) []error {
	var errs []error
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.state == stateStopped {
			continue
		}
		if err := m.run(ctx, e, "stop", m.stopTimeout(), func(ctx context.Context) error {
			return e.plugin.Stop(contextWith(ctx, m.contextFor(e.meta)))
		}); err != nil {
			errs = append(errs, err)
		}
		e.state = stateStopped
	}
	return errs
}

// run 在带超时与 panic 保护的上下文中执行某个生命周期阶段。
func (m *Manager) run(ctx context.Context, e *entry, phase string, timeout time.Duration, fn func(context.Context) error) (err error) {
	log := m.log.With("plugin", e.meta.Name, "phase", phase)

	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	defer func() {
		if v := recover(); v != nil {
			log.Error("plugin panicked", "panic", v, "stack", string(debug.Stack()))
			err = fmt.Errorf("pluginmgr: plugin %s %s panic: %v", e.meta.Name, phase, v)
		}
	}()

	start := time.Now()
	if err := fn(stageCtx); err != nil {
		log.Error("plugin stage failed", "duration_ms", time.Since(start).Milliseconds(), "error", err)
		return fmt.Errorf("pluginmgr: plugin %s %s: %w", e.meta.Name, phase, err)
	}
	log.Debug("plugin stage done", "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// contextFor 构造插件运行期上下文，按权限裁剪依赖。
func (m *Manager) contextFor(meta bot.Metadata) bot.PluginContext {
	pc := bot.PluginContext{
		Name:   meta.Name,
		Config: m.configFor(meta.Name),
		Logger: m.log.With("plugin", meta.Name),
		Storage: func() bot.Storage {
			if meta.HasPermission(bot.PermStorage) && m.deps.Storage != nil {
				return m.deps.Storage
			}
			return storage.Denied()
		}(),
	}
	if meta.HasPermission(bot.PermNetwork) {
		pc.HTTPClient = m.deps.HTTPClient
	}
	if m.deps.API != nil {
		pc.Bot = m.deps.API(meta.Name, meta)
	}
	if m.deps.Catalog != nil {
		pc.Catalog = catalogFunc(m.deps.Catalog)
	}
	return pc
}

func (m *Manager) configFor(name string) *bot.Config {
	if c, ok := m.deps.Configs[name]; ok && c != nil {
		return c
	}
	return bot.NewConfig(nil)
}

// Plugins 返回按注册顺序排列的插件元信息快照。
func (m *Manager) Plugins() []bot.Metadata {
	entries := m.snapshot()
	out := make([]bot.Metadata, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.meta)
	}
	return out
}

// Metadata 返回指定插件的元信息。
func (m *Manager) Metadata(name string) (bot.Metadata, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byName[name]
	if !ok {
		return bot.Metadata{}, false
	}
	return e.meta, true
}

// Len 返回已登记的插件数量。
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.order)
}

// contextWith 把插件上下文写入 ctx，供插件内部通过 bot.PluginContextFrom 读取。
func contextWith(ctx context.Context, pc bot.PluginContext) context.Context {
	return bot.WithPluginContext(ctx, pc)
}

func (m *Manager) setupTimeout() time.Duration {
	return orDefault(m.deps.SetupTimeout, defaultSetupTimeout)
}
func (m *Manager) startTimeout() time.Duration {
	return orDefault(m.deps.StartTimeout, defaultStartTimeout)
}
func (m *Manager) stopTimeout() time.Duration {
	return orDefault(m.deps.StopTimeout, defaultStopTimeout)
}

func orDefault(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// snapshot 返回条目切片拷贝，元素指针共享但状态字段只在管理器内部串行修改。
func (m *Manager) snapshot() []*entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*entry(nil), m.order...)
}

// catalogFunc 把函数适配为 bot.PluginCatalog。
type catalogFunc func() []bot.Metadata

func (f catalogFunc) Plugins() []bot.Metadata { return f() }
