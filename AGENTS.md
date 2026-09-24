# AGENTS.md

本文件用于指导 AI coding agent 实现一个基于 Go 的 chatbot 框架。请严格按照本文档的架构、接口、约定和任务顺序进行实现。实现过程中优先保证核心抽象稳定、MVP 可运行、测试可验证。

---

## 1. 项目概述

项目名称：`kei`  
模块路径：`github.com/RandomLemon/kei`

目标：构建一个 Go 语言编写的 chatbot 框架，满足：

1. 可接入不同 IM 平台：QQ、微信、飞书等。
2. 平台协议与业务逻辑解耦。
3. 第三方开发者可以方便快捷地开发功能插件。
4. 插件支持消息处理、回复、命令触发、正则触发、关键词触发、事件监听等能力。
5. 第三方开发者可以像开发插件一样开发平台适配器：在自己的包内注册到注册表即可接入，无需改动核心代码。
6. 核心引擎稳定，平台适配器与插件均可进程内加载，也可扩展到外部 gRPC 进程。

非目标：

1. 不实现个人微信逆向协议。
2. 不实现完整管理后台 UI。
3. 不实现复杂的自然语言处理模型。
4. 初期不追求支持所有平台，优先实现 Mock 和飞书或 OneBot 之一。

---

## 2. 技术栈与约束

- Go 1.22+
- 优先使用标准库：`context`、`log/slog`、`net/http`、`encoding/json`、`sync`、`time`。
- 配置使用 YAML：`gopkg.in/yaml.v3`。
- 外部插件与外部适配器协议使用 gRPC：`google.golang.org/grpc`、`google.golang.org/protobuf`。
- 禁止使用 Go `plugin` 作为主要插件机制。
- 适配器与插件同等对待：都通过 `pkg/bot` 的注册表注册，由配置驱动装配；`cmd/` 与 `internal/` 中不得出现平台名分支。
- 所有对外接口放在 `pkg/bot`，内部实现放在 `internal/`。
- 所有阻塞操作必须接收 `context.Context`。
- 所有插件 Handler 必须支持超时和 panic recover。
- 所有平台回调必须快速 ACK，业务异步处理。
- 所有事件必须支持去重。
- 代码必须通过 `go test ./...`、`go vet ./...` 和 `go test -race ./...`。

---

## 3. 总体架构

```text
QQ / 微信 / 飞书 / OneBot / Mock
        │
        ▼
┌──────────────────┐
│   Adapter 适配层  │  签名校验、协议解析、事件转换、API 调用
└──────────────────┘
        │ 统一 Event
        ▼
┌──────────────────┐
│   EventBus 事件总线 │  队列、并发、按会话保序、去重
└──────────────────┘
        │
        ▼
┌──────────────────┐
│   Engine 核心引擎  │
│  Middleware 链    │  recover/log/ratelimit/auth/metrics
│  Router 路由      │  命令、正则、关键词、事件类型
│  Plugin Handler   │  第三方插件处理
└──────────────────┘
        │ Reply / BotAPI
        ▼
┌──────────────────┐
│   Adapter.Send    │  统一消息段转平台消息
└──────────────────┘
```

核心原则：

1. 平台无关：插件不直接依赖 QQ、飞书、微信 SDK。
2. 插件轻量：第三方只需实现接口、注册命令/正则/关键词。
3. 适配器可插拔：新增平台只需实现 Adapter 并在自己的包（可以是独立 module）内注册，核心代码零改动。
4. 插件与适配器同构：同样的注册表、元信息与权限声明、配置驱动装配、以及外部 gRPC 进程形态。
5. 消息段统一：文本、图片、At、Markdown、卡片等统一为 Segment。
6. 动态扩展优先用外部进程/gRPC，而不是 Go `plugin`。

---

## 4. 目录结构

请按以下结构创建项目。允许根据实际情况微调，但 `pkg/bot`、`internal/`、`adapters/`、`plugins/` 的职责必须保持清晰。

```text
chatbot/
├── AGENTS.md
├── go.mod
├── go.sum
├── cmd/
│   ├── bot/
│   │   └── main.go           # 入口：空导入适配器/插件 + 配置驱动装配，不含平台分支
│   └── example-adapter/
│       └── main.go           # 外部 gRPC 适配器示例（对照 cmd/example-plugin）
├── pkg/
│   ├── bot/              # 公开 SDK：Event、Message、Adapter 注册表、Plugin、
│   │                     #          Registrar、Reply、BotAPI、Storage
│   └── message/          # 消息段、构建器
├── internal/
│   ├── engine/           # 核心引擎
│   ├── adaptermgr/       # 适配器装配：注册表查表、权限裁剪、外部适配器通道
│   ├── grpcsrv/          # BotService（插件与外部适配器共用）
│   ├── router/           # 路由匹配
│   ├── middleware/       # 中间件
│   ├── eventbus/         # 事件总线
│   └── pluginmgr/        # 插件管理
├── adapters/             # 内置适配器；第三方适配器不放在这里（见下）
│   ├── feishu/
│   ├── onebot/
│   ├── wecom/
│   └── mock/
├── plugins/
│   └── echo/
├── proto/
│   ├── plugin.proto      # 外部插件协议 + BotService（含 EmitEvent）
│   └── adapter.proto     # 外部适配器协议（同 package/go_package）
└── configs/
    └── config.yaml
```

第三方适配器放在独立包或独立 module 中，只依赖 `pkg/bot`，核心仓库不为它改动任何文件（与第三方插件完全一致）：

```text
kei-adapter-myim/         # 独立仓库/模块
├── go.mod                # require github.com/RandomLemon/kei
├── myim.go               # 实现 bot.Adapter
├── register.go           # func init() { bot.RegisterAdapter(meta, New) }
└── README.md
```

