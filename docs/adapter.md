# kei 平台适配器

覆盖 `pkg/bot` 中的适配器契约（`Adapter`/`EventSink`/`SendRequest`/`Target`/`SendResult`/`Capabilities`/`Degrade`）、注册表与工厂（`AdapterFactory`/`AdapterContext`/`AdapterMetadata`/`RegisterAdapter`/`RegisteredAdapters`/`AdapterRegistrationErrors`/`LookupAdapter`）、元信息与权限裁剪、能力降级规则、第三方适配器接入方式，以及 `internal/adaptermgr` 的装配行为和 `adapters/` 下三个内置适配器的现状。
不覆盖：`Event`/`Message`/`Segment`/`Target` 等消息段类型的逐字段定义（见 [domain-model.md](domain-model.md)）、发送侧限流/重试与 Engine 生命周期（见 [engine.md](engine.md)）、外部适配器的 gRPC 协议与重连语义（见 [grpc.md](grpc.md)）、`bots`/`adapters` 配置键全表（见 [configuration.md](configuration.md)）。
插件侧的 `Permission`/`Metadata` 与权限模型见 [plugin.md](plugin.md)；面向用户的适配器编写教程与示例配置见 [../README.md](../README.md) 的「写一个适配器」「外部适配器（gRPC）」小节。

---

## 6. Adapter 平台适配层

适配器与插件同等对待：都可以由第三方开发（进程内注册，或作为独立进程经 gRPC 接入），核心只通过注册表 + 工厂按配置装配，不为任何平台写死代码分支。装配逻辑集中在 `internal/adaptermgr`（`internal/adaptermgr/external` 负责外部进程代理），`internal/adaptermgr` 与 `cmd/` 中不存在任何平台名分支（`switch adapter`）。

### 6.1 适配器接口

在 `pkg/bot/adapter.go` 中定义。一个 Adapter 实例对应配置文件中的一个 bot（例如一个飞书应用或一个 OneBot 实现）；实现方负责签名校验、协议解析、事件转换与消息发送。

```go
type Adapter interface {
	// Name 返回平台名，例如 "feishu"、"onebot"、"mock"。
	Name() string
	// Start 启动事件接收，阻塞直到 ctx 结束或发生不可恢复错误。
	// 启动前必须完成监听/连接，保证返回后即可接收事件。
	Start(ctx context.Context, sink EventSink) error
	// Stop 停止接收事件；必须幂等，并等待已接收事件投递完毕。
	Stop(ctx context.Context) error
	// Send 把统一消息发送到目标会话。
	Send(ctx context.Context, req *SendRequest) (*SendResult, error)
	// Capabilities 声明平台能力，用于消息段降级。
	Capabilities() Capabilities
}

// EventSink 是适配器向上投递事件的唯一出口。
//
// 实现方（事件总线）必须快速返回，业务处理在后台进行，
// 以免阻塞平台回调线程。
type EventSink interface {
	// Emit 投递一个事件。返回值仅表示是否成功入队，不表示已被处理。
	Emit(ctx context.Context, ev *Event) error
}

// SendRequest 是一次发送请求。
type SendRequest struct {
	// BotID 指定使用哪个机器人实例发送。
	BotID string
	// Target 指定接收方。
	Target Target
	// Message 是待发送的统一消息。
	Message *Message
	// ReplyTo 非空时表示引用回复该消息 ID。
	ReplyTo string
}

// Target 描述消息接收方。
type Target struct {
	// Platform 是平台名。
	Platform string
	// BotID 是机器人标识（配置中的 bot 名称）。同一平台部署多个机器人时必填；
	// 为空且该平台只有一个机器人实例时由引擎自动选择。
	BotID string
	// ChannelID 是群/频道 ID，私聊时可为空。
	ChannelID string
	// UserID 是用户 ID，群聊时可为空。
	UserID string
	// Kind 是会话类型。
	Kind MessageKind
}

// SendResult 是一次发送的结果。
type SendResult struct {
	// MessageID 是平台返回的消息 ID。
	MessageID string
	// Raw 是平台原始响应，仅用于调试。
	Raw any
}

// Capabilities 声明平台支持的消息能力，引擎据此降级不支持的消息段。
type Capabilities struct {
	// Text 表示支持纯文本。
	Text bool
	// Markdown 表示支持 Markdown 富文本。
	Markdown bool
	// Image 表示支持图片。
	Image bool
	// At 表示支持 @ 用户。
	At bool
	// Card 表示支持结构化卡片。
	Card bool
	// File 表示支持文件收发。
	File bool
	// Reply 表示支持引用回复。
	Reply bool
	// Private 表示支持私聊。
	Private bool
	// Group 表示支持群聊。
	Group bool
}

// Supports 判断平台是否支持某类消息段。
func (c Capabilities) Supports(t SegmentType) bool {
	switch t {
	case SegText:
		return c.Text
	case SegMarkdown:
		return c.Markdown
	case SegImage:
		return c.Image
	case SegAt:
		return c.At
	case SegCard:
		return c.Card
	case SegReply:
		return c.Reply
	case SegFile:
		return c.File
	case SegFace:
		return c.Text
	default:
		return false
	}
}
```

