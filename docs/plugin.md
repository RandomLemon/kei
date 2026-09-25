# kei 插件系统与 BotAPI

覆盖 `pkg/bot` 中的插件契约（`Plugin`/`Metadata`/`Permission`）、`Registrar` 与全部注册 API、`Reply` 构建器、`BotAPI`/`Storage`/`PluginContext` 及其权限降级行为，并给出仓库内 `plugins/echo`、`plugins/manage` 的真实实现。
不覆盖：路由匹配与中间件链的实现（见 [engine.md](engine.md)）、适配器契约（见 [adapter.md](adapter.md)）、外部插件的 gRPC 通道（见 [grpc.md](grpc.md)）、配置键表（见 [configuration.md](configuration.md)）。
面向用户的插件编写教程与完整示例见 [../README.md](../README.md)。

---

## 9. 插件系统

### 9.1 插件接口

在 `pkg/bot` 中定义（`plugin.go`）。生命周期为 `Setup -> Start -> Stop`：`Setup` 在事件处理开始前调用，用于注册规则；`Start` 用于启动插件自身的后台任务；`Stop` 必须幂等并释放资源。任何步骤返回错误都会阻止框架启动。

```go
type Plugin interface {
	// Metadata 返回插件元信息，必须包含唯一的 Name。
	Metadata() Metadata
	// Setup 通过 Registrar 注册触发规则。
	Setup(ctx context.Context, reg Registrar) error
	// Start 启动插件后台任务，允许为空实现。
	Start(ctx context.Context) error
	// Stop 停止插件并释放资源。
	Stop(ctx context.Context) error
}

type Metadata struct {
	// Name 是插件唯一名称。
	Name string
	// Version 是插件版本。
	Version string
	// Author 是插件作者。
	Author string
	// Description 是插件说明。
	Description string
	// Permissions 是插件申请的权限。
	Permissions []Permission
}

// HasPermission 判断插件是否申请了指定权限。
func (m Metadata) HasPermission(p Permission) bool
```

`HasPermission` 在 `m.Permissions` 中查找 `p`，同时把 `PermAll` 视为包含一切权限。

`Permission` 是插件与适配器共用的类型，常量全集（`plugin.go`）：

```go
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

插件侧 `PermAll` 的语义：`Metadata.HasPermission` 对 `*` 恒返回 true，因此声明 `*` 的插件自动获得全部权限（`internal/pluginmgr` 据此裁剪依赖，见 11.4）。适配器侧完全不同：`AdapterMetadata.HasPermission` 只做精确匹配，`*` 不展开；`internal/adaptermgr.ValidateRegistry` 与 `internal/adaptermgr/external` 在适配器声明 `*` 时直接报错。适配器视角的裁剪规则见 [adapter.md](adapter.md)。

注册表 API：

```go
// RegisterPlugin 注册编译期插件，通常由插件在 init() 中调用。
//
// 注册只记录实例，实际初始化由引擎按配置的启用列表执行。
func RegisterPlugin(p Plugin)          // p == nil 时忽略
func RegisteredPlugins() []Plugin      // 按注册顺序的快照
```

插件是否被实例化由配置决定（`plugins.<name>.enabled`，可简写为 `plugins.<name>: true`），入口程序通过空导入引入插件包；`plugins.<name>.grpc_addr` 非空时该名字被当作外部插件，进程内实例会被跳过。配置键与启用规则见 [configuration.md](configuration.md)。

生命周期由 `internal/pluginmgr.Manager` 执行：按注册顺序 `Setup` 与 `Start`、逆序 `Stop`，每阶段在独立 `context.WithTimeout` 中运行（默认各 15s，`Deps.SetupTimeout/StartTimeout/StopTimeout` 可覆盖），并带 `recover` 隔离，插件 panic 或阶段失败不会拖垮进程，只终止本次启动。

要求：

1. 插件只依赖 `pkg/bot`，不得依赖 `internal/`。
2. 插件通过 `init()` 注册，主程序通过空导入启用。
3. 插件必须声明权限。
4. 插件 Handler 必须可被 recover（引擎的全局中间件链保证，见 [engine.md](engine.md)）。

### 9.2 注册器

`Registrar` 是插件注册触发规则的入口，实现必须并发安全，且同一插件注册的规则相互隔离。仓库实现位于 `internal/router/registrar.go`（由 `Router.Registrar(plugin)` 返回，同一插件名多次调用共享同一份规则表与插件中间件列表）。

```go
type Registrar interface {
	// OnCommand 注册命令触发，name 不含前缀（例如 "echo"）。
	OnCommand(name string, h Handler, opts ...Option)
	// OnRegex 注册正则触发，expr 为 RE2 表达式，编译失败会在插件 Setup 阶段返回错误。
	OnRegex(expr string, h Handler, opts ...Option)
	// OnKeyword 注册关键词触发，命中任意词即触发。
	OnKeyword(words []string, h Handler, opts ...Option)
	// OnEvent 注册事件类型触发。
	OnEvent(t EventType, h Handler, opts ...Option)
	// OnAll 注册兜底处理，匹配所有未被其它条件排除的事件。
	OnAll(h Handler, opts ...Option)
	// Use 为该插件注册的所有规则追加中间件，按注册顺序由外到内包裹。
	Use(mw Middleware)
}