接入方式只有两步：主程序空导入该 module，配置里 `bots[].adapter: myim`（外部进程形态则在 `adapters:` 段声明 `grpc_addr`）。

---

## 5. 核心领域模型

在 `pkg/bot` 中定义以下类型。这些类型是公开 SDK，必须保持稳定。

```go
package bot

import (
	"context"
	"regexp"
	"time"
)

type EventType string

const (
	EventMessage EventType = "message"
	EventNotice  EventType = "notice"
	EventRequest EventType = "request"
	EventMeta    EventType = "meta"
)

type Event struct {
	ID       string
	Type     EventType
	Platform string
	BotID    string
	Time     time.Time

	Message *Message
	Sender  *User
	Channel *Channel
	Command *Command

	Raw any // 原始平台事件，调试用
}

type Command struct {
	Name string
	Args []string
	Raw  string
}

type Message struct {
	ID       string
	Kind     MessageKind
	Segments []Segment
}

type MessageKind string

const (
	MessagePrivate MessageKind = "private"
	MessageGroup   MessageKind = "group"
	MessageChannel MessageKind = "channel"
)

type Segment struct {
	Type SegmentType
	Data map[string]any
}

type SegmentType string

const (
	SegText     SegmentType = "text"
	SegImage    SegmentType = "image"
	SegAt       SegmentType = "at"
	SegFace     SegmentType = "face"
	SegReply    SegmentType = "reply"
	SegMarkdown SegmentType = "markdown"
	SegCard     SegmentType = "card"
	SegFile     SegmentType = "file"
)

type User struct {
	ID    string
	Name  string
	IsBot bool
}

type Channel struct {
	ID   string
	Name string
	Kind MessageKind
}
```

---

## 6. Adapter 平台适配层

适配器与插件同等对待：都可以由第三方开发（进程内注册，或作为独立进程经 gRPC 接入），核心只通过注册表 + 工厂按配置装配，不为任何平台写死代码分支。

### 6.1 适配器接口

在 `pkg/bot` 中定义 Adapter 接口。这是第三方实现的最小契约，必须保持稳定。

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

// EventSink 是适配器向上投递事件的唯一出口。实现方（引擎）必须快速返回，
// 业务处理在事件总线的 worker 中进行，以免阻塞平台回调线程。
type EventSink interface {
	// Emit 投递一个事件。返回值仅表示是否成功入队，不表示已被处理。
	Emit(ctx context.Context, ev *Event) error
}

// SendRequest 是一次发送请求。
type SendRequest struct {
	// BotID 指定使用哪个机器人实例发送（配置中的 bots[].name）。
	BotID   string
	Target  Target
	Message *Message
	// ReplyTo 非空时表示引用回复该消息 ID。
	ReplyTo string
}

// Target 描述消息接收方。
type Target struct {
	// Platform 是平台名。
	Platform string
	// BotID 是机器人标识；同一平台部署多个实例时必填。
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
	MessageID string
	Raw       any
}

// Capabilities 声明平台支持的消息能力，引擎据此降级不支持的消息段。
type Capabilities struct {
	Text     bool
	Markdown bool
	Image    bool
	At       bool
	Card     bool
	File     bool
	Reply    bool
	Private  bool
	Group    bool
}

// Supports 判断平台是否支持某类消息段。
func (c Capabilities) Supports(t SegmentType) bool

// Degrade 按平台能力降级消息段，返回可直接发送的消息。降级规则见 6.4。
func Degrade(msg *Message, caps Capabilities) *Message
```

### 6.2 注册表与工厂

适配器以「元信息 + 工厂」注册到 `pkg/bot` 的全局注册表，机制与插件注册（`RegisterPlugin`）同构：注册只记录，是否实例化由配置决定。

```go
// AdapterFactory 依据 AdapterContext 构造一个适配器实例。
//
// 注册的是工厂而非单例：同一平台可以有多个实例（配置中的多个 bot，
// 例如两个飞书应用），实例状态不得放在包级变量里。
type AdapterFactory func(ctx AdapterContext) (Adapter, error)

// AdapterContext 是构造适配器实例所需的依赖与配置（对应插件侧的 PluginContext）。
type AdapterContext struct {
	// BotID 是实例名（bots[].name）。适配器必须把它写入 Event.BotID 与 SendRequest.BotID。
	BotID string
	// Config 是该实例的适配器私有配置（bots[] 条目中除 name/adapter/plugins 外的键），
	// 支持 "a.b.c" 多级键读取。
	Config *Config
	// Logger 不为 nil；适配器日志必须使用它，并带上 bot/adapter 字段。
	Logger *slog.Logger
	// Storage 仅在声明 PermStorage 时是可用实现，否则为拒绝式实现。
	Storage Storage
	// HTTPClient 仅在声明 PermNetwork 时非 nil。
	HTTPClient *http.Client
}

// AdapterMetadata 是适配器元信息（对应插件侧的 Metadata）。
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
	// Permissions 是适配器申请的权限，见 6.3。
	Permissions []Permission
	// Options 是适配器接受的配置键（bots[] 条目上的私有键），用于核心对未知
	// 配置键告警；为空表示不校验。
	Options []string
}

// HasPermission 判断适配器是否申请了指定权限。
func (m AdapterMetadata) HasPermission(p Permission) bool

// RegisterAdapter 注册编译期适配器工厂，通常由适配器包在 init() 中调用。
//
// 与插件一样只记录，不构造实例；Name 为空或 factory 为 nil 的注册被忽略。
// 同名重复注册不是「后者覆盖前者」：必须在启动校验阶段报错（见第 7 章）。
func RegisterAdapter(meta AdapterMetadata, factory AdapterFactory)

// RegisteredAdapters 返回按注册顺序排列的元信息快照。
func RegisteredAdapters() []AdapterMetadata