要点：

- `Supports` 只覆盖与消息段对应的一组能力位；`Private`/`Group` 描述会话类型，不参与 `Supports`，也不参与 `Degrade`。
- `SegFace` 由 `Text` 支撑：平台没有独立的表情段能力时，表情随文本一起发送。因此 `Supports(SegFace) == false` 只可能来自 `Text: false`，声明了纯文本的适配器无法通过关闭能力位来阻止表情段走文本通路。
- `Supports` 只识别 `SegText`/`SegMarkdown`/`SegImage`/`SegAt`/`SegCard`/`SegReply`/`SegFile`/`SegFace` 八种段类型；其余（含核心尚未定义的新类型）一律返回 false，即按「不支持」处理。
- `TargetFromEvent(ev *Event) Target`（同文件）由事件推导回复目标：`Platform`/`BotID` 取自事件，`Kind` 优先取 `ev.Message.Kind`、缺失时回退 `ev.Channel.Kind`，`ChannelID`/`UserID` 分别取自 `ev.Channel`、`ev.Sender`；`ev == nil` 时返回零值。
- `Start` 收到的 `EventSink` 由引擎实现（`internal/engine` 的 `eventSink`），其 `Emit` 埋点后写入事件总线，返回值只表示是否成功入队。

### 6.2 注册表与工厂

适配器以「元信息 + 工厂」注册到 `pkg/bot` 的全局注册表（`pkg/bot/adapter_registry.go`），机制与插件注册 `RegisterPlugin` 同构：注册只记录，是否实例化由配置决定。

```go
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
	// Config 是该实例的适配器私有配置（bots[] 条目中除 name/adapter/plugins
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
func AdapterRegistrationErrors() []AdapterRegistrationError

// RegisteredAdapters 返回按注册顺序排列的元信息快照。
func RegisteredAdapters() []AdapterMetadata

// LookupAdapter 按注册名取元信息与工厂，未注册时返回 false。
//
// 同名重复注册时返回首次注册者；是否允许重复由启动校验判定。
func LookupAdapter(name string) (AdapterMetadata, AdapterFactory, bool)
```

要点：

- 注册表是包级切片 + `sync.RWMutex`；`RegisterAdapter` 存入元信息的深拷贝（`clone` 复制 `Platforms`/`Permissions`/`Options`），`RegisteredAdapters`/`LookupAdapter` 也返回深拷贝，调用方修改不会影响注册表。
- `RegisterAdapter` 的签名不返回错误（公开 SDK 稳定性），因此被拒绝的注册不会当场失败，也不会静默失效：空注册名与 nil 工厂分别以 `"注册名为空"`、`"工厂为 nil"` 记入独立的拒绝列表（`adapterReject`，与 `adapterRegistry` 分开），`AdapterRegistrationErrors` 按注册顺序返回快照；启动校验 `internal/adaptermgr.ValidateRegistry` 在列表非空时报错 `adaptermgr: 适配器注册被拒绝: <名字或<空名>>（<原因>）；RegisterAdapter 要求 Name 非空且 factory 非 nil`（名字为空时显示 `<空名>`，多条拒绝以 `, ` 连接）。
- `RegisteredAdapters` 按注册顺序返回；`LookupAdapter` 线性查找，重复注册时返回**首次**注册者的元信息与工厂，但重复本身会被启动校验判为错误。
- 保留键常量是 `bot.OptListenAddr`（值为 `"listen_addr"`），核心只把它与非保留权限 `PermNetListen` 做核对，不解释其取值。
- 适配器不持有 `BotAPI`（`AdapterContext` 中刻意没有该字段）：事件只能经 `EventSink` 上行、消息只能经 `Adapter.Send` 下行，避免平台层反向进入引擎造成递归与死锁。外部适配器代理的 `Start(ctx, sink)` 仅用 `sink` 满足接口（要求非 nil），事件实际经 `BotService.EmitEvent` 上行，由核心侧完成授权与入队。
- 注册表还提供面向审计的只读视图：`AdapterInfo{BotID, Metadata, External}` 与 `AdapterCatalog interface { Adapters() []AdapterInfo }`（`AdapterInfo.External` 表示该实例由外部 gRPC 适配器进程提供）。管理插件 `/adapters` 同时展示注册表（`RegisteredAdapters`）与绑定信息（`AdapterCatalog`）。
- 元信息的字符集约束 `[a-z0-9_-]` 不在注册表中校验，而由配置解析校验（`internal/config` 对 `bots[].adapter` 与 `adapters.<name>` 校验，报错文案 `只允许 [a-z0-9_-]`）；注册表校验只管「被拒绝的注册」、重名、`Platforms` 非空与 `PermAll`。