// Handler 是插件事件处理函数。
//
// 所有 Handler 都会被 Recover、Timeout 中间件包裹，因此允许 panic 与阻塞，
// 但必须响应 ctx 取消。
type Handler func(ctx context.Context, e *Event, r Reply) error

// Middleware 是洋葱模型中间件：返回的 Handler 负责在合适的时机调用 next。
type Middleware func(next Handler) Handler

// Option 用于定制注册的规则。
type Option func(*Rule)
```

各注册入口的实际默认值（`internal/router/registrar.go`）：

| 入口 | 预置的规则字段 | 记录失败的通道 |
| --- | --- | --- |
| `OnCommand` | `Command`，`EventType = bot.EventMessage` | `RegistrationErrors` |
| `OnRegex` | `Regex`（编译成功时），`EventType = bot.EventMessage` | 编译失败记入 `RegistrationErrors` 并跳过该规则，不 panic |
| `OnKeyword` | `Keywords`（切片被复制），`EventType = bot.EventMessage` | `RegistrationErrors` |
| `OnEvent` | `EventType`（不限定会话类型与平台） | `RegistrationErrors` |
| `OnAll` | 无过滤条件，所有事件都会命中 | `RegistrationErrors` |

与注释不一致处：`OnRegex` 的文档注释称「编译失败会在插件 Setup 阶段返回错误」，实际通道是 `Router.RegistrationErrors()`（`recordErr` 累计并记 Warn 日志），非法正则只是不注册该规则，`Setup` 本身仍返回 nil；引擎当前不读取 `RegistrationErrors`，需要审计正则问题的调用方可自行读取（`Engine.Router()` 暴露路由器）。

`Use` 只影响调用 `Use` 之后为该插件注册的规则：每条规则在注册时把当前的插件中间件列表复制进 `Rule.Middlewares`，此前已注册的规则保持不变。折叠顺序由内到外为 handler → 插件 `Use` 中间件 → 全局中间件；同一组内按注册顺序由外到内包裹，即「先注册的先执行」。

全局中间件中的 `middleware.Metrics(rec)`（`internal/middleware`）是规则命中与处理结果的唯一指标上报点：进入该中间件即表示所在规则已匹配当前事件（路由器只为命中的规则构造处理链），因此先调用 `rec.RuleMatched(plugin, rule)`，再在 `next` 返回后调用 `rec.EventHandled(plugin, rule, 耗时, err)`——规则随后是否被 Auth/RateLimit 等拒绝只体现在 `EventHandled` 的 result 标签里，命中计数仍然递增。`metrics.Recorder` 另有 `EventPublished`、`EventDropped`、`MessageSent` 与供外部适配器通道使用的 `AdapterReconnected`、`AdapterReconnectFailed`、`AdapterDisabled`（对应 `kei_adapter_reconnects_total{adapter,result}` 与 `kei_adapter_disabled_total{adapter}`），中间件链上报的指标族与全局链顺序见 [engine.md](engine.md)。

`Option` 是 `func(*Rule)`，仓库提供全部 8 个构造器（`pkg/bot/registrar.go`）：

```go
// WithPriority 设置规则优先级，数值越大越先执行。
func WithPriority(p int) Option
// WithID 设置规则 ID，便于日志与指标定位。
func WithID(id string) Option
// WithPlatforms 限定规则只在指定平台生效。
func WithPlatforms(platforms ...string) Option
// WithKind 限定规则只对指定会话类型生效。
func WithKind(k MessageKind) Option
// WithBotIDs 限定规则只在指定机器人上生效，传空列表表示任何机器人都命中不了。
func WithBotIDs(ids ...string) Option
// WithAdmin 标记规则为管理员专属，需要 Auth 中间件配合。
func WithAdmin() Option
// WithEventType 限定规则的事件类型，等价于 OnEvent，但可用于 OnAll/OnKeyword 等。
func WithEventType(t EventType) Option
// WithMatch 追加自定义断言。
func WithMatch(fn func(*Event) bool) Option
```

`WithBotIDs` 与引擎的每 bot 插件白名单配合：`Rule.BotIDs` 为 `nil` 时不做机器人过滤，非 `nil`（含空切片）表示白名单，`BotIDs` 为空的规则任何机器人都命中不了。`bots[].plugins` 非空时引擎在启动事件处理前调用 `Router.UpdateRules` 把每条规则的 `BotIDs` 收窄为允许它的 bot 名字；`WithPlatforms`、`WithKind`、`WithMatch` 的结果与之取「与」。判断逻辑在 `(*Rule).Matches`，匹配语义与优先级排序见 [engine.md](engine.md)。

规则结构（`Option` 的作用对象，完整定义在 `pkg/bot/registrar.go`）：

```go
type Rule struct {
	ID          string           // 未显式指定时由注册器生成 plugin:kind:index
	Priority    int              // 数值越大越先执行
	Platforms   []string         // 空表示全部平台
	BotIDs      []string         // nil 表示不过滤；非 nil 表示白名单
	EventType   EventType        // 空表示全部类型
	Kind        MessageKind      // 空表示全部会话类型
	Command     string           // 不含前缀，比较忽略大小写
	Regex       *regexp.Regexp   // 对消息纯文本做 FindStringSubmatch
	Keywords    []string         // 命中任意一个（忽略大小写的子串匹配）即触发
	Match       func(*Event) bool
	AdminOnly   bool             // 需要 Auth 中间件配合
	Handler     Handler
	Plugin      string           // 由注册器填充
	Middlewares []Middleware     // 该插件通过 Registrar.Use 声明的中间件
}