// LookupAdapter 按注册名取元信息与工厂，未注册时返回 false。
func LookupAdapter(name string) (AdapterMetadata, AdapterFactory, bool)
```

适配器不持有 `BotAPI`（`AdapterContext` 中刻意没有该字段）：事件只能经 `EventSink` 上行、消息只能经 `Send` 下行，避免平台层反向进入引擎造成递归与死锁。

### 6.3 元信息与权限

`Permission` 类型与常量全集见 9.1。核心按 `AdapterMetadata.Permissions` 在装配期裁剪依赖，并把权限透出给 `/adapters` 审计：

| 权限 | 含义 | 未声明时核心的行为 |
| --- | --- | --- |
| `send_message` | 出站发送（适配器自己的 `Send`） | 仅审计；权限模型与插件共用 |
| `network` | 出站网络请求 | `AdapterContext.HTTPClient` 为 nil |
| `storage` | 读写去重/状态缓存 | 注入拒绝式 `Storage`（`ErrPermissionDenied`） |
| `net_listen` | 启动入站监听（webhook / 长连接） | 配置里出现保留键 `listen_addr` 时启动失败 |
| `receive_event` | 向核心投递事件（外部适配器经 `BotService.EmitEvent`） | 外部适配器调用被拒绝 |
| `*` | 不适用于适配器（声明即启动失败） | — |

要求：

1. 权限必须如实声明；适配器不得声明 `*`（注册表校验直接报错），`*` 只对内置插件有意义。
2. 声明与授予取交集：外部适配器实际可用权限 = `AdapterMetadata.Permissions` ∩ `adapters.<name>.permissions`。
3. 未声明 `network` 却发起出站请求、未声明 `storage` 却写缓存，属于适配器缺陷；核心只保证不为其提供依赖，不做事后拦截。

### 6.4 能力与降级

1. 引擎在调用 `Adapter.Send` 之前统一执行 `bot.Degrade(msg, caps)`，适配器只需如实声明 `Capabilities`。
2. 降级规则：Markdown → 纯文本；图片 → `[image] url`；文件 → `[file] name url`；At → `@名字`；表情 → `[face] id`；卡片 → `[card] json`；引用段在平台不支持引用时丢弃；未知段类型 → `[类型]`。全部段都受支持时原样返回，不产生拷贝。
3. 降级必须记录日志与指标；整条消息因降级变为空时发送失败并返回错误，不得静默丢消息。
4. 外部适配器的能力以 `AdapterStartResponse.capabilities` 为唯一权威来源；缺失或全零按「仅文本」处理并告警。

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

### 6.6 实现要求

1. 内置适配器放 `adapters/<platform>`；第三方适配器放独立包或独立 module，只依赖 `pkg/bot`，不得依赖 `internal/`，也不得要求核心侧改动。
2. 适配器必须在自己的 `init()` 中调用 `RegisterAdapter`；主程序通过空导入启用，是否实例化由配置中的 `bots[].adapter` 决定。
3. Adapter 负责签名校验、协议解析、事件转换为统一 `Event`、把统一 `Message` 转为平台消息并发送。
4. 必须声明 `Capabilities` 与 `Permissions`，声明必须与实现一致；错报能力会导致发送失败或内容丢失。
5. 工厂必须支持同平台多实例：状态只能挂在实例上，`New` 每次返回互相隔离的实例。
6. `Event.ID` 必须稳定且平台内唯一（用于去重），`Event.Platform` 必须属于 `AdapterMetadata.Platforms`，`Event.BotID` 必须等于 `AdapterContext.BotID`。
7. `Start` 返回前必须完成监听/连接；`Stop` 必须幂等并等待已接收事件投递完毕；适配器内部 goroutine 必须有退出机制，不得泄漏。
8. 平台回调必须快速 ACK：先 ACK 平台，再异步投递到核心；`EventSink.Emit` 返回只表示已入队。入队失败必须记录日志与指标，不得静默丢弃。
9. 配置与密钥只能经 `AdapterContext.Config` 读取，不得直接读环境变量或文件。
10. 重复投递同一事件必须安全（核心按 `Event.ID` 去重），但适配器不得依赖去重来掩盖自身的重复投递。
11. Mock Adapter 必须实现，用于本地测试和事件回放。
12. 适配器的外部进程形态（gRPC）见第 12 章，行为要求与本章完全一致。

---

## 7. Engine 核心引擎

在 `internal/engine` 中实现。

```go
type Engine struct {
	adapters map[string]bot.Adapter
	router   *router.Router
	bus      eventbus.EventBus
	plugins  map[string]bot.Plugin
	api      bot.BotAPI
}

func (e *Engine) Run(ctx context.Context) error {
	// 启动插件、适配器、事件消费循环
}

