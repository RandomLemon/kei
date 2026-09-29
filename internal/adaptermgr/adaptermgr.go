// Package adaptermgr 按配置装配进程内适配器实例。
//
// 适配器来源是 pkg/bot 注册表，包含内置 adapters/ 与第三方 Go module；
// 装配对调用方完全等价（同一套 bot.Adapter 接口）。
//
// 本包是权限裁剪与保留键校验的唯一执行点：未声明 storage/network 的适配器拿不到
// 真实 Storage/HTTPClient，未声明 net_listen 却配置了 listen_addr 的适配器直接
// 装配失败。平台名分支一律不存在于本包与 cmd/。
package adaptermgr

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

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
}

// Binding 是一个已完成装配的适配器绑定。
type Binding struct {
	// BotID 是配置中的 bot 名称。
	BotID string
	// Adapter 是适配器实例。
	Adapter bot.Adapter
	// Info 是该绑定的展示信息（元信息）。
	Info bot.AdapterInfo
}

// Bindings 是全部 bot 的装配结果。
type Bindings struct {
	log  *slog.Logger
	list []Binding
}

// List 返回绑定列表，顺序与配置中的 bots 一致。
func (b *Bindings) List() []Binding {
	return append([]Binding(nil), b.list...)
}

// Validate 校验适配器注册表与配置的一致性，返回可行动的启动错误。
//
// 只做静态检查，不建立任何连接。
// 被 enabled: false 禁用的适配器与 bot 实例都不参与校验：它们在 Build 阶段被跳过。
func Validate(cfg *config.Config) error {
	if err := ValidateRegistry(); err != nil {
		return err
	}
	registered := registeredNames()
	declared := declaredNames(cfg)

	for i := range cfg.Bots {
		bc := &cfg.Bots[i]
		if !bc.IsEnabled() || !cfg.AdapterEnabled(bc.Adapter) {
			continue
		}
		meta, _, ok := bot.LookupAdapter(bc.Adapter)
		if !ok {
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
// 报错条件：存在被拒绝的注册（空注册名或 nil 工厂）、注册名重复、
// 元信息缺 Platforms、声明了仅插件可用的 PermAll。
func ValidateRegistry() error {
	if err := validateRegistryOf(bot.RegisteredAdapters()); err != nil {
		return err
	}
	return validateRegistrationRejects(bot.AdapterRegistrationErrors())
}

// validateRegistrationRejects 是「被拒绝的注册」校验的纯函数形式。
func validateRegistrationRejects(rejects []bot.AdapterRegistrationError) error {
	if len(rejects) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(rejects))
	for _, r := range rejects {
		name := r.Name
		if name == "" {
			name = "<空名>"
		}
		msgs = append(msgs, fmt.Sprintf("%s（%s）", name, r.Reason))
	}
	return fmt.Errorf("adaptermgr: 适配器注册被拒绝: %s；RegisterAdapter 要求 Name 非空且 factory 非 nil",
		strings.Join(msgs, ", "))
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

// Build 依据配置装配所有 bot 的进程内适配器。
func Build(cfg *config.Config, deps Deps) (*Bindings, error) {
	if cfg == nil {
		return nil, errors.New("adaptermgr: nil config")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}

	b := &Bindings{log: deps.Logger}

	for _, bc := range cfg.Bots {
		if !bc.IsEnabled() {
			// 实例被显式停用：不建通道、不装配，也不记未知键告警。
			deps.Logger.Info("bot 已禁用，跳过", "bot", bc.Name, "adapter", bc.Adapter)
			continue
		}
		if !cfg.AdapterEnabled(bc.Adapter) {
			// 适配器被显式禁用：不装配它的任何 bot 实例。
			deps.Logger.Warn("适配器已禁用，跳过 bot", "adapter", bc.Adapter, "bot", bc.Name)
			continue
		}

		bind, err := buildInProcess(bc, deps)
		if err != nil {
			return nil, err
		}
		b.list = append(b.list, bind)
	}

	for _, name := range declaredNames(cfg) {
		ac := cfg.Adapters[name]
		if !ac.IsEnabled() {
			b.log.Info("适配器已禁用", "adapter", name)
			continue
		}
		if _, _, ok := bot.LookupAdapter(name); !ok {
			b.log.Warn("适配器已在 adapters 段声明但未注册（是否漏了空导入？）", "adapter", name)
		}
	}
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