// Matches 判断规则是否命中事件（不包含 AdminOnly 与自定义断言之外的中间件逻辑）。
func (r *Rule) Matches(e *Event) bool
```

路由信息通过 ctx 传给 Handler（`pkg/bot/context.go`）。源码中不存在名为 `RuleContext` 的类型，路由信息的载体是 `bot.Route`：

```go
// Route 描述当前正在执行的路由规则，由路由器写入 ctx 供中间件与日志使用。
type Route struct {
	Plugin       string   // 规则所属插件名
	RuleID       string   // 规则 ID
	Trigger      string   // 触发方式的可读描述，例如 "command:echo"、"regex:^hi"
	Priority     int      // 规则优先级
	AdminOnly    bool     // 规则要求管理员身份，由 Auth 中间件校验
	RegexMatches []string // 正则规则的捕获结果（含完整匹配），非正则规则为空
}

func WithRoute(ctx context.Context, r *Route) context.Context // r == nil 时原样返回 ctx
func RouteFrom(ctx context.Context) (*Route, bool)            // 未注入时返回 false
```

用法：正则规则的捕获组只能经 `Route.RegexMatches` 读取，索引 0 是完整匹配。

```go
reg.OnRegex(`^天气\s+(\S+)$`, func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
	city := "北京"
	if route, ok := bot.RouteFrom(ctx); ok && len(route.RegexMatches) > 1 {
		city = route.RegexMatches[1]
	}
	return r.Text("查询 " + city).Send(ctx)
})
```

路由在每个 Handler 调用前写入 `Route`，并为每条规则创建独立的 `Reply`，因此多条规则同时命中时回复内容互不污染。

### 9.3 回复构建器

`Reply` 由引擎在调用 Handler 时注入（`pkg/bot/reply.go`）。所有链式方法返回 `Reply` 自身以便连续构建；一次 Handler 调用只应调用一次 `Send`，重复 `Send` 会重复发送。构建过程不加锁，`Reply` 不可跨 goroutine 使用。

```go
type Reply interface {
	// Text 追加纯文本段。
	Text(string) Reply
	// Markdown 追加 Markdown 段。
	Markdown(string) Reply
	// Image 追加图片段，参数为图片 URL 或平台文件标识。
	Image(string) Reply
	// At 追加 @ 段。
	At(userID string) Reply
	// Card 追加结构化卡片段，载荷由平台适配器解释。
	Card(card any) Reply
	// Send 把已构建的消息段发送到当前事件所在会话。
	Send(ctx context.Context) error
}