func (e *Engine) handle(ctx context.Context, ev *bot.Event) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := e.router.Dispatch(ctx, ev); err != nil {
		// 记录日志
	}
}
```

要求：

1. 启动顺序：加载配置 -> 初始化日志/指标/存储 -> 注册编译期插件与适配器（空导入）-> 校验注册表与配置 -> 连接外部插件与外部适配器 -> 按配置装配 Adapter -> 启动 EventBus worker -> 启动插件 -> 启动 Adapter。
2. 关闭顺序：停止接收 -> 等待处理完成 -> 停止 Adapter（含外部适配器通道）-> 停止插件 -> 停止 gRPC 服务。
3. 支持优雅退出。
4. 每个 Adapter 独立 goroutine 接收事件。
5. EventBus 使用 worker pool。
6. 同一会话按 `platform:botID:channelID:userID` 哈希保序。
7. 插件 Handler 必须支持 `context` 超时和 panic recover。
8. 发送消息要处理平台限流，可加重试和令牌桶。
9. 适配器只能经注册表装配：装配逻辑集中在 `internal/adaptermgr`，按 `bot.LookupAdapter` 的结果构造 `bot.AdapterContext`；`cmd/` 与 `internal/` 不得出现平台名分支（`switch adapter`）。
10. 启动校验必须报错且信息可行动：`bots[].adapter` 既未注册也未在 `adapters:` 段声明、注册名重复、元信息缺 `Name` 或 `Platforms`。错误必须列出所有已注册适配器名。
11. `AdapterMetadata.Permissions` 是装配裁剪的唯一依据：未声明 `storage`/`network` 时分别注入拒绝式 Storage 与 nil HTTPClient；未声明 `net_listen` 却配置了保留键 `listen_addr` 时启动失败。
12. 外部适配器崩溃只影响其绑定的 bot：记录日志与指标、退避重连，重连失败达上限时停用该 bot，核心进程不得退出。
13. 管理命令 `/adapters` 输出已注册适配器（名称、版本、平台、权限、进程内/外部）与每个 bot 的绑定关系。

---

## 8. EventBus 事件总线

在 `internal/eventbus` 中实现。

```go
type EventBus interface {
	Publish(ctx context.Context, ev *bot.Event) error
	Subscribe(handler func(context.Context, *bot.Event)) error
}
```

要求：

1. MVP 可使用 `chan *bot.Event` + worker pool。
2. 必须支持按会话保序。
3. 必须支持事件去重，推荐用事件 ID + TTL 缓存。
4. 必须支持优雅关闭，关闭前消费完已入队事件。
5. 必须提供单元测试验证并发安全和去重。

---

## 9. 插件系统

### 9.1 插件接口

在 `pkg/bot` 中定义。

```go
type Plugin interface {
	Metadata() Metadata
	Setup(ctx context.Context, reg Registrar) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type Metadata struct {
	Name        string
	Version     string
	Author      string
	Description string
	Permissions []Permission
}

type Permission string

// 支持的权限；插件与适配器共用同一类型（适配器侧的裁剪规则见 6.3）。
const (
	PermSendMessage Permission = "send_message"
	PermReadUser    Permission = "read_user"
	PermNetwork     Permission = "network"
	PermStorage     Permission = "storage"
	// PermNetListen 允许启动入站监听（webhook / 长连接），适配器使用。
	PermNetListen Permission = "net_listen"
	// PermReceiveEvent 允许向核心投递事件（外部适配器的 BotService.EmitEvent），适配器使用。
	PermReceiveEvent Permission = "receive_event"
	// PermAdmin 允许执行管理员命令（配合 Auth 中间件）。
	PermAdmin Permission = "admin"
	// PermAll 表示全部权限，仅内置插件与内置适配器可用。
	PermAll Permission = "*"
)
```

### 9.2 注册器

```go
type Registrar interface {
	OnCommand(name string, h Handler, opts ...Option)
	OnRegex(expr string, h Handler, opts ...Option)
	OnKeyword(words []string, h Handler, opts ...Option)
	OnEvent(t EventType, h Handler, opts ...Option)
	OnAll(h Handler, opts ...Option)
	Use(mw Middleware)
}

type Handler func(ctx context.Context, e *Event, r Reply) error

type Middleware func(next Handler) Handler

type Option func(*Rule)
```

### 9.3 回复构建器

```go
type Reply interface {
	Text(string) Reply
	Markdown(string) Reply
	Image(string) Reply
	At(userID string) Reply
	Card(card any) Reply
	Send(ctx context.Context) error
}
```

### 9.4 插件示例

在 `plugins/echo` 中实现 echo 插件：

```go
package echo

import (
	"context"
	"strings"

	"github.com/RandomLemon/kei/pkg/bot"
)

type EchoPlugin struct{}

func (p *EchoPlugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name:    "echo",
		Version: "v1.0.0",
		Author:  "core",
	}
}

func (p *EchoPlugin) Setup(ctx context.Context, reg bot.Registrar) error {
	reg.OnCommand("echo", func(ctx context.Context, e *bot.Event, r bot.Reply) error {
		text := strings.Join(e.Command.Args, " ")
		return r.Text(text).Send(ctx)
	})
	return nil
}

func (p *EchoPlugin) Start(ctx context.Context) error { return nil }
func (p *EchoPlugin) Stop(ctx context.Context) error  { return nil }

func init() {
	bot.RegisterPlugin(&EchoPlugin{})
}
```

要求：

1. 插件只依赖 `pkg/bot`，不得依赖 `internal/`。
2. 插件通过 `init()` 注册，主程序通过空导入启用。
3. 插件必须声明权限。
4. 插件 Handler 必须可被 recover。

---

## 10. 路由与中间件

在 `internal/router` 和 `internal/middleware` 中实现。

路由规则：

```go
type Rule struct {
	ID        string
	Priority  int
	Platforms []string
	EventType bot.EventType
	Kind      bot.MessageKind
	Command   string
	Regex     *regexp.Regexp
	Keywords  []string
	Handler   bot.Handler
}
```

要求：

1. 支持命令、正则、关键词、事件类型、平台过滤、消息类型、权限、优先级。
2. 优先级数值越大越先匹配。
3. 中间件采用洋葱模型。
4. 必须实现以下中间件：
   - Recover：捕获插件 panic。
   - Logger：记录事件、耗时、插件名。
   - Metrics：Prometheus 指标，可后置。
   - RateLimit：按用户/群/插件限流。
   - Auth：管理员校验。
   - Dedup：消息去重。
   - Timeout：防止插件卡死。
5. 中间件必须可组合、可测试。

---

## 11. BotAPI 与插件上下文

在 `pkg/bot` 中定义。

```go
type BotAPI interface {
	Send(ctx context.Context, target Target, msg *Message) (*SendResult, error)
	Reply(ctx context.Context, ev *Event, msg *Message) (*SendResult, error)
	Logger() *slog.Logger
	Storage() Storage
}

