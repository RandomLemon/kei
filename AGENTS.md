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
5. 核心引擎稳定，平台适配器可插拔，插件可进程内加载，也可扩展到外部 gRPC 插件。

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
- 外部插件协议使用 gRPC：`google.golang.org/grpc`、`google.golang.org/protobuf`。
- 禁止使用 Go `plugin` 作为主要插件机制。
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
3. 适配器可插拔：新增平台只需实现 Adapter。
4. 消息段统一：文本、图片、At、Markdown、卡片等统一为 Segment。
5. 动态扩展优先用外部进程/gRPC，而不是 Go `plugin`。

---

## 4. 目录结构

请按以下结构创建项目。允许根据实际情况微调，但 `pkg/bot`、`internal/`、`adapters/`、`plugins/` 的职责必须保持清晰。

```text
chatbot/
├── AGENTS.md
├── go.mod
├── go.sum
├── cmd/
│   └── bot/
│       └── main.go
├── pkg/
│   ├── bot/              # 公开 SDK：Event、Message、Plugin、Registrar、Reply
│   └── message/          # 消息段、构建器
├── internal/
│   ├── engine/           # 核心引擎
│   ├── router/           # 路由匹配
│   ├── middleware/       # 中间件
│   ├── eventbus/         # 事件总线
│   └── pluginmgr/        # 插件管理
├── adapters/
│   ├── feishu/
│   ├── onebot/
│   ├── wecom/
│   └── mock/
├── plugins/
│   ├── echo/
│   └── weather/
├── proto/
│   └── plugin.proto
└── configs/
    └── config.yaml
```

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

在 `pkg/bot` 中定义 Adapter 接口。

```go
type Adapter interface {
	Name() string
	Start(ctx context.Context, sink EventSink) error
	Stop(ctx context.Context) error
	Send(ctx context.Context, req *SendRequest) (*SendResult, error)
	Capabilities() Capabilities
}

type EventSink interface {
	Emit(ctx context.Context, ev *Event) error
}

type SendRequest struct {
	BotID   string
	Target  Target
	Message *Message
	ReplyTo string
}

type Target struct {
	Platform  string
	ChannelID string
	UserID    string
	Kind      MessageKind
}

type SendResult struct {
	MessageID string
	Raw       any
}

type Capabilities struct {
	Text     bool
	Markdown bool
	Image    bool
	At       bool
	Card     bool
	Reply    bool
	Private  bool
	Group    bool
}
```

实现要求：

1. 每个平台在 `adapters/<platform>` 下实现 Adapter。
2. Adapter 负责签名校验、协议解析、事件转换为统一 `Event`。
3. Adapter 负责把统一 `Message` 转为平台消息并发送。
4. Adapter 必须声明 `Capabilities`。
5. 飞书回调必须快速 ACK，业务事件异步推入 EventBus。
6. Mock Adapter 必须实现，用于本地测试和事件回放。

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

1. 启动顺序：加载配置 -> 初始化日志/指标/存储 -> 注册编译期插件 -> 连接外部插件 -> 初始化 Adapter -> 启动 EventBus worker -> 启动插件 -> 启动 Adapter。
2. 关闭顺序：停止接收 -> 等待处理完成 -> 停止 Adapter -> 停止插件。
3. 支持优雅退出。
4. 每个 Adapter 独立 goroutine 接收事件。
5. EventBus 使用 worker pool。
6. 同一会话按 `platform:botID:channelID:userID` 哈希保序。
7. 插件 Handler 必须支持 `context` 超时和 panic recover。
8. 发送消息要处理平台限流，可加重试和令牌桶。

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