// ReplyFactory 为事件创建回复构建器，由引擎注入到路由器。
type ReplyFactory func(ctx context.Context, ev *Event) Reply
```

默认实现是 `internal/reply.Builder`（`reply.New(api bot.BotAPI, ev *bot.Event)`），由引擎通过 `Router.Options.Replies` 注入；`Router.Options.Replies` 为空时路由器退化为 `bot.NoopReply`。语义：

- `Text`/`Markdown`/`Image`/`At`/`Card` 分别追加 `SegText`+`KeyText`、`SegMarkdown`+`KeyText`、`SegImage`+`KeyURL`、`SegAt`+`KeyUserID`、`SegCard`+`KeyCard` 段。参数为空串或 `nil` 时**不追加任何段**（调用仍可继续链式）。
- 会话类型在 `New` 时确定：优先取 `ev.Message.Kind`，其次 `ev.Channel.Kind`，都取不到时用 `bot.MessagePrivate`。
- `Send` 把累积的段组装成 `bot.Message` 后交给 `BotAPI.Reply` 一次；`ev.Message` 非空时把入参消息 ID 写入 `msg.ID`，用于平台侧引用回复。没有消息段时不发送，返回 `reply: empty message`；`BotAPI` 为 `nil` 时返回 `reply: no bot api configured`。
- `Send` 不清空已累积的段，再次 `Send` 会用同样的段再发一次。
- `(*Builder).Segments()` 返回消息段的快照副本。

`NoopReply` 是只记录内容、不真正发送的实现，用于路由器未配置 `ReplyFactory` 时的兜底，以及插件单元测试中断言 Handler 构造出的内容：

```go
func NewNoopReply() *NoopReply
func (r *NoopReply) Segments() []Segment   // 段快照
func (r *NoopReply) Message() *Message     // 会话类型来自 SetKind
func (r *NoopReply) PlainText() string     // 所有文本段拼接结果
func (r *NoopReply) SetKind(k MessageKind) *NoopReply
func (r *NoopReply) Send(context.Context) error // 始终返回 nil，无副作用
```

消息段类型、`Segment.Data` 键名常量与 `Message.PlainText` 的读取规则见 [domain-model.md](domain-model.md)。构建出的消息在发送前由引擎按平台 `Capabilities` 降级，见 [adapter.md](adapter.md)。

### 9.4 插件示例

`plugins/echo` 的真实实现（`plugins/echo/echo.go`）：

```go
// Package echo 提供最简示例插件：回显命令参数。
package echo