type Storage interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

type PluginContext struct {
	Config     *Config
	Logger     *slog.Logger
	Storage    Storage
	HTTPClient *http.Client
	Bot        BotAPI
}
```

要求：

1. `BotAPI` 是插件与平台交互的唯一入口。
2. `Storage` MVP 可用内存实现，后续可替换 Redis/SQLite。
3. 插件通过 `PluginContext` 获取配置、日志、HTTP Client、Storage。
4. 插件默认不能读文件、不能任意发消息，需要声明权限。

---

## 12. 外部插件与外部适配器 gRPC 协议

外部插件与外部适配器共用同一套通道与信任模型：核心只做 `BotService` 的 Server，并作为客户端 dial 外部进程；外部进程实现自己的服务并反向调用核心的 `BotService`。

### 12.1 拓扑

```text
外部插件进程：实现 PluginService（核心 dial 调用）→ 反向调用核心 BotService.SendMessage
核心进程：    实现 BotService（Server，监听 grpc.addr）+ 作为客户端 dial 外部进程
外部适配器进程：实现 AdapterService（核心 dial 调用）→ 反向调用核心 BotService.EmitEvent

数据流：平台事件 → 适配器 → EmitEvent / EventSink.Emit → EventBus → Engine → Reply
        → BotService.SendMessage（外部插件）或 AdapterService.Send（适配器）→ 平台
```

统一约定：

1. 事件投递：核心 dial 外部插件并调用 `PluginService.HandleEvent`；适配器方向相反，事件由平台推给适配器，适配器再经 `EmitEvent` 送进核心。
2. 事件上行：外部适配器经 `BotService.EmitEvent` 投递（对应进程内适配器的 `EventSink.Emit`）。
3. 发送：外部插件经 `BotService.SendMessage` 回复；核心经 `AdapterService.Send` 让适配器发送。
4. 认证：`token` 必填且与核心配置一致，可选 mTLS（`grpc` 段的 `cert_file`/`key_file`/`ca_file`）。
5. 崩溃隔离：外部进程崩溃或断连只影响其自身绑定的插件/bot，核心记录日志与指标，绝不退出。
6. 语言无关：`proto/` 是唯一契约，外部插件与适配器可用 Python、Node.js、Rust 实现。

### 12.2 proto

`proto/plugin.proto`（插件 + `BotService`）与 `proto/adapter.proto`（适配器）同属 `package plugin`、共用 `go_package = github.com/RandomLemon/kei/proto/pluginpb`，`adapter.proto` 通过 `import "plugin.proto"` 复用 `Event`、`Message`、`Target`、`Empty`、`SendResponse` 等定义。

```proto
// proto/plugin.proto：BotService 增加适配器上行事件入口
service BotService {
  rpc SendMessage(SendRequest) returns (SendResponse);
  rpc GetConfig(GetConfigRequest) returns (GetConfigResponse);
  rpc Log(LogRequest) returns (Empty);
  // EmitEvent 供外部适配器投递平台事件，需要 receive_event 权限；
  // 返回 ok 只表示事件已入队，不表示已被处理。
  rpc EmitEvent(EmitEventRequest) returns (EmitEventResponse);
}
```

```proto
// proto/adapter.proto
syntax = "proto3";

package plugin;                 // 与 plugin.proto 同包，复用其消息定义
import "plugin.proto";

option go_package = "github.com/RandomLemon/kei/proto/pluginpb";

// AdapterInfo 是核心下发给外部适配器的身份与权限，对应 bot.AdapterMetadata。
// AdapterInitResponse 中回传时表示适配器的实际上报值（含版本、平台与配置键）。
message AdapterInfo {
  string name = 1;
  string version = 2;
  string author = 3;
  string description = 4;
  repeated string platforms = 5;
  repeated string permissions = 6;
  repeated string options = 7;   // 适配器接受的配置键，用于未知键告警
}

// AdapterInitRequest 是核心连接适配器后的初始化参数。
message AdapterInitRequest {
  AdapterInfo info = 1;
  string token = 2;        // 适配器反向调用核心的令牌
  string core_addr = 3;    // 核心 BotService 监听地址
  string config_json = 4;  // adapters.<name> 段中除通道键外的进程级配置
}

// AdapterInitResponse 回传适配器实际身份；platforms 必须非空，name 必须与配置名一致。
message AdapterInitResponse {
  bool ok = 1;
  string error = 2;
  AdapterInfo info = 3;
}

// AdapterInstance 描述一个要与适配器绑定的 bot 实例，每个实例 Start 一次。
message AdapterInstance {
  string bot_id = 1;       // bots[].name
  string platform = 2;     // 写入 Event.Platform
  string config_json = 3;  // 该实例的适配器私有配置
}

// AdapterStartResponse 返回实例启动结果与能力声明（该实例能力的唯一权威来源）。
message AdapterStartResponse {
  bool ok = 1;
  string error = 2;
  Capabilities capabilities = 3;
  string platform = 4;     // 该实例的权威平台名，必须与 AdapterInstance.platform 一致
}

message AdapterStopRequest {
  string bot_id = 1;
}

// Capabilities 对应 bot.Capabilities。
message Capabilities {
  bool text = 1;
  bool markdown = 2;
  bool image = 3;
  bool at = 4;
  bool card = 5;
  bool file = 6;
  bool reply = 7;
  bool private = 8;
  bool group = 9;
}

// AdapterSendRequest 是核心请求适配器向平台发送消息（对应 bot.SendRequest）。
message AdapterSendRequest {
  string bot_id = 1;
  Target target = 2;       // 复用 plugin.proto
  Message message = 3;     // 复用 plugin.proto
  string reply_to = 4;
}

message EmitEventRequest {
  string token = 1;
  Event event = 2;         // 复用 plugin.proto
}

