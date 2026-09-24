// Package adaptermgr 按配置装配适配器实例。
//
// 装配来源有两个，对调用方完全等价（同一套 bot.Adapter 接口）：
//
//   - 进程内适配器：pkg/bot 注册表，包含内置 adapters/ 与第三方 Go module；
//   - 外部适配器：adapters 段声明了 grpc_addr 的独立进程（见子包 external）。
//
// 本包是权限裁剪与保留键校验的唯一执行点：未声明 storage/network 的适配器拿不到
// 真实 Storage/HTTPClient，未声明 net_listen 却配置了 listen_addr 的适配器直接
// 装配失败。平台名分支一律不存在于本包与 cmd/。
package adaptermgr

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/RandomLemon/kei/internal/adaptermgr/external"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

// Deps 是装配适配器所需的共享依赖。
type Deps struct {
	// Logger 是根日志器，nil 时使用 slog.Default()。
	Logger *slog.Logger
	// Storage 是可用存储；仅声明了 PermStorage 的适配器拿得到。
	Storage bot.Storage
	// HTTPClient 是带超时的客户端；仅声明了 PermNetwork 的适配器拿得到。
	HTTPClient *http.Client
	// TLS 是连接外部适配器时的客户端 TLS；nil 表示明文。
	TLS *tls.Config
}

// Binding 是一个已完成装配的适配器绑定。
type Binding struct {
	// BotID 是配置中的 bot 名称。
	BotID string
	// Adapter 是适配器实例。
	Adapter bot.Adapter
	// Info 是该绑定的展示信息（元信息与进程内/外部标记）。
	Info bot.AdapterInfo
}

// Bindings 是全部 bot 的装配结果。
type Bindings struct {
	log      *slog.Logger
	list     []Binding
	external map[string][]string // 外部适配器名 -> 绑定的 bot ID
	closers  []io.Closer
}

// List 返回绑定列表，顺序与配置中的 bots 一致。
func (b *Bindings) List() []Binding {
	return append([]Binding(nil), b.list...)
}