const (
	PermSendMessage Permission = "send_message"
	PermReadUser    Permission = "read_user"
	PermNetwork     Permission = "network"
	PermStorage     Permission = "storage"
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

	"github.com/yourorg/chatbot/pkg/bot"
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
5. 提供天气插件示例，展示多轮对话或外部 HTTP 调用。

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

## 12. 外部插件 gRPC 协议

在 `proto/plugin.proto` 中定义。

```proto
syntax = "proto3";

package plugin;

service PluginService {
  rpc Init(InitRequest) returns (InitResponse);
  rpc HandleEvent(Event) returns (HandleResult);
  rpc Shutdown(Empty) returns (Empty);
}

service BotService {
  rpc SendMessage(SendRequest) returns (SendResponse);
  rpc GetConfig(GetConfigRequest) returns (GetConfigResponse);
  rpc Log(LogRequest) returns (Empty);
}
```

要求：

1. 核心作为 gRPC Server，外部插件启动后连接并注册。
2. 插件处理事件时，通过 `BotService.SendMessage` 回复。
3. 支持 token/mTLS 权限控制。
4. 插件崩溃不影响主进程。
5. 外部插件可用 Python、Node.js、Rust 编写。
6. MVP 可先实现进程内插件，gRPC 作为第二阶段。

---

## 13. 配置

在 `configs/config.yaml` 提供示例。

```yaml
bots:
  - name: feishu-main
    adapter: feishu
    app_id: xxx
    app_secret: xxx

  - name: qq-main
    adapter: onebot
    ws_url: ws://127.0.0.1:3001

plugins:
  weather:
    enabled: true
    api_key: xxx
```

要求：

1. 配置结构体放在 `internal/config`。
2. 支持环境变量覆盖。
3. 支持插件独立配置。
4. 配置加载失败必须返回明确错误。

---

## 14. 实现阶段与任务清单

请按以下顺序实现。每个阶段完成后必须能通过测试。

### 阶段 1：项目初始化与核心接口

- [ ] 创建 `go.mod`，模块路径 `github.com/yourorg/chatbot`。
- [ ] 创建目录结构。
- [ ] 在 `pkg/bot` 定义 Event、Message、Segment、User、Channel、Command。
- [ ] 在 `pkg/bot` 定义 Adapter、Plugin、Registrar、Reply、BotAPI、Storage。
- [ ] 编写接口单元测试或编译测试。
- [ ] 验收：`go build ./...` 通过。

### 阶段 2：核心引擎与事件总线

- [ ] 实现 `internal/eventbus`，支持 Publish/Subscribe、worker pool、去重、保序。
- [ ] 实现 `internal/router`，支持命令、正则、关键词、事件类型匹配。
- [ ] 实现 `internal/middleware`，至少包含 Recover、Logger、Timeout、Dedup。
- [ ] 实现 `internal/engine`，串联 Adapter、EventBus、Router、Plugin。
- [ ] 编写单元测试。
- [ ] 验收：`go test ./...` 通过。

### 阶段 3：Mock Adapter 与 Echo 插件

- [ ] 实现 `adapters/mock`，支持手动注入事件和捕获发送消息。
- [ ] 实现 `plugins/echo`。
- [ ] 在 `cmd/bot/main.go` 中启动 Engine、Mock Adapter、Echo 插件。
- [ ] 编写集成测试：注入 `/echo hello`，断言回复 `hello`。
- [ ] 验收：`go test -race ./...` 通过。

### 阶段 4：飞书 Adapter 或 OneBot Adapter

- [ ] 选择飞书或 OneBot 之一实现 Adapter。
- [ ] 飞书：实现事件订阅 HTTP 回调、challenge 校验、签名校验、快速 ACK、异步事件投递。
- [ ] OneBot：实现 WebSocket 或 HTTP 接入，解析事件，调用发送 API。
- [ ] 实现消息段与平台消息互转。
- [ ] 实现 Capabilities 降级。
- [ ] 编写适配器单元测试。
- [ ] 验收：可本地 Mock 平台请求，事件进入 Engine 并触发插件回复。

### 阶段 5：配置与生命周期

- [ ] 实现 `internal/config`，加载 YAML，支持环境变量覆盖。
- [ ] 在 `cmd/bot/main.go` 中实现优雅启动和关闭。
- [ ] 支持多 bot、多 adapter 配置。
- [ ] 支持插件启用/禁用。
- [ ] 验收：`go run ./cmd/bot -config configs/config.yaml` 可启动并优雅退出。

### 阶段 6：外部插件 gRPC

- [ ] 编写 `proto/plugin.proto`，生成 Go 代码。
- [ ] 实现核心 gRPC Server，提供 BotService。
- [ ] 实现外部插件加载器。
- [ ] 提供 Python 或 Go 外部插件示例。
- [ ] 支持 token/mTLS。
- [ ] 验收：外部插件可连接核心、接收事件、发送回复。

### 阶段 7：可观测性与限流

- [ ] 实现 Prometheus 指标。
- [ ] 实现日志规范，使用 `log/slog`。
- [ ] 实现令牌桶限流。
- [ ] 实现消息去重 TTL 缓存。
- [ ] 实现管理命令，如 `/ping`、`/version`、`/plugins`。
- [ ] 验收：指标可暴露，限流可测试。

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
5. 适配器测试使用 `httptest` 或 Mock WebSocket。
6. gRPC 插件测试使用 bufconn。

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
5. 所有平台相关代码必须放在 `adapters/`。
6. 所有插件相关代码必须放在 `plugins/`。
7. 所有核心逻辑必须放在 `internal/`。
8. 每完成一个阶段，运行测试并确保通过。
9. 遇到设计歧义时，优先遵循本文档；本文档未覆盖的，选择简单可测试的方案。
10. 最终交付必须包含：可运行代码、测试、示例配置、README 或使用说明。

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

---

## 20. 最终交付物

1. 完整 Go 项目源码。
2. `AGENTS.md` 本文件。
3. `README.md` 使用说明。
4. `configs/config.yaml` 示例配置。
5. `plugins/echo` 示例插件。
6. `adapters/mock` Mock 适配器。
7. 至少一个真实平台适配器（飞书或 OneBot）。
8. 测试用例。
9. 可选：`proto/plugin.proto` 与外部插件示例。