### 6.3 元信息与权限

`Permission` 类型与常量全集定义在 `pkg/bot/plugin.go`，插件与适配器共用同一类型：

```go
type Permission string

// 支持的权限；插件与适配器共用同一类型，适配器侧的裁剪规则见 AdapterMetadata。
const (
	// PermSendMessage 允许插件主动发送消息（而不仅是回复当前会话）。
	PermSendMessage Permission = "send_message"
	// PermReadUser 允许读取用户信息。
	PermReadUser Permission = "read_user"
	// PermNetwork 允许发起外部网络请求。
	PermNetwork Permission = "network"
	// PermStorage 允许读写 Storage。
	PermStorage Permission = "storage"
	// PermNetListen 允许启动入站监听（webhook / 长连接），适配器使用。
	PermNetListen Permission = "net_listen"
	// PermReceiveEvent 允许向核心投递事件（外部适配器的 BotService.EmitEvent）。
	PermReceiveEvent Permission = "receive_event"
	// PermAdmin 允许执行管理员命令（配合 Auth 中间件）。
	PermAdmin Permission = "admin"
	// PermAll 表示全部权限，仅内置插件可用；适配器不得声明。
	PermAll Permission = "*"
)
```

两个 `HasPermission` 语义不同，适配器一律使用前者：

| 方法 | 行为 |
| --- | --- |
| `AdapterMetadata.HasPermission(p)` | 精确匹配 `Permissions` 切片；**不**把 `PermAll` 当作通配 |
| `Metadata.HasPermission(p)`（插件侧） | `x == p || x == PermAll`，即 `PermAll` 视为拥有全部权限 |

核心按 `AdapterMetadata.Permissions` 在装配期裁剪依赖（`internal/adaptermgr.buildInProcess`），并把权限透出给 `/adapters` 审计：

| 权限 | 含义 | 未声明时核心的行为 |
| --- | --- | --- |
| `send_message` | 出站发送（适配器自己的 `Send`） | 仅审计；权限模型与插件共用。进程内适配器路径上没有任何 `PermSendMessage` 检查，`Send` 始终可用（唯一例外是 gRPC `BotService.SendMessage`，它对插件与适配器令牌都要求该权限，见 [grpc.md](grpc.md)） |
| `network` | 出站网络请求 | `AdapterContext.HTTPClient` 为 nil |
| `storage` | 读写去重/状态缓存 | 注入拒绝式 `Storage`（`internal/storage.Denied()`，全部方法返回 `ErrPermissionDenied`） |
| `net_listen` | 启动入站监听（webhook / 长连接） | 配置里出现保留键 `listen_addr` 时启动失败 |
| `receive_event` | 向核心投递事件（外部适配器经 `BotService.EmitEvent`） | 外部适配器调用被拒绝 |
| `*` | 不适用于适配器（声明即启动失败） | — |

要求：

1. 权限必须如实声明；适配器不得声明 `*`（注册表校验直接报错），`*` 只对内置插件有意义。
2. 声明与授予取交集：外部适配器实际可用权限 = `AdapterMetadata.Permissions` ∩ `adapters.<name>.permissions`。
3. 未声明 `network` 却发起出站请求、未声明 `storage` 却写缓存，属于适配器缺陷；核心只保证不为其提供依赖，不做事后拦截。

各条的落地位置：

