package onebot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// OneBot v11 的 post_type 取值。
const (
	postMessage = "message"
	postNotice  = "notice"
	postRequest = "request"
	postMeta    = "meta_event"
)

// onebotEvent 是上报事件的原始结构，只声明本适配器需要的字段。
//
// 所有 ID 使用 flexID，兼容 OneBot 实现把 ID 序列化成数字或字符串的两种做法。
type onebotEvent struct {
	// Time 是事件发生时间（Unix 秒）。
	Time int64 `json:"time"`
	// SelfID 是接收事件的机器人 ID。
	SelfID flexID `json:"self_id"`
	// PostType 是事件大类。
	PostType string `json:"post_type"`

	// MessageType 是消息会话类型：group / private。
	MessageType string `json:"message_type"`
	// MessageID 是消息 ID。
	MessageID flexID `json:"message_id"`
	// Message 是消息内容，可能是段数组或 CQ 码字符串。
	Message json.RawMessage `json:"message"`
	// SubType 是子类型，例如 notice 的 group_recall、meta_event 的 heartbeat。
	SubType string `json:"sub_type"`

	// NoticeType 是通知类型。
	NoticeType string `json:"notice_type"`
	// RequestType 是请求类型。
	RequestType string `json:"request_type"`
	// MetaEventType 是元事件类型。
	MetaEventType string `json:"meta_event_type"`

	// UserID 是事件发起者 ID。
	UserID flexID `json:"user_id"`
	// GroupID 是群 ID。
	GroupID flexID `json:"group_id"`

	// Sender 是消息发送者信息。
	Sender struct {
		UserID   flexID `json:"user_id"`
		Nickname string `json:"nickname"`
		Card     string `json:"card"`
	} `json:"sender"`
}

// flexID 是 OneBot 中 ID 字段的统一表示。
//
// OneBot 实现可能把 ID 序列化成数字或字符串，甚至对超长 ID 使用字符串以避免
// 精度丢失；flexID 把两种形式都归一为 string，满足项目"ID 统一 string"的约定。
type flexID string

// String 返回 ID 的字符串形式。
func (f flexID) String() string { return string(f) }

// UnmarshalJSON 接受数字、字符串与 null 三种 JSON 形式。
func (f *flexID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = ""
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*f = flexID(s)
		return nil
	}
	*f = flexID(trimmed)
	return nil
}

// convertEvent 把 OneBot 上报的原始 JSON 转换为统一事件。
//
// botID 是适配器配置的机器人名称，写入 Event.BotID；fallbackSelfID 在事件缺少
// self_id 时用作兜底。无法识别的 post_type 或结构错误的报文返回错误，由 HTTP 层回 400。
func convertEvent(raw []byte, botID, fallbackSelfID string, log *slog.Logger) (*bot.Event, error) {
	var p onebotEvent
	if err := decodeJSON(raw, &p); err != nil {
		return nil, fmt.Errorf("onebot: 解析事件失败: %w", err)
	}

	var eventType bot.EventType
	switch p.PostType {
	case postMessage:
		eventType = bot.EventMessage
	case postNotice:
		eventType = bot.EventNotice
	case postRequest:
		eventType = bot.EventRequest
	case postMeta:
		eventType = bot.EventMeta
	default:
		return nil, fmt.Errorf("onebot: 不支持的 post_type %q", p.PostType)
	}

	selfID := p.SelfID.String()
	if selfID == "" {
		selfID = fallbackSelfID
	}

	ev := &bot.Event{
		ID:       eventID(&p, selfID, eventType),
		Type:     eventType,
		Platform: platformName,
		BotID:    botID,
		Time:     time.Unix(p.Time, 0).UTC(),
		Raw:      json.RawMessage(raw),
	}
	ev.Sender = buildSender(&p)

	if eventType == bot.EventMessage {
		kind := messageKind(&p)
		msg, err := parseMessage(p.Message, kind, log)
		if err != nil {
			return nil, err
		}
		msg.ID = p.MessageID.String()
		ev.Message = msg
		if kind == bot.MessageGroup {
			ev.Channel = &bot.Channel{ID: p.GroupID.String(), Kind: bot.MessageGroup}
		}
	}
	return ev, nil
}

// messageKind 推断消息事件的会话类型。
func messageKind(p *onebotEvent) bot.MessageKind {
	switch p.MessageType {
	case "group":
		return bot.MessageGroup
	case "private":
		return bot.MessagePrivate
	default:
		// message_type 缺失时按是否带 group_id 推断。
		if p.GroupID.String() != "" {
			return bot.MessageGroup
		}
		return bot.MessagePrivate
	}
}

// buildSender 构造发送者信息；没有任何身份字段时返回 nil。
//
// OneBot 协议不提供发送者是否为机器人的信息，IsBot 恒为 false。
func buildSender(p *onebotEvent) *bot.User {
	id := firstNonEmpty(p.Sender.UserID.String(), p.UserID.String())
	name := firstNonEmpty(p.Sender.Card, p.Sender.Nickname)
	if id == "" && name == "" {
		return nil
	}
	return &bot.User{ID: id, Name: name}
}

// eventID 生成事件 ID。
//
// 消息事件优先使用平台 message_id；其它类型（以及缺失 message_id 的消息）用
// self_id、post_type、时间与子类型/会话 ID 确定性拼接，保证同一事件重复上报时
// 生成相同 ID，便于上游去重。
func eventID(p *onebotEvent, selfID string, eventType bot.EventType) string {
	if eventType == bot.EventMessage {
		if id := p.MessageID.String(); id != "" {
			return id
		}
	}

	var b strings.Builder
	b.WriteString(selfID)
	b.WriteByte('-')
	b.WriteString(p.PostType)
	b.WriteByte('-')
	b.WriteString(strconv.FormatInt(p.Time, 10))
	for _, part := range [...]string{
		p.NoticeType,
		p.RequestType,
		p.MetaEventType,
		p.SubType,
		p.GroupID.String(),
		p.UserID.String(),
	} {
		if part != "" {
			b.WriteByte('-')
			b.WriteString(part)
		}
	}
	return b.String()
}

// decodeJSON 解析 JSON，数字统一保留为 json.Number，并拒绝尾部多余内容。
func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	switch err := dec.Decode(&extra); err {
	case io.EOF:
		return nil
	case nil:
		return fmt.Errorf("JSON 尾部存在多余内容")
	default:
		return err
	}
}