message EmitEventResponse {
  bool ok = 1;
  string error = 2;
}

// AdapterService 由外部适配器进程实现，核心 dial 后调用。
service AdapterService {
  rpc Init(AdapterInitRequest) returns (AdapterInitResponse);
  rpc Start(AdapterInstance) returns (AdapterStartResponse);
  rpc Stop(AdapterStopRequest) returns (Empty);
  rpc Send(AdapterSendRequest) returns (SendResponse);   // 复用 plugin.proto
  rpc Shutdown(Empty) returns (Empty);
}
```

### 12.3 要求

1. 核心侧实现放 `internal/adaptermgr`（对照 `internal/pluginmgr`）：注册表查表与权限裁剪、外部适配器代理；`BotService` 的 gRPC 实现提到 `internal/grpcsrv`，由插件与外部适配器共用。
2. 一个外部适配器进程可服务多个 bot：`Init` 一次，`Start` 每个绑定的 bot 一次，`Stop` 幂等；实例状态必须按 `bot_id` 隔离。
3. 事件上行只经 `BotService.EmitEvent`：适配器必须先 ACK 平台，再调用它；`ok` 只表示已入队。
4. 授权：`EmitEvent` 需要 `receive_event` 权限，`GetConfig`/`Log` 与插件同一规则；token 缺失、token 不匹配或权限不足时返回错误，事件不得落到事件总线。
5. 能力协商：`Start` 返回的 `Capabilities` 是该实例的唯一权威声明；全零按「仅文本」处理并告警，核心据此执行降级（见 6.4）。
6. 平台名协商：平台名以 `AdapterInitResponse.info.platforms` 为准；上报多个平台时必须在 `adapters.<name>.platform` 指定，否则启动失败；实例上报的 `platform` 与之一致才接受。
7. 崩溃隔离与重连：外部适配器断连时该 bot 进入降级状态（不再产生事件、发送返回明确错误）并按指数退避重连；达到重连上限只停用该 bot，核心进程与其他 bot 不受影响。
8. 生命周期对齐：核心关闭时按「停止接收 -> 等待处理完成 -> `Stop` 各实例 -> `Shutdown`」顺序收尾，且必须带超时。
9. MVP 先做进程内适配器注册表，外部 gRPC 适配器作为阶段 8（对照外部插件的阶段 6）；外部插件协议的最小实现仍是阶段 6。

---

## 13. 配置

在 `configs/config.yaml` 提供示例。

```yaml
bots:
  - name: feishu-main
    adapter: feishu         # 引用注册表中的适配器名
    app_id: xxx
    app_secret: xxx

  - name: qq-main
    adapter: onebot
    ws_url: ws://127.0.0.1:3001

  - name: myim-main
    adapter: myim           # 第三方适配器：进程内注册，或下方 adapters 段声明的外部适配器
    api_base: https://im.example.com
    listen_addr: 127.0.0.1:19081   # 保留键，要求该适配器声明 net_listen

plugins:
  echo:
    enabled: true

# 可选：适配器声明。声明 grpc_addr 即视为外部适配器；只写 enabled 可禁用进程内适配器。
adapters:
  feishu:                          # 示例：禁用内置适配器（进程内声明只允许 enabled 键）
    enabled: false
  myim:
    enabled: true                  # 缺省 true；false 表示不加载该适配器及其 bots[] 实例
    grpc_addr: 127.0.0.1:50071     # 外部适配器 AdapterService 监听地址
    token: change-me               # 适配器反向调用核心的令牌，必填
    timeout: 10s                   # 单次 RPC 超时与启动就绪等待上限
    permissions: [receive_event, network, net_listen]
    # 其余键作为进程级配置经 AdapterInitRequest.config_json 下发