- 注册表校验：`internal/adaptermgr.ValidateRegistry` = `validateRegistryOf` + `validateRegistrationRejects`。报错条件为「存在被拒绝的注册（空注册名或 nil 工厂）」「注册名重复」「元信息缺 `Platforms`」「声明了 `PermAll`」，文案分别是 `适配器注册被拒绝: <名字或<空名>>（<原因>）…`、`适配器注册名重复: …`、`适配器 %s 未声明 Platforms`、`适配器 %s 不得声明 * 权限（仅插件可用）`。外部适配器在 `Init` 应答中上报 `PermAll` 同样被拒（`internal/adaptermgr/external`）。
- 依赖裁剪：`PermStorage` 且 `Deps.Storage != nil` 时注入真实存储，否则一律注入 `storage.Denied()`（未授予、或授予了但核心没有可用存储，结果相同）；只有声明了 `PermNetwork` 才注入 `Deps.HTTPClient`，未声明时该字段保持 nil。`PermSendMessage` 不参与任何裁剪。
- 保留键校验：`checkReservedOptions(botID, settings, adapter, perms)` 在 `adaptermgr.Validate`（进程内适配器）与 `external.Client.Instance`（外部适配器实例）两处执行；`listen_addr` 值为 nil 或去空白后为空时视为未配置。报错文案：`配置了保留键 listen_addr，但适配器 %s 未声明 net_listen 权限`。
- 权限交集：`intersectPermissions(declared, granted)` 保留 `declared` 的顺序；`granted` 为空时返回 nil。生效集合写入 `Client.Metadata().Permissions`，因此 `/adapters` 展示的是交集后的结果，外部实例的保留键校验也基于它。`adapters.<name>.permissions` 缺省为 `[receive_event]`（`internal/config`），只对声明了 `grpc_addr` 的外部适配器生效。
- `net_listen` 的校验只覆盖保留键 `listen_addr`：适配器是否真的监听、监听在哪个地址，核心不检查也不拦截；换用别的配置键做入站监听则完全不受该权限约束。
- 适配器元信息的 `Options` 只影响未知键告警（见 6.5），不影响权限。

### 6.4 能力与降级

1. 引擎在调用 `Adapter.Send` 之前统一执行 `bot.Degrade(msg, caps)`，适配器只需如实声明 `Capabilities`。执行点是 `internal/engine.SendRequest`：解析出适配器后调用 `bot.Degrade(req.Message, ad.Capabilities())`，再组装 `bot.SendRequest`。内置 feishu/onebot 适配器在自己的 `Send` 里也调用 `bot.Degrade`（对已降级的消息是幂等操作），OneBot 的 `buildArray` 另对 Markdown 段做了兜底映射。
2. 降级规则（`pkg/bot/degrade.go` 逐条对应）：

```go
// Degrade 按平台能力降级消息段，返回可直接发送的消息。
//
// 降级规则：Markdown 转纯文本；图片/文件转带链接的文本；@ 转 "@名字"；
// 卡片转为 JSON 文本；引用段在不支持时丢弃。所有段都被支持时原样返回，
// 不产生拷贝。入参消息不会被修改。
func Degrade(msg *Message, caps Capabilities) *Message
```

| 段类型 | 触发条件 | 降级结果（源码表达式） |
| --- | --- | --- |
| `SegMarkdown` | `!caps.Markdown` | 文本段，内容 `str(seg.Data[KeyText])`（原样保留 Markdown 文本） |
| `SegImage` | `!caps.Image` | `"[image] " + firstNonEmpty(str(KeyURL), str(KeyFile))` |
| `SegFile` | `!caps.File` | `"[file] " + joinNonEmpty(" ", str(KeyFileName), firstNonEmpty(str(KeyURL), str(KeyFile)))` |
| `SegAt` | `!caps.At` | `"@" + firstNonEmpty(str(KeyUserName), str(KeyUserID))` |
| `SegFace` | `!caps.Text`（`Supports(SegFace) == c.Text`） | `"[face] " + str(KeyFaceID)` |
| `SegCard` | `!caps.Card` | `"[card] " + jsonText(seg.Data[KeyCard])` |
| `SegReply` | `!caps.Reply` | 直接丢弃该段，不产生新段（消息本身仍发送） |
| 其它类型 | `Supports` 返回 false（未知类型恒 false） | `"[" + string(seg.Type) + "]"` |
| 任意受支持的段 | `caps.Supports(seg.Type)` 为 true | 原样追加，不做转换 |

   辅助函数的取值语义：

   - `str(v)`：nil → `""`；string 原样；其它类型 `fmt.Sprint(v)`。
   - `jsonText(v)`：nil → `""`；string 原样返回；否则 `json.Marshal`，序列化失败时回退 `fmt.Sprint(v)`。
   - `appendText(s)` 只在 `s == ""` 时跳过追加。除 `SegReply` 外的每条规则都带固定前缀或符号，因此只有 `SegMarkdown` 的 `KeyText` 为空时该段才会整体消失；数据键全缺的图片/文件/@/表情/卡片段仍会产生 `"[image] "`、`"[file] "`、`"@"`、`"[face] "`、`"[card] "` 这类文本段。
   - 全部段都受支持时函数直接 `return msg`，不产生拷贝，且入参消息不会被修改（只有存在不受支持的段时才新建 `Message` 与段切片，`ID`/`Kind` 原样保留）。