import (
	"context"
	"strings"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Plugin 是 echo 插件，无状态、无外部依赖。
type Plugin struct{}

// 确保 Plugin 满足 bot.Plugin。
var _ bot.Plugin = (*Plugin)(nil)

// Metadata 返回插件元信息。
func (p *Plugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name:        "echo",
		Version:     "v0.1.0",
		Author:      "core",
		Description: "回显命令参数：/echo <文本>",
	}
}

// Setup 注册 /echo 命令。
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	reg.OnCommand("echo", func(ctx context.Context, e *bot.Event, r bot.Reply) error {
		if e.Command == nil || len(e.Command.Args) == 0 {
			return r.Text("用法: /echo <文本>").Send(ctx)
		}
		return r.Text(strings.Join(e.Command.Args, " ")).Send(ctx)
	}, bot.WithPriority(100), bot.WithID("echo:echo"))
	return nil
}

// Start 无后台任务。
func (p *Plugin) Start(context.Context) error { return nil }

// Stop 无资源需要释放。
func (p *Plugin) Stop(context.Context) error { return nil }

func init() {
	bot.RegisterPlugin(&Plugin{})
}
```

启用路径：插件包在 `init()` 中调用 `bot.RegisterPlugin`；入口程序空导入该包（`cmd/bot/main.go` 中 `_ "github.com/RandomLemon/kei/plugins/echo"`）；配置里写 `plugins: { echo: true }` 即启用，`enabledPlugins` 只把「已注册且配置启用」的插件交给引擎，配置启用但未注册的插件只记告警。插件目录中的空导入与 `plugins` 段配置见 [configuration.md](configuration.md)。

需要 `PluginContext`（配置、日志、存储、插件目录）的插件在 `Setup` 阶段用 `bot.PluginContextFrom(ctx)` 取出并保存，`plugins/manage` 就是这么做的：

```go
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	pc, ok := bot.PluginContextFrom(ctx)
	if !ok {
		return fmt.Errorf("manage: missing plugin context")
	}
	p.cfg = pc.Config
	p.start = time.Now()

	pluginOpts := []bot.Option{bot.WithPriority(100), bot.WithID("manage:plugins")}
	if p.cfg.Bool("plugins_admin_only", false) {
		pluginOpts = append(pluginOpts, bot.WithAdmin())
	}
	reg.OnCommand("plugins", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		return r.Text(p.describePlugins(pc.Catalog)).Send(ctx)
	}, pluginOpts...)

	reg.OnCommand("admin", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
		return r.Text("管理员校验通过").Send(ctx)
	}, bot.WithPriority(100), bot.WithID("manage:admin"), bot.WithAdmin())
	return nil
}
```

`plugins/manage` 的完整命令集为 `/ping`、`/version`、`/plugins`、`/adapters`、`/admin`；`/adapters` 把 `PluginContext.Adapters`（绑定信息）与 `bot.RegisteredAdapters()`（注册表）合并展示，是「插件与适配器同构注册」的直接示例。

---

## 11. BotAPI 与插件上下文

### 11.1 BotAPI

在 `pkg/bot` 中定义（`api.go`），是插件与平台交互的唯一入口。插件不得直接依赖任何平台 SDK；主动发送与回复都必须经过该接口，以便引擎统一执行权限校验、能力降级与限流。

```go
type BotAPI interface {
	// Send 向目标会话发送消息。
	Send(ctx context.Context, target Target, msg *Message) (*SendResult, error)
	// Reply 回复某个事件所在的会话。
	Reply(ctx context.Context, ev *Event, msg *Message) (*SendResult, error)
	// Logger 返回带插件字段的日志器。
	Logger() *slog.Logger
	// Storage 返回插件可用的键值存储。
	Storage() Storage
}
```

`*internal/engine.Engine` 本身满足 `BotAPI`，但插件拿到的是 `internal/engine` 的 `pluginAPI` 门面（权限校验与日志字段在此收口）：

- `Send`：未声明 `PermSendMessage` 时返回 `fmt.Errorf("engine: plugin %s lacks permission %s", ...)`，不发送；声明后转调 `Engine.Send`（发送前按适配器能力降级、按 bot 限流、失败按退避重试）。
- `Reply`：所有插件都可使用，无需额外权限。
- `Logger`：返回 `engine.log.With("plugin", <插件名>)` 派生的日志器。
- `Storage`：声明了 `PermStorage` 时返回引擎存储，否则返回拒绝式实现。

`Target` 与 `TargetFromEvent` 可以把当前事件转成发送目标，用于插件的主动发送：

```go
func TargetFromEvent(ev *Event) Target
```

事件上行与消息下行的完整链路、`SendResult`、`SendRequest` 与降级细节见 [domain-model.md](domain-model.md) 与 [adapter.md](adapter.md)。

### 11.2 Storage

```go
// ErrNotFound 表示 Storage 中不存在所请求的键。
//
// 实现必须保证「键不存在」返回可被 errors.Is(err, ErrNotFound) 识别的错误。
var ErrNotFound = errors.New("bot: storage: key not found")