```

要求：

1. 配置结构体放在 `internal/config`。
2. 支持环境变量覆盖。
3. 支持插件独立配置；适配器配置分两层：实例级（`bots[]` 条目上的私有键）与进程级（`adapters.<name>`，仅外部适配器）。
4. 配置加载失败必须返回明确错误。
5. `adapters.<name>.enabled` 控制适配器是否启用，缺省为 **true**（与插件相反：适配器不需要声明即可用，声明只用于启用/禁用或接入外部进程）。环境变量 `KEI_ADAPTERS_<NAME>_ENABLED` 可覆盖。
6. `enabled: false` 的适配器不建立任何通道、其 `bots[]` 条目一并跳过（每个跳过的 bot 记 warn 日志），也不做通道字段与权限校验；核心仍可只运行插件。
7. `bots[].adapter` 必须能解析为「已注册的进程内适配器」或「声明了 `grpc_addr` 的外部适配器」，否则启动失败，并列出所有已注册适配器名；被禁用的适配器跳过该检查。
8. 声明了 `grpc_addr` 即走外部 gRPC 通道：此时要求 `grpc.addr` 非空、`token` 非空，否则启动失败（与外部插件规则一致）。
9. 没有 `grpc_addr` 的声明只允许 `enabled` 键：实例配置写在 `bots[]` 条目上，通道配置写 `grpc_addr`，其余键一律报错，避免静默失效的配置。
10. 存在启用的外部适配器时 `grpc.addr` 必须是固定的 `host:port`（不能是 `:0`）：适配器进程用它反向调用核心，临时端口无法预先告知。
11. `permissions` 缺省为 `[receive_event]`；实际授予 = 适配器声明的权限 ∩ 此处声明的权限，未授予权限对应的核心 API 一律拒绝。
12. 保留键 `listen_addr` 只允许出现在声明了 `net_listen` 的适配器实例上，否则启动失败。
13. 适配器不认识的私有键必须告警（不报错），以支持第三方适配器独立演进；进程内适配器的键集来自 `AdapterMetadata.Options`，外部适配器来自 `AdapterInitResponse.info.options`。
14. 外部适配器与外部插件共用 `grpc` 段的 TLS/mTLS 配置（`cert_file`/`key_file`/`ca_file`）。

---

## 14. 实现阶段与任务清单

请按以下顺序实现。每个阶段完成后必须能通过测试。

### 阶段 1：项目初始化与核心接口

- [ ] 创建 `go.mod`，模块路径 `github.com/RandomLemon/kei`。
- [ ] 创建目录结构。
- [ ] 在 `pkg/bot` 定义 Event、Message、Segment、User、Channel、Command。
- [ ] 在 `pkg/bot` 定义 Adapter、Plugin、Registrar、Reply、BotAPI、Storage。
- [ ] 在 `pkg/bot` 定义 Adapter 注册表：`AdapterFactory`、`AdapterContext`、`AdapterMetadata`、`RegisterAdapter`、`RegisteredAdapters`、`LookupAdapter`（与插件注册表 API 对称）。
- [ ] 编写接口单元测试或编译测试。
- [ ] 验收：`go build ./...` 通过。

### 阶段 2：核心引擎与事件总线

- [ ] 实现 `internal/eventbus`，支持 Publish/Subscribe、worker pool、去重、保序。
- [ ] 实现 `internal/router`，支持命令、正则、关键词、事件类型匹配。
- [ ] 实现 `internal/middleware`，至少包含 Recover、Logger、Timeout、Dedup。
- [ ] 实现 `internal/engine`，串联 Adapter、EventBus、Router、Plugin；引擎只接收 `AdapterBinding`，不感知平台名。
- [ ] 编写单元测试。
- [ ] 验收：`go test ./...` 通过。

### 阶段 3：Mock Adapter 与 Echo 插件

- [ ] 实现 `adapters/mock`，支持手动注入事件和捕获发送消息，并在 `init()` 中注册工厂。
- [ ] 实现 `plugins/echo`。
- [ ] 在 `cmd/bot/main.go` 中通过空导入 + `internal/adaptermgr` 装配 Mock Adapter 并启动 Engine，不得出现平台名分支。
- [ ] 编写集成测试：注入 `/echo hello`，断言回复 `hello`。
- [ ] 验收：`go test -race ./...` 通过。

### 阶段 4：飞书 Adapter 或 OneBot Adapter

- [ ] 选择飞书或 OneBot 之一实现 Adapter，并在包内 `init()` 中 `RegisterAdapter`。
- [ ] 飞书：实现事件订阅 HTTP 回调、challenge 校验、签名校验、快速 ACK、异步事件投递。
- [ ] OneBot：实现 WebSocket 或 HTTP 接入，解析事件，调用发送 API。
- [ ] 实现消息段与平台消息互转，如实声明 Capabilities 与 Permissions。
- [ ] 实现 Capabilities 降级（复用 `bot.Degrade`）。
- [ ] 编写适配器单元测试。
- [ ] 验收：可本地 Mock 平台请求，事件进入 Engine 并触发插件回复；新增平台未改动 `cmd/` 与 `internal/`。

### 阶段 5：配置与生命周期

- [ ] 实现 `internal/config`，加载 YAML，支持环境变量覆盖。
- [ ] 在 `cmd/bot/main.go` 中实现优雅启动和关闭。
- [ ] 支持多 bot、多 adapter 配置；`bots[].adapter` 经注册表解析。
- [ ] 支持插件启用/禁用。
- [ ] 支持 `adapters.<name>.enabled` 启用/禁用适配器（缺省启用，禁用则跳过其 bot）；支持 `adapters:` 段声明外部适配器；未知适配器名、重复注册名、缺 `Name`/`Platforms` 必须启动报错。
- [ ] 验收：`go run ./cmd/bot -config configs/config.yaml` 可启动并优雅退出。

### 阶段 6：外部插件 gRPC

- [ ] 编写 `proto/plugin.proto`，生成 Go 代码。
- [ ] 实现核心 gRPC Server（`internal/grpcsrv`），提供 BotService。
- [ ] 实现外部插件加载器。
- [ ] 提供 Python 或 Go 外部插件示例。
- [ ] 支持 token/mTLS。
- [ ] 验收：外部插件可连接核心、接收事件、发送回复。

### 阶段 7：可观测性与限流

- [ ] 实现 Prometheus 指标。
- [ ] 实现日志规范，使用 `log/slog`。
- [ ] 实现令牌桶限流。
- [ ] 实现消息去重 TTL 缓存。
- [ ] 实现管理命令，如 `/ping`、`/version`、`/plugins`、`/adapters`。
- [ ] 验收：指标可暴露，限流可测试，`/adapters` 能列出注册表与绑定关系。

### 阶段 8：第三方适配器支持（外部 gRPC 适配器）

- [ ] 编写 `proto/adapter.proto`，生成 Go 代码；`BotService` 增加 `EmitEvent`。
- [ ] 实现 `internal/adaptermgr`：注册表查表、`AdapterContext` 装配、按 `Permissions` 裁剪 Storage/HTTPClient、保留键校验。
- [ ] 实现核心侧外部适配器通道：dial `AdapterService`，调用 `Init`/`Start`/`Stop`/`Send`，并把 `EmitEvent` 接入事件总线（token + `receive_event` 校验）。
- [ ] 处理生命周期与容错：一进程多实例、优雅 `Shutdown`、断连退避重连、达上限只停用该 bot。
- [ ] 提供 Go 外部适配器示例 `cmd/example-adapter`，并给出注册表版第三方适配器示例（独立 module 布局见第 4 章）。
- [ ] 编写测试（bufconn）：注册表装配、权限裁剪、保留键校验、`EmitEvent` 入队、`Send` 出站、token/权限不足被拒、适配器崩溃不影响核心。
- [ ] 更新 `README.md`：适配器注册表用法、第三方适配器接入步骤（独立 module + 空导入 + 配置）、`adapters:` 段与外部适配器示例。
- [ ] 验收：示例外部适配器可被核心 dial、投递事件并被插件回复；`go test ./...`、`go test -race ./...`、`go vet ./...` 全绿。

---

## 15. 代码约定

1. 包名小写，简短，不使用下划线。
2. 公开接口必须有注释。
3. 所有错误必须返回，不得 panic，除非是启动阶段不可恢复错误。
4. 所有 goroutine 必须有退出机制，避免泄漏。
5. 所有 map 并发访问必须加锁或使用 sync.Map。
6. 所有时间使用 `time.Time`，时区统一 UTC。
7. 所有 ID 使用字符串，不假设是数字。
8. 日志使用 `log/slog`，字段化输出。
9. 测试使用标准库 `testing`，必要时使用 `testify`。
10. 禁止在 `pkg/bot` 中引入 `internal/` 包。
11. 适配器与插件一样只依赖 `pkg/bot`，不得依赖 `internal/`；平台专属代码只允许出现在 `adapters/` 或第三方适配器包内。
12. 适配器/插件的实例状态挂在实例上，禁止包级可变状态；注册表与装配必须并发安全。

---

## 16. 测试要求

1. 每个核心包必须有单元测试。
2. 必须包含集成测试：Mock Adapter -> EventBus -> Router -> Echo 插件 -> Reply。
3. 必须运行 `go test -race ./...`。
4. 必须测试：
   - 命令触发
   - 正则触发
   - 关键词触发
   - 中间件顺序
   - panic recover
   - 超时
   - 去重
   - 同一会话保序
   - 优雅关闭
   - 适配器注册表：未知名字报错并列出已注册名、重复注册名报错、缺 `Name`/`Platforms` 报错
   - 工厂多实例隔离：同平台两个 bot 的实例状态互不影响
   - 权限裁剪：未声明 `storage`/`network` 时注入拒绝式 Storage / nil HTTPClient，未声明 `net_listen` 时 `listen_addr` 导致启动失败
   - 适配器启用开关：`enabled: false` 时不建立外部通道（地址不可达也不 dial）、其 bot 被跳过、其他 bot 不受影响；禁用条目不参与校验
5. 适配器测试使用 `httptest` 或 Mock WebSocket；Capabilities 降级用表驱动测试覆盖 6.4 的每条规则。
6. gRPC 插件测试使用 bufconn。
7. 外部适配器用 bufconn 测试：`Init`/`Start`/`Stop`/`Send`、`EmitEvent` 入队、token 错误被拒、缺 `receive_event` 被拒、适配器崩溃后核心继续服务其他 bot。
8. 第三方适配器接入必须有一个「不改核心代码」的验收测试：只空导入示例适配器包 + 配置 `bots[].adapter` 即可跑通事件到回复。

---

## 17. 构建与运行命令

```bash
# 格式化
gofmt -w .