3. 降级必须记录日志与指标；整条消息因降级变为空时发送失败并返回错误，不得静默丢消息。实现现状：

   - 空消息、或降级后 `len(degraded.Segments) == 0`，`internal/engine.SendRequest` 直接返回错误 `engine: 消息在按平台能力降级后没有可发送的段（bot %s, platform %s）`，不会调用 `Adapter.Send`。
   - 降级过程本身不产生日志，也没有专门的降级指标；发送结果由 `kei_messages_sent_total{platform,result}` 与 `kei_message_send_seconds{platform}` 记录（`internal/metrics`），重试时只记 `Debug` 日志 `send failed, retrying`。上述「降级后为空」的错误在引擎内既不记日志也不记指标，只在向上返回后由调用方（插件中间件的 error 日志、gRPC `SendResponse.Error` 等）体现。
4. 外部适配器的能力以 `AdapterStartResponse.capabilities` 为唯一权威来源；缺失或全零按「仅文本」处理并告警。实现于 `internal/adaptermgr/external/adapter.go`：`Start` 应答到达前 `Capabilities()` 返回零值（引擎只在发送前查询，此时实例必然已启动）；`capsFromProto` 转换后若结果等于零值，则改写为 `bot.Capabilities{Text: true}` 并记 warn 日志 `外部适配器未声明实例能力，按仅文本处理`。进程内适配器没有该兜底：能力完全由适配器的 `Capabilities()` 决定。

### 6.5 第三方适配器示例

独立 module 内的注册与实现（核心仓库零改动）：

```go
package myim

import (
	"github.com/RandomLemon/kei/pkg/bot"
)

// init 注册工厂：主程序只需空导入本包（或本 module）。
func init() {
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        "myim",
		Version:     "v0.1.0",
		Author:      "third-party",
		Description: "MyIM 平台适配器",
		Platforms:   []string{"myim"},
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermNetListen},
	}, New)
}

// New 由核心按配置调用，每个 bot 实例调用一次。
func New(ac bot.AdapterContext) (bot.Adapter, error) {
	return &Adapter{
		botID:      ac.BotID,
		apiBase:    ac.Config.String("api_base", ""),
		listenAddr: ac.Config.String("listen_addr", "127.0.0.1:0"),
		http:       ac.HTTPClient, // 已按 PermNetwork 裁剪，未声明时为 nil
		log:        ac.Logger.With("bot", ac.BotID, "adapter", "myim"),
	}, nil
}
```

主程序侧只有空导入与配置：

```go
import _ "github.com/example/kei-adapter-myim"
```

```yaml
bots:
  - name: myim-main
    adapter: myim            # 注册名，与 AdapterMetadata.Name 一致
    api_base: https://im.example.com
    listen_addr: 127.0.0.1:19081
```

约定与校验：