// Close 关闭外部适配器通道，可重复调用。
func (b *Bindings) Close() error {
	var errs []error
	for _, c := range b.closers {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	b.closers = nil
	return errors.Join(errs...)
}

// eventEmitter 是核心事件入口的可选扩展（internal/engine 实现）。
//
// 外部适配器通过 BotService.EmitEvent 投递事件，核心侧最终落到该入口；
// 这里用类型断言而不是接口方法，避免把引擎写进 pkg/bot 的 BotAPI。
type eventEmitter interface {
	// Emit 把一个事件写入事件总线。
	Emit(ctx context.Context, ev *bot.Event) error
}

// EmitFunc 返回外部适配器上行事件的处理函数。
//
// 处理函数校验适配器名与事件所属 bot 的归属，再写入核心事件总线；因此一个
// 适配器进程无法伪造其他 bot 的事件。返回值可直接作为 grpcsrv 的 EmitEvent 钩子。
func (b *Bindings) EmitFunc(api bot.BotAPI) (func(context.Context, string, *bot.Event) error, error) {
	emitter, ok := api.(eventEmitter)
	if !ok {
		return nil, errors.New("adaptermgr: 核心未提供事件入口（BotAPI 未实现 Emit）")
	}
	return func(ctx context.Context, adapter string, ev *bot.Event) error {
		botIDs, declared := b.external[adapter]
		if !declared {
			return fmt.Errorf("adaptermgr: %q 不是已配置的外部适配器", adapter)
		}
		if ev == nil {
			return errors.New("adaptermgr: nil event")
		}
		if !containsString(botIDs, ev.BotID) {
			return fmt.Errorf("adaptermgr: 外部适配器 %q 无权投递 bot %q 的事件", adapter, ev.BotID)
		}
		return emitter.Emit(ctx, ev)
	}, nil
}

// Validate 校验适配器注册表与配置的一致性，返回可行动的启动错误。
//
// 只做静态检查，不建立任何连接；外部适配器上报的元信息在 Build 阶段校验。
func Validate(cfg *config.Config) error {
	if err := ValidateRegistry(); err != nil {
		return err
	}
	registered := registeredNames()
	declared := declaredNames(cfg)

	for i := range cfg.Bots {
		bc := &cfg.Bots[i]
		meta, _, ok := bot.LookupAdapter(bc.Adapter)
		if !ok {
			if _, external := cfg.Adapters[bc.Adapter]; external {
				continue
			}
			return fmt.Errorf("config: bots[%d].adapter: 未知适配器 %q（已注册: %s；adapters 段已声明: %s）",
				i, bc.Adapter, joinOrNone(registered), joinOrNone(declared))
		}
		if err := checkReservedOptions(bc.Name, bc.Settings, meta.Name, meta.Permissions); err != nil {
			return err
		}
	}
	return nil
}

// ValidateRegistry 校验进程内适配器注册表。
//
// 报错条件：注册名重复、元信息缺 Platforms、声明了仅插件可用的 PermAll。
func ValidateRegistry() error {
	return validateRegistryOf(bot.RegisteredAdapters())
}

// validateRegistryOf 是注册表校验的纯函数形式，便于单测直接覆盖规则。
func validateRegistryOf(metas []bot.AdapterMetadata) error {
	counts := make(map[string]int)
	for _, m := range metas {
		counts[m.Name]++
		if len(m.Platforms) == 0 {
			return fmt.Errorf("adaptermgr: 适配器 %s 未声明 Platforms", m.Name)
		}
		if m.HasPermission(bot.PermAll) {
			return fmt.Errorf("adaptermgr: 适配器 %s 不得声明 %s 权限（仅插件可用）", m.Name, bot.PermAll)
		}
	}

	var dups []string
	for name, n := range counts {
		if n > 1 {
			dups = append(dups, name)
		}
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		return fmt.Errorf("adaptermgr: 适配器注册名重复: %s", strings.Join(dups, ", "))
	}
	return nil
}

// Build 依据配置装配所有 bot 的适配器。
//
// 外部适配器按名字建立一条通道（Init 一次），再为每个绑定的 bot 建立实例
// （Start 一次）。任一步失败都会回收已建立的外部连接并返回错误。
func Build(ctx context.Context, cfg *config.Config, deps Deps) (*Bindings, error) {
	if cfg == nil {
		return nil, errors.New("adaptermgr: nil config")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}

	b := &Bindings{log: deps.Logger, external: make(map[string][]string)}
	clients := make(map[string]*external.Client)

	// 仅装配失败时回收已建立的外部连接；成功后连接的所有权归 Bindings。
	assembled := false
	defer func() {
		if assembled {
			return
		}
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	for _, bc := range cfg.Bots {
		ac, isExternal := cfg.Adapters[bc.Adapter]
		if !isExternal {
			bind, err := buildInProcess(bc, deps)
			if err != nil {
				return nil, err
			}
			b.list = append(b.list, bind)
			continue
		}

		client, ok := clients[bc.Adapter]
		if !ok {
			var err error
			client, err = external.New(ctx, external.Config{
				Name:        bc.Adapter,
				Addr:        ac.GrpcAddr,
				CoreAddr:    cfg.Grpc.Addr,
				Token:       ac.Token,
				Timeout:     ac.Timeout,
				Permissions: Permissions(ac.Permissions),
				Platform:    ac.Platform,
				Settings:    ac.Settings,
				TLS:         deps.TLS,
			}, external.Deps{Logger: deps.Logger})
			if err != nil {
				return nil, fmt.Errorf("adaptermgr: 外部适配器 %s: %w", bc.Adapter, err)
			}
			clients[bc.Adapter] = client
			b.closers = append(b.closers, client)
			warnUnknownOptions(ac.Settings, client.Metadata().Options, deps.Logger, "adapters."+bc.Adapter, bc.Adapter)
		}

		ad, err := client.Instance(bc.Name, bot.NewConfig(bc.Settings))
		if err != nil {
			return nil, fmt.Errorf("adaptermgr: 外部适配器 %s 的 bot %s: %w", bc.Adapter, bc.Name, err)
		}
		meta := client.Metadata()
		warnUnknownOptions(bc.Settings, meta.Options, deps.Logger, bc.Name, meta.Name)

		b.external[bc.Adapter] = append(b.external[bc.Adapter], bc.Name)
		b.list = append(b.list, Binding{
			BotID:   bc.Name,
			Adapter: ad,
			Info:    bot.AdapterInfo{BotID: bc.Name, Metadata: meta, External: true},
		})
	}

	for _, name := range declaredNames(cfg) {
		if _, ok := clients[name]; !ok {
			b.log.Warn("适配器已在 adapters 段声明但没有 bot 引用", "adapter", name)
		}
	}
	assembled = true
	return b, nil
}

// buildInProcess 依据注册表装配一个进程内适配器实例。
func buildInProcess(bc config.BotConfig, deps Deps) (Binding, error) {
	meta, factory, ok := bot.LookupAdapter(bc.Adapter)
	if !ok {
		// Validate 已经保证可达，这里只做防御。
		return Binding{}, fmt.Errorf("adaptermgr: 未知适配器 %q（bot %s）", bc.Adapter, bc.Name)
	}
	warnUnknownOptions(bc.Settings, meta.Options, deps.Logger, bc.Name, meta.Name)

	ac := bot.AdapterContext{
		BotID:  bc.Name,
		Config: bot.NewConfig(bc.Settings),
		Logger: deps.Logger.With("bot", bc.Name, "adapter", meta.Name),
	}
	if meta.HasPermission(bot.PermStorage) && deps.Storage != nil {
		ac.Storage = deps.Storage
	} else {
		ac.Storage = storage.Denied()
	}
	if meta.HasPermission(bot.PermNetwork) {
		ac.HTTPClient = deps.HTTPClient
	}

	ad, err := factory(ac)
	if err != nil {
		return Binding{}, fmt.Errorf("adaptermgr: 适配器 %s 构造失败（bot %s）: %w", meta.Name, bc.Name, err)
	}
	if ad == nil {
		return Binding{}, fmt.Errorf("adaptermgr: 适配器 %s 的工厂返回 nil（bot %s）", meta.Name, bc.Name)
	}
	name := ad.Name()
	if name == "" {
		return Binding{}, fmt.Errorf("adaptermgr: 适配器 %s 返回空平台名（bot %s）", meta.Name, bc.Name)
	}
	if !containsString(meta.Platforms, name) {
		deps.Logger.Warn("适配器返回的平台名未在元信息中声明",
			"adapter", meta.Name, "bot", bc.Name, "platform", name, "declared", meta.Platforms)
	}
	return Binding{
		BotID:   bc.Name,
		Adapter: ad,
		Info:    bot.AdapterInfo{BotID: bc.Name, Metadata: meta},
	}, nil
}

// checkReservedOptions 校验保留键：配置了 listen_addr 就必须声明 net_listen。
func checkReservedOptions(botID string, settings map[string]any, adapter string, perms []bot.Permission) error {
	if !optionConfigured(settings, bot.OptListenAddr) {
		return nil
	}
	if hasPermission(perms, bot.PermNetListen) {
		return nil
	}
	return fmt.Errorf("config: bot %s 配置了保留键 %s，但适配器 %s 未声明 %s 权限",
		botID, bot.OptListenAddr, adapter, bot.PermNetListen)
}

// optionConfigured 判断配置中是否存在非空的指定键。
func optionConfigured(settings map[string]any, key string) bool {
	v, ok := settings[key]
	if !ok || v == nil {
		return false
	}
	return strings.TrimSpace(fmt.Sprint(v)) != ""
}

// warnUnknownOptions 对适配器不认识的私有配置键告警（不报错）。
//
// 适配器未声明 Options 时无法判断，静默跳过。
func warnUnknownOptions(settings map[string]any, known []string, logger *slog.Logger, botID, adapter string) {
	if len(known) == 0 {
		return
	}
	for key := range settings {
		if !containsString(known, key) {
			logger.Warn("适配器不认识的配置键",
				"bot", botID, "adapter", adapter, "key", key)
		}
	}
}

// Permissions 把配置中的权限名转换为权限集合，忽略空白项。
func Permissions(names []string) []bot.Permission {
	out := make([]bot.Permission, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, bot.Permission(name))
		}
	}
	return out
}

// registeredNames 返回已注册适配器名（排序）。
func registeredNames() []string {
	metas := bot.RegisteredAdapters()
	out := make([]string, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

// declaredNames 返回 adapters 段声明的适配器名（排序）。
func declaredNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Adapters))
	for name := range cfg.Adapters {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// joinOrNone 把名字列表拼成可读文本，空列表返回「无」。
func joinOrNone(names []string) string {
	if len(names) == 0 {
		return "无"
	}
	return strings.Join(names, ", ")
}

// containsString 判断字符串切片是否包含目标。
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// hasPermission 判断权限集合是否包含目标。
func hasPermission(perms []bot.Permission, p bot.Permission) bool {
	for _, x := range perms {
		if x == p {
			return true
		}
	}
	return false
}