// Storage 是插件可用的键值存储。
//
// MVP 使用内存实现，后续可替换为 Redis/SQLite；实现必须并发安全。
type Storage interface {
	// Get 读取键值，键不存在时返回 ErrNotFound。
	Get(ctx context.Context, key string) ([]byte, error)
	// Set 写入键值；ttl <= 0 表示永不过期。
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete 删除键值，键不存在时不返回错误。
	Delete(ctx context.Context, key string) error
}
```

内存实现位于 `internal/storage`：

- `storage.NewMemory() *Memory`：构造并发安全的存储并启动过期清理 goroutine（每 `janitorInterval = time.Minute` 扫一次）。使用完必须 `Close()` 释放后台 goroutine，`Close` 可重复调用。
- `Get` 返回值的副本，键不存在或已过期返回 `bot.ErrNotFound`；`Set` 拷贝入参切片，`ttl <= 0` 表示永不过期；`Delete` 对不存在的键返回 nil；`Len()` 返回当前条目数（含尚未清理的过期条目）。入参 ctx 已取消时立即返回 `ctx.Err()`。
- `storage.Denied() bot.Storage`：拒绝一切访问的实现，`Get`/`Set`/`Delete` 都返回 `storage.ErrPermissionDenied`。未声明 `PermStorage` 的插件（`internal/engine.pluginAPI`）与适配器（`internal/adaptermgr`）拿到的就是它。`ErrPermissionDenied` 定义在 `internal/storage`，插件只依赖 `pkg/bot` 时无法引用该变量，只能按「错误非 nil」或错误文本处理；可比较的公开哨兵只有 `bot.ErrNotFound`。

替换方式：`engine.Options.Storage` 传入自定义 `bot.Storage`；为 nil 时引擎内部新建 `storage.NewMemory()` 并在关闭时调用其 `Close`。`internal/pluginmgr.Deps.Storage` 则接收引擎注入的实例，按插件权限分发。

### 11.3 PluginContext

`PluginContext` 是插件运行期依赖的聚合，由 `internal/pluginmgr` 在 `Setup`/`Start`/`Stop` 阶段注入，插件内部用 `bot.PluginContextFrom(ctx)` 获取。

```go
type PluginContext struct {
	// Name 是插件自身名称，等于 Metadata().Name。
	Name string
	// Config 是该插件的独立配置，未配置时为空配置（方法仍然安全）。
	Config *Config
	// Logger 是已绑定 plugin 字段的日志器。
	Logger *slog.Logger
	// Storage 是插件可用的键值存储，插件需声明 PermStorage 才能使用。
	Storage Storage
	// HTTPClient 是带超时的 HTTP 客户端，插件需声明 PermNetwork 才能使用。
	HTTPClient *http.Client
	// Bot 是插件访问平台的唯一入口。
	Bot BotAPI
	// Catalog 提供插件列表，可能为 nil。
	Catalog PluginCatalog
	// Adapters 提供已加载适配器列表，可能为 nil。
	Adapters AdapterCatalog
}

func WithPluginContext(ctx context.Context, pc PluginContext) context.Context
func PluginContextFrom(ctx context.Context) (PluginContext, bool)

