package bot

import "context"

// Adapter 是平台适配层接口。
//
// 一个 Adapter 实例对应配置文件中的一个 bot（例如一个飞书应用或一个
// OneBot 实现）。实现方负责签名校验、协议解析、事件转换与消息发送，
// 并把平台调用统一为 Send/SendRequest。
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

// TargetFromEvent 由事件推导出回复目标，插件可据此主动发送消息。
func TargetFromEvent(ev *Event) Target {
	if ev == nil {
		return Target{}
	}
	t := Target{Platform: ev.Platform, BotID: ev.BotID, Kind: MessageKind("")}
	if ev.Message != nil {
		t.Kind = ev.Message.Kind
	}
	if ev.Channel != nil {
		t.ChannelID = ev.Channel.ID
		if t.Kind == "" {
			t.Kind = ev.Channel.Kind
		}
	}
	if ev.Sender != nil {
		t.UserID = ev.Sender.ID
	}
	return t
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
