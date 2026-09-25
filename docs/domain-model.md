# kei 领域模型

## 5. 核心领域模型

覆盖 `pkg/bot` 中平台无关的领域类型、枚举常量与方法，以及 `pkg/message` 的消息段构建器；同时给出 `Segment.Data` 的键名契约。

不覆盖：消息段降级规则与 `Capabilities` 映射（见 adapter.md#64-能力与降级）、事件总线与路由行为（见 engine.md）、插件运行期接口与 `Reply` 构建器（见 plugin.md）、gRPC 消息映射（见 grpc.md）。

### 5.1 稳定性契约

`pkg/bot` 是 kei chatbot 框架的公开 SDK。它只包含平台无关的领域模型与接口：事件、消息段、适配器、插件、路由注册器、回复构建器与 BotAPI。插件只允许依赖本包，不得依赖 `internal/` 下的任何实现，从而保证插件与平台、与内核实现解耦。

本包的公开类型与接口被视为稳定契约，修改必须保持向后兼容。以下类型均为公开 SDK 的组成部分，已发布字段不得删除、改名或改变语义；新增字段必须保持零值语义（零值表示「未设置」）。

### 5.2 事件

`EventType` 是事件的分类，适配器负责把平台事件映射到这些类型。

```go
type EventType string

// 支持的事件类型。
const (
	// EventMessage 表示用户或群聊产生的消息事件。
	EventMessage EventType = "message"
	// EventNotice 表示通知类事件，例如入群、撤回、戳一戳。
	EventNotice EventType = "notice"
	// EventRequest 表示请求类事件，例如加好友、加群邀请。
	EventRequest EventType = "request"
	// EventMeta 表示元事件，例如心跳、生命周期。
	EventMeta EventType = "meta"
)
```

`Event` 是平台无关的统一事件，是适配层与业务层之间唯一的传输单元。除 `Raw` 之外的所有字段都必须由适配器填充；`Raw` 仅用于调试与排查，业务代码不应依赖其结构。

```go
type Event struct {
	// ID 是事件唯一标识，用于去重；没有平台 ID 时适配器需自行生成。
	ID string
	// Type 是事件分类。
	Type EventType
	// Platform 是产生事件的平台名，与 Adapter.Name() 一致。
	Platform string
	// BotID 是接收该事件的机器人标识，多机器人部署时用于区分。
	BotID string
	// Time 是事件发生时间，统一使用 UTC。
	Time time.Time

	// Message 仅在 Type 为 EventMessage 时非空。
	Message *Message
	// Sender 是消息发送者。
	Sender *User
	// Channel 是消息所在会话，私聊时可能为空。
	Channel *Channel
	// Command 是解析后的命令，未触发命令时为 nil。
	Command *Command

	// Raw 是平台原始事件，仅用于调试。
	Raw any
}
```

`Event` 的方法：

```go
// SessionKey 返回会话归属键，格式为 platform:botID:channelID:userID。
//
// 事件总线按该键哈希保序：同一会话的事件严格串行处理，不同会话可并行。
// 私聊场景 Channel 为空，此时以发送者 ID 作为会话标识。
func (e *Event) SessionKey() string

// Text 返回消息中所有文本段拼接后的内容，并按顺序拼接 Markdown 段。
func (e *Event) Text() string
```

`Command` 是解析后的命令。

```go
type Command struct {
	// Name 是命令名，不含前缀，例如 "echo"。
	Name string
	// Args 是按空白切分后的参数。
	Args []string
	// Raw 是命令后的原始参数字符串，保留原始空白。
	Raw string
}

// Text 返回命令与参数重新拼接后的文本。
func (c *Command) Text() string

// ParseCommand 从文本中解析命令。prefixes 是允许的命令前缀，例如 "/" 与 "!"。
//
// 解析规则：去掉首尾空白后，若文本以某个前缀开头，则前缀之后的第一个
// 空白分隔词为命令名，其余部分为参数（保持原始空白）。前缀之后为空则返回 nil。
// 未命中任何前缀时返回 nil，调用方可按关键词或正则规则处理。
func ParseCommand(text string, prefixes []string) *Command
```

源码补充（公开契约）：`(*Event).SessionKey`、`(*Event).Text`、`(*Command).Text`、`ParseCommand` 四个方法/函数均为公开契约。

### 5.3 消息与会话类型

