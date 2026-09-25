package bot

import (
	"log/slog"
	"net/http"
	"sync"
)

// OptListenAddr 是适配器的保留配置键：入站监听地址（webhook / 长连接）。
//
// 核心按它与非保留权限核对：配置了该键的 bot，其适配器必须声明 PermNetListen，
// 否则启动失败（见 internal/adaptermgr）。核心不解释它的取值，由适配器自行解析。
const OptListenAddr = "listen_addr"

// AdapterFactory 依据 AdapterContext 构造一个适配器实例。
//
// 注册的是工厂而不是单例：同一平台可以有多个实例（配置中的多个 bot，
// 例如两个飞书应用），因此实例状态不得放在包级变量里。
type AdapterFactory func(ctx AdapterContext) (Adapter, error)

// AdapterContext 是构造适配器实例所需的依赖与配置。
//
// 与插件侧的 PluginContext 对应，但刻意不包含 BotAPI：适配器位于引擎之下，
// 事件只能经 EventSink 上行、消息只能经 Adapter.Send 下行。
type AdapterContext struct {
	// BotID 是实例名（配置中的 bots[].name）。适配器必须把它写入
	// Event.BotID 与 SendRequest.BotID。
	BotID string
	// Config 是该实例的适配器私有配置（bots[] 条目中除 name/adapter/enabled/plugins
	// 外的键），支持 "a.b.c" 多级键读取。
	Config *Config
	// Logger 不为 nil；适配器日志必须使用它，并带上 bot/adapter 字段。
	Logger *slog.Logger
	// Storage 仅在声明 PermStorage 时是可用实现，否则为始终拒绝的实现。
	Storage Storage
	// HTTPClient 仅在声明 PermNetwork 时非 nil；适配器依赖出站请求时应先检查。
	HTTPClient *http.Client
}

// AdapterMetadata 是适配器元信息，与插件侧的 Metadata 对应。
type AdapterMetadata struct {
	// Name 是注册名，全局唯一，只允许 [a-z0-9_-]；配置中的 bots[].adapter 引用它。
	Name string
	// Version 是适配器版本。
	Version string
	// Author 是适配器作者。
	Author string
	// Description 是适配器说明。
	Description string
	// Platforms 是写入 Event.Platform 的平台标识，至少一个。
	Platforms []string
	// Permissions 是适配器申请的权限；PermAll 不适用于适配器。
	Permissions []Permission
	// Options 是适配器接受的配置键（bots[] 条目上的私有键），用于核心对未知
	// 配置键告警；为空表示不校验。
	Options []string
}

// HasPermission 判断适配器是否申请了指定权限。
func (m AdapterMetadata) HasPermission(p Permission) bool {
	for _, x := range m.Permissions {
		if x == p {
			return true
		}
	}
	return false
}

// clone 深拷贝元信息，保证注册表快照与调用方持有的切片互不影响。
func (m AdapterMetadata) clone() AdapterMetadata {
	m.Platforms = append([]string(nil), m.Platforms...)
	m.Permissions = append([]Permission(nil), m.Permissions...)
	m.Options = append([]string(nil), m.Options...)
	return m
}

// AdapterInfo 描述一个已加载的适配器实例，供管理类插件展示。
type AdapterInfo struct {
	// BotID 是配置中的 bot 名称。
	BotID string
	// Metadata 是适配器的注册元信息。
	Metadata AdapterMetadata
	// External 表示该实例由外部 gRPC 适配器进程提供。
	External bool
}

// AdapterCatalog 提供已加载适配器的绑定信息，供管理类插件使用。
type AdapterCatalog interface {
	// Adapters 返回当前适配器绑定的快照。
	Adapters() []AdapterInfo
}

// AdapterRegistrationError 描述一次未能进入注册表的适配器注册。
//
// RegisterAdapter 的签名不返回错误（公开 SDK 必须保持稳定），被拒绝的注册
// 记录在这里，由启动校验（internal/adaptermgr）报出，避免静默失效。
type AdapterRegistrationError struct {
	// Name 是注册时给出的名字；名称为空时该字段为空串。
	Name string
	// Reason 是注册被拒绝的原因。
	Reason string
}

// adapterRegistration 是注册表中的一项。
type adapterRegistration struct {
	meta    AdapterMetadata
	factory AdapterFactory
}

var (
	adapterMu       sync.RWMutex
	adapterRegistry []adapterRegistration
	adapterReject   []AdapterRegistrationError
)

// RegisterAdapter 注册编译期适配器工厂，通常由适配器包在 init() 中调用。
//
// 与 RegisterPlugin 一样只记录，不构造实例。注册名（meta.Name）为空或
// factory 为 nil 时无法成为可用适配器：该次注册被拒绝、不入表，而是记入
// AdapterRegistrationErrors，由启动校验报错终止启动（见 internal/adaptermgr）。
// 同名重复注册不是「后者覆盖前者」：必须在启动校验阶段报错。
func RegisterAdapter(meta AdapterMetadata, factory AdapterFactory) {
	switch {
	case meta.Name == "":
		recordAdapterReject(meta.Name, "注册名为空")
		return
	case factory == nil:
		recordAdapterReject(meta.Name, "工厂为 nil")
		return
	}
	adapterMu.Lock()
	defer adapterMu.Unlock()
	adapterRegistry = append(adapterRegistry, adapterRegistration{meta: meta.clone(), factory: factory})
}

// recordAdapterReject 记录一次被拒绝的注册。
func recordAdapterReject(name, reason string) {
	adapterMu.Lock()
	defer adapterMu.Unlock()
	adapterReject = append(adapterReject, AdapterRegistrationError{Name: name, Reason: reason})
}

// AdapterRegistrationErrors 返回被拒绝的注册快照（按注册顺序）。
//
// 非空表示有适配器包调用了 RegisterAdapter 但没有提供可用的名字或工厂；
// 启动校验会据此报错，而不是让该适配器在运行时表现为「未注册」。
func AdapterRegistrationErrors() []AdapterRegistrationError {
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	return append([]AdapterRegistrationError(nil), adapterReject...)
}

// RegisteredAdapters 返回按注册顺序排列的元信息快照。
func RegisteredAdapters() []AdapterMetadata {
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	out := make([]AdapterMetadata, 0, len(adapterRegistry))
	for _, r := range adapterRegistry {
		out = append(out, r.meta.clone())
	}
	return out
}

// LookupAdapter 按注册名取元信息与工厂，未注册时返回 false。
//
// 同名重复注册时返回首次注册者；是否允许重复由启动校验判定。
func LookupAdapter(name string) (AdapterMetadata, AdapterFactory, bool) {
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	for _, r := range adapterRegistry {
		if r.meta.Name == name {
			return r.meta.clone(), r.factory, true
		}
	}
	return AdapterMetadata{}, nil, false
}