// PluginCatalog 提供已加载插件的元信息，供管理类插件使用。
type PluginCatalog interface {
	// Plugins 返回当前已加载插件的元信息快照。
	Plugins() []Metadata
}

// AdapterCatalog 提供已加载适配器的绑定信息，供管理类插件使用（定义在 adapter_registry.go）。
type AdapterCatalog interface {
	Adapters() []AdapterInfo
}
```

注入范围：`PluginContext` 只写入**生命周期方法**（`Setup`/`Start`/`Stop`）收到的 ctx。Handler 执行时的 ctx 由路由器构造，只携带 `Route`，不含 `PluginContext`；因此 Handler 需要的配置、日志、存储、目录应在 `Setup` 阶段取出后保存（`plugins/manage` 的做法）。

字段来源与取值：

| 字段 | 来源 | 说明 |
| --- | --- | --- |
| `Name` | `Metadata().Name` | 用于日志与配置查找 |
| `Config` | `pluginmgr.Deps.Configs[name]` | 缺失时为 `bot.NewConfig(nil)` 空配置，取值方法安全 |
| `Logger` | `pluginmgr.Deps.Logger` | 派生自根日志器并绑定 `plugin` 字段 |
| `Storage` | `pluginmgr.Deps.Storage` | 未声明 `PermStorage` 或引擎未提供存储时为 `storage.Denied()` |
| `HTTPClient` | `pluginmgr.Deps.HTTPClient` | 仅声明 `PermNetwork` 时非 nil |
| `Bot` | `pluginmgr.Deps.API(name, meta)` | 每插件独立门面；`Deps.API` 为 nil 时该字段为空（`nil` 接口） |
| `Catalog` | `pluginmgr.Deps.Catalog` | 引擎实现 `bot.PluginCatalog`；未接线时为 nil |
| `Adapters` | `pluginmgr.Deps.AdapterCatalog` | 引擎实现 `bot.AdapterCatalog`；未接线时为 nil |

`Config` 是插件的独立配置，来自配置中该插件名下的键值，支持 `"a.b.c"` 形式的多级键；所有取值方法对 nil 接收者与缺失键都返回默认值：

```go
func NewConfig(raw map[string]any) *Config
func (c *Config) Raw() map[string]any              // 原始映射，调用方不得修改
func (c *Config) Get(key string) (any, bool)       // 路径以 "." 分隔
func (c *Config) String(key, def string) string    // string/fmt.Stringer/fmt.Sprint
func (c *Config) Bool(key string, def bool) bool   // bool，或可被 strconv.ParseBool 解析的字符串
func (c *Config) Int(key string, def int) int      // int/int64/float64，或可被 strconv.Atoi 解析的字符串
func (c *Config) Duration(key string, def time.Duration) time.Duration // 字符串走 time.ParseDuration，数字按秒
func (c *Config) Strings(key string) []string      // []string 拷贝 / []any 中的字符串 / 逗号分隔字符串
func (c *Config) Unmarshal(v any) error            // json.Marshal(raw) 后 json.Unmarshal 到 v
```

### 11.4 权限与降级

插件侧权限全部在装配期由核心裁剪依赖，不在运行期事后拦截（唯一例外是 `BotAPI.Send` 的显式校验）：

| 权限 | 未声明时插件实际得到什么 |
| --- | --- |
| `send_message` | `BotAPI.Send` 返回错误 `engine: plugin <name> lacks permission send_message`；`Reply` 不受影响 |
| `network` | `PluginContext.HTTPClient` 为 nil，插件应自行降级或报错 |
| `storage` | `PluginContext.Storage` 与 `BotAPI.Storage()` 都是 `storage.Denied()`，任意调用返回 `storage.ErrPermissionDenied` |
| `admin` | 仅作声明与审计；管理员校验实际由 `Rule.AdminOnly` + Auth 中间件按 `auth.admin_users` 完成，与插件元信息无关 |
| `read_user` | 仅作声明与审计，核心当前不据此裁剪任何依赖（用户信息本就在 `Event.Sender` 中） |
| `net_listen` / `receive_event` | 属于适配器语义，进程内插件不涉及 |
| `*` | `Metadata.HasPermission` 视为包含一切权限，等价于声明全部权限；仅内置插件可用 |

要点：

1. `BotAPI` 是插件与平台交互的唯一入口，`Send` 需要 `PermSendMessage`，`Reply` 始终可用。
2. `Storage` 的 MVP 为内存实现，可通过 `engine.Options.Storage` 替换为 Redis/SQLite 等实现。
3. 插件通过 `PluginContext` 获取配置、日志、HTTP Client、Storage、插件目录与适配器目录。
4. 插件默认不能任意发消息（需 `PermSendMessage`）、不能读写存储（需 `PermStorage`）、不能发起出站请求（需 `PermNetwork`）；核心不提供文件访问能力。

外部插件的权限来自核心配置而非插件进程自报：`plugins.<name>.permissions`（缺失或为空时默认 `[send_message]`，支持列表或逗号分隔字符串），授权集合随 gRPC 令牌下发。外部适配器相反，权限由适配器进程上报后与 `adapters.<name>.permissions` 取交集。两边的越权调用都由 `internal/grpcsrv` 以 `codes.PermissionDenied` 拒绝：`BotService.SendMessage` 校验 `PermSendMessage`，`BotService.EmitEvent` 校验 `PermReceiveEvent`。协议细节见 [grpc.md](grpc.md)。

### 11.5 插件测试辅助

插件可以只用 `pkg/bot` 做单元测试，无需启动引擎：

- `bot.NewRecordingRegistrar() *RecordingRegistrar`：记录式 `Registrar`。只记录规则而不做匹配，测试代码取出规则后可直接调用其 `Handler`。语义与路由器一致：消息类触发的默认 `EventType` 为 `EventMessage`，非法正则记入 `Errors()` 而不是 panic。
  - 读取接口：`Rules() []*Rule`、`Errors() []error`、`Command(name string) (*Rule, bool)`、`Find(fn func(*Rule) bool) (*Rule, bool)`。
  - `Use` 会为**已记录**的规则追加中间件（与路由器「只影响其后注册的规则」不同）。
  - 未显式指定 ID 的规则自动得到 `rec:<kind>:<index>` 形式的 ID。
- `bot.NewNoopReply() *NoopReply`：记录段而不发送，用 `PlainText()` 断言文本、`Segments()`/`Message()` 断言段结构。
- `bot.WithPluginContext(ctx, pc)`：测试中手动构造 `PluginContext`（`Config`、`Logger`、`Catalog`、`Adapters` 可直接赋值）后注入 ctx，用于驱动依赖上下文的 `Setup`。
- `bot.WithRoute(ctx, &bot.Route{...})`：构造带路由信息的 ctx，用于测试读取 `RegexMatches` 或校验中间件对 `AdminOnly` 的处理。

```go
reg := bot.NewRecordingRegistrar()
if err := (&Plugin{}).Setup(bot.WithPluginContext(ctx, pc), reg); err != nil { ... }
rule, ok := reg.Command("hello")
reply := bot.NewNoopReply()
_ = rule.Handler(ctx, ev, reply)
fmt.Println(reply.PlainText())
```

仓库中的实际用例：`plugins/echo/echo_test.go`（记录器 + NoopReply 断言回显内容）、`plugins/manage/manage_test.go`（手动构造 `PluginContext`，含 `Catalog`/`Adapters` 假实现）。

## 相关文档

- [插件契约与消息段类型](domain-model.md)：`Event`、`Message`、`Segment`、`Target`、`Capabilities` 的定义。
- [Engine、路由与中间件](engine.md)：`Rule.Matches` 的匹配语义、优先级与中间件链的执行顺序。
- [Adapter 平台适配层](adapter.md)：权限裁剪的适配器侧规则、能力与降级。
- [配置](configuration.md)：`plugins` 段、`bots[].plugins` 白名单与环境变量覆盖。