- `AdapterContext` 的字段就是全部可用依赖：`BotID`（实例名，必须写进 `Event.BotID` 与 `SendRequest.BotID`）、`Config`、`Logger`、`Storage`、`HTTPClient`。`Config` 是 `bot.NewConfig(bots[] 条目中除 name/adapter/enabled/plugins 外的键)`，`Get`/`String`/`Bool`/`Int`/`Duration`/`Strings` 都支持 `"a.b.c"` 多级键，键缺失或类型不符时返回默认值。（`pkg/bot` 中 `AdapterContext.Config` 的注释仍写作「除 name/adapter/plugins 外」，`enabled` 是后加的实例级开关。）
- 实例级开关：`bots[]` 条目上的 `enabled: false` 跳过该实例（见 6.7），此时该实例的 `Settings` 不参与任何校验，也不会产生未知键告警。
- 声明了 `PermNetListen` 的适配器才允许在实例配置里出现保留键 `listen_addr`（见 6.3）；`Options` 非空时应把 `listen_addr` 一并列入，否则会被当作未知键告警。
- 未知键告警：`internal/adaptermgr.warnUnknownOptions` 对「不在 `AdapterMetadata.Options` 中的私有键」记 warn 日志 `适配器不认识的配置键`，字段为 `bot`/`adapter`/`key`。`Options` 为空表示不校验、静默跳过（此时连 `listen_addr` 也不会告警）；只告警不报错，以支持第三方适配器独立演进。外部适配器的键集来自 `AdapterInitResponse.info.options`（见 [grpc.md](grpc.md)）。
- 进程内适配器与外部适配器在配置上等价：`bots[].adapter` 引用注册名即可，无需 `adapters:` 段声明；`adapters.<name>` 只用于显式启用/禁用或（声明了 `grpc_addr` 时）接入外部进程（见 [configuration.md](configuration.md)）。
- 独立的 module/包布局、`go.mod`、空导入与配置写法见 [../README.md](../README.md) 的「写一个适配器」小节；其他语言的外部进程形态见同文件的「外部适配器（gRPC）」小节。

### 6.6 实现要求

1. 内置适配器放 `adapters/<platform>`；第三方适配器放独立包或独立 module，只依赖 `pkg/bot`，不得依赖 `internal/`，也不得要求核心侧改动。**已实现**：`adapters/mock`、`adapters/onebot`、`adapters/feishu` 的非测试源码只 import 标准库与 `github.com/RandomLemon/kei/pkg/bot`（测试另用 `pkg/message` 构造消息，同属公开 SDK）。
2. 适配器必须在自己的 `init()` 中调用 `RegisterAdapter`；主程序通过空导入启用，是否实例化由配置中的 `bots[].adapter` 决定。**已实现**：三个内置适配器的 `register.go` 都在 `init()` 注册；`cmd/bot/main.go` 空导入 feishu/mock/onebot。注册名或工厂不合法的注册不会静默失效：`RegisterAdapter` 拒绝该次注册并记入 `AdapterRegistrationErrors`，启动校验（`adaptermgr.ValidateRegistry`）据此报错，而不是让它在运行时表现为「未注册」。
3. Adapter 负责签名校验、协议解析、事件转换为统一 `Event`、把统一 `Message` 转为平台消息并发送。**已实现**：飞书的签名/解密/token 校验与开放平台调用、OneBot 的 `Authorization` 校验与 HTTP 上报解析均在各适配器包内。
4. 必须声明 `Capabilities` 与 `Permissions`，声明必须与实现一致；错报能力会导致发送失败或内容丢失。**已实现**：内置适配器的能力位与权限见 6.7，并与发送路径逐一对齐（例如 OneBot 不声明 `Markdown`/`Card`）。
5. 工厂必须支持同平台多实例：状态只能挂在实例上，`New` 每次返回互相隔离的实例。**已实现**：三个内置适配器的可变状态都在 `Adapter` 结构体上；`adaptermgr` 对每个 bot 调一次工厂，外部适配器代理的实例状态也按 `bot_id` 隔离。
6. `Event.ID` 必须稳定且平台内唯一（用于去重），`Event.Platform` 必须属于 `AdapterMetadata.Platforms`，`Event.BotID` 必须等于 `AdapterContext.BotID`。**已实现（含以下边界）**：核心只在装配期用 warn 日志提示不一致——若 `adapter.Name()` 非空但不在 `meta.Platforms` 中，记 `适配器返回的平台名未在元信息中声明`，但不再报错；`adapter.Name()` 为空则装配失败。事件被投递后，去重由事件总线按 `Event.ID` + TTL 完成（`internal/eventbus` 与 `internal/dedup`），未知段类型等字段不做校验。外部适配器路径另有一层归属校验：`Bindings.EmitFunc` 拒绝未配置的适配器名与不属于该适配器的 `Event.BotID`。
7. `Start` 返回前必须完成监听/连接；`Stop` 必须幂等并等待已接收事件投递完毕；适配器内部 goroutine 必须有退出机制，不得泄漏。**已实现**：OneBot `Start` 先 `net.Listen` 再进入 `Serve`，飞书 `Start` 同样先监听、退出前 `Shutdown` + 等 worker（`wg.Wait`）、并给队列剩余事件 5s 排空预算；OneBot 在退出时显式收尾 hijacked 的反向 WebSocket 连接。
8. 平台回调必须快速 ACK：先 ACK 平台，再异步投递到核心；`EventSink.Emit` 返回只表示已入队。入队失败必须记录日志与指标，不得静默丢弃。**已实现（日志层面）**：飞书校验通过后先回 `200 {"code":0}` 再入队（队列容量 1024、2 个 worker，满时丢弃并记 warn `飞书事件队列已满，丢弃事件`）；OneBot 上报先回 `204` 再 `Emit`，失败记 warn `onebot: 投递事件失败`。**指标缺口**：上述丢弃与投递失败都没有对应指标，只有日志。
9. 配置与密钥只能经 `AdapterContext.Config` 读取，不得直接读环境变量或文件。**已实现**：`adapters/` 下没有 `os.Getenv`/`os.ReadFile`/`os.Open` 调用；环境变量覆盖由配置加载层统一完成。
10. 重复投递同一事件必须安全（核心按 `Event.ID` 去重），但适配器不得依赖去重来掩盖自身的重复投递。**已实现**：事件总线在 `Publish` 时用 TTL + 容量上限的去重集合丢弃重复 ID；内置适配器生成稳定 ID（飞书优先 `header.event_id`、缺失时用平台名 + bot 名 + 纳秒时间 + 自增序号兜底；OneBot 消息事件优先 `message_id`，其余类型用 `self_id`/`post_type`/时间/子类型确定性拼接）。
11. Mock Adapter 必须实现，用于本地测试和事件回放。**已实现**：`adapters/mock`（见 6.7）。
12. 适配器的外部进程形态（gRPC）见第 12 章（[grpc.md](grpc.md)），行为要求与本章完全一致。协议、`Init`/`Start`/`Stop`/`Shutdown`/`Send` 语义、能力与平台名协商、崩溃隔离与重连见该文档；核心侧代理实现是 `internal/adaptermgr/external`，重连与停用指标（`kei_adapter_reconnects_total{adapter,result}`、`kei_adapter_disabled_total{adapter}`）经 `adaptermgr.Deps.Recorder` 注入。