`MessageKind` 描述消息所在会话的类型。

```go
type MessageKind string

// 支持的会话类型。
const (
	// MessagePrivate 是私聊消息。
	MessagePrivate MessageKind = "private"
	// MessageGroup 是群聊消息。
	MessageGroup MessageKind = "group"
	// MessageChannel 是频道消息，例如飞书的会话频道。
	MessageChannel MessageKind = "channel"
)
```

`Message` 是平台无关的消息，由若干消息段组成。

```go
type Message struct {
	// ID 是消息 ID，发送前可为空。
	ID string
	// Kind 是消息所在会话类型。
	Kind MessageKind
	// Segments 是按顺序排列的消息段。
	Segments []Segment
}

// PlainText 返回消息中所有文本段的拼接结果。
//
// Markdown 段按纯文本对待；其它类型忽略。用于路由的命令与关键词匹配。
func (m *Message) PlainText() string
```

源码补充（公开契约）：`(*Message).PlainText` 为公开契约；`PlainText` 只读取 `SegText` 与 `SegMarkdown` 两类的 `KeyText` 值。

### 5.4 消息段

`SegmentType` 是消息段类型。

```go
type SegmentType string

// 支持的消息段类型。
const (
	// SegText 是纯文本段，数据键为 KeyText。
	SegText SegmentType = "text"
	// SegImage 是图片段，数据键为 KeyURL 或 KeyFile。
	SegImage SegmentType = "image"
	// SegAt 是 @ 段，数据键为 KeyUserID，可选 KeyUserName。
	SegAt SegmentType = "at"
	// SegFace 是表情段，数据键为 KeyFaceID。
	SegFace SegmentType = "face"
	// SegReply 是引用回复段，数据键为 KeyMessageID。
	SegReply SegmentType = "reply"
	// SegMarkdown 是 Markdown 段，数据键为 KeyText。
	SegMarkdown SegmentType = "markdown"
	// SegCard 是卡片段，数据键为 KeyCard。
	SegCard SegmentType = "card"
	// SegFile 是文件段，数据键为 KeyURL 或 KeyFile，可选 KeyFileName。
	SegFile SegmentType = "file"
)
```

`Segment` 是消息的一个片段。

```go
type Segment struct {
	// Type 是片段类型。
	Type SegmentType
	// Data 是片段数据，键名使用本包的 Key* 常量。
	Data map[string]any
}
```

### 5.5 用户与会话

```go
// User 是消息发送者或 @ 的目标用户。
type User struct {
	// ID 是平台内的用户唯一标识。
	ID string
	// Name 是用户显示名。
	Name string
	// IsBot 表示该用户是否为机器人。
	IsBot bool
}

// Channel 是消息所在的会话（群、频道、私聊会话）。
type Channel struct {
	// ID 是平台内的会话唯一标识。
	ID string
	// Name 是会话显示名。
	Name string
	// Kind 是会话类型。
	Kind MessageKind
}
```

### 5.6 Segment.Data 键约定

`Segment.Data` 中约定的键名。适配器与插件都必须使用这些常量而不是字面量，避免跨包字段漂移。它们是无类型字符串常量，可直接用作 `map[string]any` 的键。

```go
const (
	// KeyText 是文本内容，用于 SegText 与 SegMarkdown。
	KeyText = "text"
	// KeyURL 是可下载地址，用于 SegImage、SegFile。
	KeyURL = "url"
	// KeyFile 是本地路径或平台文件标识，用于 SegImage、SegFile。
	KeyFile = "file"
	// KeyFileName 是文件名，用于 SegFile。
	KeyFileName = "file_name"
	// KeyUserID 是用户 ID，用于 SegAt。
	KeyUserID = "user_id"
	// KeyUserName 是用户显示名，用于 SegAt。
	KeyUserName = "name"
	// KeyFaceID 是表情 ID，用于 SegFace。
	KeyFaceID = "id"
	// KeyMessageID 是被引用的消息 ID，用于 SegReply。
	KeyMessageID = "message_id"
	// KeyCard 是卡片载荷，用于 SegCard，取值由平台决定。
	KeyCard = "card"
)
```

