package bot

import (
	"strings"
	"time"
)

// EventType 是事件的分类，适配器负责把平台事件映射到这些类型。
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

// Event 是平台无关的统一事件，是适配层与业务层之间唯一的传输单元。
//
// 除 Raw 之外的所有字段都必须由适配器填充；Raw 仅用于调试与排查，
// 业务代码不应依赖其结构。
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

// SessionKey 返回会话归属键，格式为 platform:botID:channelID:userID。
//
// 事件总线按该键哈希保序：同一会话的事件严格串行处理，不同会话可并行。
// 私聊场景 Channel 为空，此时以发送者 ID 作为会话标识。
func (e *Event) SessionKey() string {
	platform, botID := e.Platform, e.BotID
	channelID, userID := "", ""
	if e.Channel != nil {
		channelID = e.Channel.ID
	}
	if e.Sender != nil {
		userID = e.Sender.ID
	}
	if channelID == "" {
		channelID = userID
	}
	return platform + ":" + botID + ":" + channelID + ":" + userID
}

// Text 返回消息中所有文本段拼接后的内容，并按顺序拼接 Markdown 段。
func (e *Event) Text() string {
	if e == nil || e.Message == nil {
		return ""
	}
	return e.Message.PlainText()
}

// Command 是解析后的命令。
type Command struct {
	// Name 是命令名，不含前缀，例如 "echo"。
	Name string
	// Args 是按空白切分后的参数。
	Args []string
	// Raw 是命令后的原始参数字符串，保留原始空白。
	Raw string
}

// Text 返回命令与参数重新拼接后的文本。
func (c *Command) Text() string {
	if c == nil {
		return ""
	}
	if c.Raw == "" {
		return c.Name
	}
	return c.Name + " " + c.Raw
}

// ParseCommand 从文本中解析命令。prefixes 是允许的命令前缀，例如 "/" 与 "!"。
//
// 解析规则：去掉首尾空白后，若文本以某个前缀开头，则前缀之后的第一个
// 空白分隔词为命令名，其余部分为参数（保持原始空白）。前缀之后为空则返回 nil。
// 未命中任何前缀时返回 nil，调用方可按关键词或正则规则处理。
func ParseCommand(text string, prefixes []string) *Command {
	s := strings.TrimSpace(text)
	if s == "" {
		return nil
	}
	var body string
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if strings.HasPrefix(s, p) {
			body = s[len(p):]
			break
		}
	}
	if body == "" {
		return nil
	}
	body = strings.TrimLeft(body, " \t")
	if body == "" {
		return nil
	}
	name, rest := body, ""
	if i := strings.IndexAny(body, " \t"); i >= 0 {
		name, rest = body[:i], strings.TrimLeft(body[i+1:], " \t")
	}
	if name == "" {
		return nil
	}
	return &Command{
		Name: name,
		Args: strings.Fields(rest),
		Raw:  rest,
	}
}