### 6.7 内置适配器现状

三个内置适配器都在 `cmd/bot/main.go` 中空导入。`wecom`（企业微信）**未实现**：仓库中没有 `adapters/wecom` 目录，`cmd/bot/main_test.go` 把 `adapter: wecom` 当作「未注册适配器」的失败用例，[../README.md](../README.md) 的「已知限制」也记录了这一点（企业微信可参考飞书适配器的结构接入）。

`bots[]` 条目上的 `enabled: false` 是实例级开关（缺省 true，可用 `KEI_BOTS_<NAME>_ENABLED` 覆盖）：`adaptermgr.Validate` 与 `Build` 都跳过该实例，跳过时不校验保留键、不装配、不记未知键告警，`Build` 记一条 `bot 已禁用，跳过` info 日志。它与 `adapters.<name>.enabled` 的区别是作用范围只到单个实例：同一适配器的其他 bot 实例不受影响，内置适配器（`adapters/feishu` 等）无需任何改动即支持。

#### mock

- 注册名/平台：`mock`（`adapters/mock/register.go`）；版本 `v0.1.0`，作者 `core`。
- 权限：`[net_listen]`；配置键（`Options`）：`platform`、`listen_addr`。
- 接入方式：不连接任何真实平台。事件经 `Adapter.Inject`/`InjectText` 手动注入，或经 HTTP 控制面 `POST /inject` 注入；`GET /sent` 查看发送记录，`DELETE /sent` 清空记录，`GET /healthz` 健康检查。控制面只在实例配置了非空 `listen_addr` 时启动，监听地址可经 `Addr()` 查询。
- 能力：默认全能力（`Text`/`Markdown`/`Image`/`At`/`Card`/`File`/`Reply`/`Private`/`Group` 全为 true），便于验证降级路径之外的行为；`Options.Capabilities` 非零值时以它为准（可构造「仅文本」等边界）。
- 行为：`Send` 不产生任何网络调用，返回 `mock-<自增序号>` 消息 ID 并把 `SendRecord` 记入内存；`Start` 要求非 nil `EventSink`，重复调用返回错误 `mock: already started`，未启动时 `Inject` 返回 `mock: adapter is not running`；`Inject` 会为缺失字段补默认值（平台、bot、时间、类型、ID、Sender、Message、群聊 Channel）。不发起出站请求，工厂不要求 `network` 权限，`HTTPClient` 为 nil 也能构造成功。

#### onebot