# 构建
go build ./...

# 测试
go test ./...

# 竞态测试
go test -race ./...

# 静态检查
go vet ./...

# 运行
go run ./cmd/bot -config configs/config.yaml
```

如果使用 `golangci-lint`：

```bash
golangci-lint run ./...
```

---

## 18. 给实现 Agent 的注意事项

1. 优先实现 MVP：Mock Adapter + Echo 插件 + Engine 跑通，再扩展平台。
2. 不要过度设计。接口稳定比功能多更重要。
3. 不要使用 Go `plugin` 作为主要插件机制。
4. 保持 `pkg/bot` 简洁、稳定、无内部依赖。
5. 内置平台相关代码必须放在 `adapters/`；第三方适配器放独立包或独立 module，核心仓库不得为其改动任何文件。
6. 所有插件相关代码必须放在 `plugins/`。
7. 所有核心逻辑必须放在 `internal/`。
8. 每完成一个阶段，运行测试并确保通过。
9. 遇到设计歧义时，优先遵循本文档；本文档未覆盖的，选择简单可测试的方案。
10. 最终交付必须包含：可运行代码、测试、示例配置、README 或使用说明。
11. 适配器与插件必须共用同一套注册/配置/权限模型；任何平台专属分支出现在 `cmd/` 或 `internal/` 都视为设计错误。
12. 新增平台时先问：只加一个新包（或新 module）+ 一段配置能否跑通？不能就说明注册表设计被绕过，先修设计再加平台。

---

## 19. 完成定义（Definition of Done）

一个功能被认为完成，必须满足：

- 代码已实现并合并到主分支。
- 有单元测试或集成测试覆盖。
- `go build ./...` 通过。
- `go test ./...` 通过。
- `go test -race ./...` 通过。
- `go vet ./...` 通过。
- 公开接口有注释。
- 示例配置可运行。
- 文档或注释说明如何使用。
- 不引入未声明的外部依赖。
- 不破坏 `pkg/bot` 的向后兼容性。
- 新适配器可在不修改核心代码的前提下接入：进程内注册（独立包/独立 module）或外部 gRPC 进程，并有测试覆盖。

---

## 20. 最终交付物

1. 完整 Go 项目源码。
2. `AGENTS.md` 本文件。
3. `README.md` 使用说明。
4. `configs/config.yaml` 示例配置。
5. `plugins/echo` 示例插件。
6. `adapters/mock` Mock 适配器（经注册表接入）。
7. 至少一个真实平台适配器（飞书或 OneBot），经注册表接入。
8. 第三方适配器示例：只依赖 `pkg/bot` 的独立 module 注册示例 + `cmd/example-adapter` 外部 gRPC 适配器示例。
9. 测试用例。
10. `proto/plugin.proto`、`proto/adapter.proto` 与外部插件/适配器示例（非 Go 语言实现可选）。