| 常量 | 字面值 | 适用 SegmentType | 必需性 | 取值类型 |
| --- | --- | --- | --- | --- |
| `KeyText` | `"text"` | `SegText`、`SegMarkdown` | 必需 | `string` |
| `KeyURL` | `"url"` | `SegImage`、`SegFile` | 与 `KeyFile` 至少一个非空 | `string`（可下载地址） |
| `KeyFile` | `"file"` | `SegImage`、`SegFile` | 与 `KeyURL` 至少一个非空 | `string`（本地路径或平台文件标识） |
| `KeyFileName` | `"file_name"` | `SegFile` | 可选 | `string` |
| `KeyUserID` | `"user_id"` | `SegAt` | 必需 | `string` |
| `KeyUserName` | `"name"` | `SegAt` | 可选 | `string` |
| `KeyFaceID` | `"id"` | `SegFace` | 必需 | `string` |
| `KeyMessageID` | `"message_id"` | `SegReply` | 必需 | `string` |
| `KeyCard` | `"card"` | `SegCard` | 必需 | 任意（`any`），载荷结构由平台决定 |

约定：

1. 除 `KeyCard` 外，各键的取值均为 `string`；读取方按 `seg.Data[bot.KeyX].(string)` 类型断言，类型不符时按缺失处理（`bot.Message.PlainText` 即如此）。
2. 适配器在读取平台消息构造段时，值为空的键应省略而不是写入空串（例如飞书、OneBot 适配器对 `KeyUserName`、`KeyFileName` 的处理）。
3. 适配器在发送时对 `SegImage`、`SegFile` 同时尝试 `KeyFile` 与 `KeyURL`，两者都为空视为不可发送。
4. 未知的 `SegmentType` 或未知 `Data` 键不影响类型契约：消费方必须忽略而不报错。

源码补充（公开契约）：第九个键 `KeyCard = "card"`，以及全部九个 `Key*` 常量本身。

### 5.7 消息段构造与读取辅助

`pkg/message` 提供消息与消息段的构建器。插件通过本包构造 `bot.Message`，再交给 `bot.BotAPI` 或 `bot.Reply` 发送，从而避免在各处手写 map 字面量。

```go
// Text 构造纯文本段。
func Text(s string) bot.Segment

// Markdown 构造 Markdown 段，平台不支持时会降级为纯文本。
func Markdown(s string) bot.Segment

// Image 通过 URL 构造图片段。
func Image(url string) bot.Segment

// ImageFile 通过本地文件路径或平台文件标识构造图片段。
func ImageFile(file string) bot.Segment

// At 构造 @ 段。
func At(userID string) bot.Segment

// AtName 构造带显示名的 @ 段，平台不支持时会降级为 "@名字"。
func AtName(userID, name string) bot.Segment

// Face 构造表情段。
func Face(id string) bot.Segment

// Reply 构造引用段。
func Reply(messageID string) bot.Segment

// Card 构造卡片段。
func Card(card any) bot.Segment

// File 构造文件段。
func File(url, name string) bot.Segment

// New 按会话类型构造消息。
func New(kind bot.MessageKind, segs ...bot.Segment) *bot.Message

// Group 构造群聊消息。
func Group(segs ...bot.Segment) *bot.Message

// Private 构造私聊消息。
func Private(segs ...bot.Segment) *bot.Message

// Channel 构造频道消息。
func Channel(segs ...bot.Segment) *bot.Message

// Plain 用会话类型与文本构造单段消息。
func Plain(kind bot.MessageKind, text string) *bot.Message
```

要点：

- `New` 会复制 `segs` 切片，调用方之后修改入参切片不会影响已构造的消息；`Plain` 等价于 `New(kind, Text(text))`。
- 构建器只做「常量 + 键」的装配，不校验平台能力；是否可发送由引擎按 `Capabilities` 降级后决定（见 adapter.md#64-能力与降级）。
- 读取辅助：`(*bot.Message).PlainText` 与 `(*bot.Event).Text` 读取文本段内容，`(*bot.Command).Text` 还原命令文本，`(*bot.Event).SessionKey` 生成会话归属键。回复构建器（`bot.Reply`）的读写辅助见 plugin.md。

边界：消息段降级规则（Markdown、图片、文件、At、表情、卡片、引用、未知段类型的转换）不在本文档，见 adapter.md#64-能力与降级。

## 相关文档

- [架构与目录结构](architecture.md)
- [Adapter 平台适配层：能力与降级](adapter.md#64-能力与降级)
- [插件系统与 BotAPI](plugin.md)
- [外部插件与适配器 gRPC 协议](grpc.md)