- 注册名/平台：`onebot`（`adapters/onebot/register.go` 的 `platformName`）；版本 `v0.2.0`，作者 `core`。
- 权限：`[network, net_listen]`；配置键（`Options`）：`api_url`、`listen_addr`、`path`、`ws_path`、`ping_interval`、`secret`、`access_token`、`self_id`。
- 接入方式：同一个 `listen_addr` 上同时提供两种入站通道，可同时启用。HTTP 上报走 `path`（默认 `/onebot/event`，必须以 `/` 开头），校验 `Authorization: Bearer <secret>` 或 `?access_token=<secret>`（`secret` 为空时不校验），解析成功后立即回 `204` 再投递。反向 WebSocket 走 `ws_path`（默认 `/onebot/ws`，同样必须以 `/` 开头且不得与 `path` 相同），由 OneBot 实现主动连入；`X-Client-Role` 未知或缺失时按 `universal` 处理，`self_id` 与请求头 `X-Self-ID` 都非空且不一致时拒绝连接（403；任一为空则放行），心跳间隔默认 30s，负值表示不发心跳。两个路径之外的请求返回 404。
- 发送：优先经可承载 API 的反向 WebSocket 连接下发（按 `echo` 匹配响应），没有可用连接时回落到 `api_url` 的 HTTP API；`api_url` 为空且无连接时返回错误。目标类型为空时按 `ChannelID`/`UserID` 推断群聊或私聊，群聊用 `send_group_msg`、私聊用 `send_private_msg`。
- 能力：`Text`/`Image`/`At`/`File`/`Reply`/`Private`/`Group` 为 true；`Markdown`、`Card` 为 false；表情段由 `Text` 支撑，因此 Markdown/卡片会在发送前被 `bot.Degrade` 转成文本。
- 工厂：`newFromContext` 在 `ac.HTTPClient == nil`（即未授予 `network`）时直接失败，不回落到默认客户端。

#### feishu

- 注册名/平台：`feishu`（`adapters/feishu` 的 `PlatformName`）；版本 `v0.1.0`，作者 `core`。
- 权限：`[network, net_listen]`；配置键（`Options`）：`app_id`、`app_secret`、`verification_token`、`encrypt_key`、`listen_addr`、`path`、`base_url`。
- 接入方式：开放平台事件订阅回调。必填 `Name`、`AppID`、`AppSecret`、`VerificationToken`、`ListenAddr`；`path` 默认 `/feishu/event`，`base_url` 默认 `https://open.feishu.cn`。`encrypt_key` 非空时校验 `X-Lark-Signature`（时间戳 + nonce + body）并解密加密事件；`url_verification` 用 body 中的 token 校验后原样回写 `challenge`；正常事件校验 token 后先回 `200 {"code":0}` 再入队异步投递（队列容量 1024、2 个 worker，队列满丢弃并告警）。
- 发送：用 `tenant_access_token` 调开放平台 IM 接口。一条飞书消息只能有一种 `msg_type`，因此统一消息会被切成若干块依次发送（连续文本与 `@` 合并为 `text`，图片单独 `image`，卡片单独 `interactive`，文件给出 `file_key` 时按 `file` 发送），返回最后一块的 `message_id`；`ReplyTo` 或消息中的引用段决定回复目标，否则按 `Target.Kind`/`UserID`/`ChannelID` 解析 `receive_id`。
- 能力：`Text`/`Markdown`/`Image`/`At`/`Card`/`File`/`Reply`/`Private`/`Group` 全为 true。注意 `Markdown` 只是「不会在降级时被转成文本」：`buildChunks` 把 `SegMarkdown` 按纯文本写入 `text` 分块（飞书的 `text` 类型不渲染 Markdown，源码注释指明需要富文本时应改用卡片段），因此该能力位描述的是「Markdown 内容可原样送达」，而不是「飞书会渲染 Markdown」。
- 工厂：`newFromContext` 在 `ac.HTTPClient == nil`（即未授予 `network`）时直接失败。

---

## 相关文档

- [domain-model.md](domain-model.md)：`Event`、`Message`、`Segment`、`Target`、`Capabilities` 等公开领域模型的定义与 `Key*` 常量。
- [engine.md](engine.md)：发送前的降级、限流与重试，适配器装配与生命周期、`/adapters` 相关的管理路径。
- [grpc.md](grpc.md)：外部适配器进程的 `AdapterService`/`BotService` 协议、能力与平台名协商、权限交集与断连重连。
- [configuration.md](configuration.md)：`bots`/`adapters` 段的键、默认值、保留键校验与环境变量覆盖规则。
