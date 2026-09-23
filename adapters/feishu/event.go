package feishu

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// 飞书事件相关常量。
const (
	// eventTypeMessagePrefix 是消息接收事件类型前缀，完整值为 "im.message.receive_v1"。
	eventTypeMessagePrefix = "im.message.receive"
	// mentionPrefix 是文本内容中的 @ 占位符前缀，形如 "@_user_1"。
	mentionPrefix = "@_user_"
	// chatTypeP2P 表示单聊会话。
	chatTypeP2P = "p2p"
	// chatTypeGroup 表示群聊会话。
	chatTypeGroup = "group"
	// messageTypeText 是文本消息类型。
	messageTypeText = "text"
	// messageTypePost 是富文本消息类型。
	messageTypePost = "post"
	// messageTypeImage 是图片消息类型。
	messageTypeImage = "image"
	// messageTypeFile 是文件消息类型。
	messageTypeFile = "file"
	// senderTypeApp 表示发送者为应用（机器人）。
	senderTypeApp = "app"
)

// envelope 是飞书事件回调的信封，兼容 schema 2.0 与 url_verification 请求。
type envelope struct {
	// Schema 是事件协议版本，例如 "2.0"。
	Schema string `json:"schema"`
	// Type 在 url_verification 请求中为 "url_verification"。
	Type string `json:"type"`
	// Token 在 url_verification 请求中携带 Verification Token。
	Token string `json:"token"`
	// Challenge 在 url_verification 请求中携带校验串。
	Challenge string `json:"challenge"`
	// Encrypt 是加密事件的 base64 密文。
	Encrypt string `json:"encrypt"`
	// Header 是事件头。
	Header eventHeader `json:"header"`
	// Event 是事件体。
	Event eventBody `json:"event"`
	// raw 是用于事件转换的原始 JSON（加密事件为解密后的明文），仅供调试。
	raw []byte
}

// eventHeader 是 schema 2.0 事件头。
type eventHeader struct {
	// EventID 是事件唯一标识，用于去重。
	EventID string `json:"event_id"`
	// EventType 是事件类型，例如 "im.message.receive_v1"。
	EventType string `json:"event_type"`
	// CreateTime 是事件创建时间，毫秒时间戳字符串。
	CreateTime string `json:"create_time"`
	// Token 是事件订阅的 Verification Token。
	Token string `json:"token"`
	// AppID 是事件所属应用。
	AppID string `json:"app_id"`
	// TenantKey 是租户标识。
	TenantKey string `json:"tenant_key"`
}

// eventBody 是事件体，字段随事件类型变化，这里覆盖消息事件用到的字段。
type eventBody struct {
	// Sender 是事件发送者。
	Sender eventSender `json:"sender"`
	// Message 是消息内容，仅消息事件存在。
	Message eventMessage `json:"message"`
}

// eventSender 是事件发送者。
type eventSender struct {
	// SenderID 是发送者标识集合。
	SenderID senderID `json:"sender_id"`
	// SenderType 是发送者类型，"app" 表示机器人。
	SenderType string `json:"sender_type"`
	// TenantKey 是发送者所属租户。
	TenantKey string `json:"tenant_key"`
	// Nickname 是发送者昵称；官方消息事件不返回，部分事件与自建扩展会带上。
	Nickname string `json:"nickname"`
}

// senderID 是发送者/提及用户的标识集合。
type senderID struct {
	// OpenID 是应用内用户标识。
	OpenID string `json:"open_id"`
	// UnionID 是开放平台用户标识。
	UnionID string `json:"union_id"`
	// UserID 是租户内用户标识。
	UserID string `json:"user_id"`
	// Nickname 是用户昵称；官方结构无此字段，作为兼容扩展读取。
	Nickname string `json:"nickname"`
}

// eventMessage 是消息事件的消息体。
type eventMessage struct {
	// MessageID 是消息 ID。
	MessageID string `json:"message_id"`
	// RootID 是话题根消息 ID。
	RootID string `json:"root_id"`
	// ParentID 是回复的父消息 ID。
	ParentID string `json:"parent_id"`
	// ChatID 是会话 ID。
	ChatID string `json:"chat_id"`
	// ChatType 是会话类型，"p2p" 或 "group"。
	ChatType string `json:"chat_type"`
	// MessageType 是消息类型，例如 "text"、"post"、"image"。
	MessageType string `json:"message_type"`
	// Content 是消息内容 JSON 字符串，结构随 MessageType 变化。
	Content string `json:"content"`
	// Mentions 是被 @ 的用户列表。
	Mentions []mention `json:"mentions"`
}

// mention 是被 @ 的用户。
type mention struct {
	// Key 是文本内容中的占位符，例如 "@_user_1"。
	Key string `json:"key"`
	// ID 是被 @ 用户的标识集合。
	ID senderID `json:"id"`
	// Name 是被 @ 用户显示名。
	Name string `json:"name"`
	// TenantKey 是被 @ 用户所属租户。
	TenantKey string `json:"tenant_key"`
}

// decodeEnvelope 解析回调 body：加密事件先解密，再反序列化为信封。
func (a *Adapter) decodeEnvelope(body []byte) (*envelope, error) {
	var outer envelope
	if err := json.Unmarshal(body, &outer); err != nil {
		return nil, fmt.Errorf("feishu: 回调 body 不是合法 JSON: %w", err)
	}
	if outer.Encrypt == "" {
		outer.raw = body
		return &outer, nil
	}
	plain, err := decryptEvent(a.opts.EncryptKey, outer.Encrypt)
	if err != nil {
		return nil, err
	}
	var inner envelope
	if err := json.Unmarshal(plain, &inner); err != nil {
		return nil, fmt.Errorf("feishu: 解密后的事件不是合法 JSON: %w", err)
	}
	inner.raw = plain
	return &inner, nil
}

// convert 把飞书信封转换为统一事件。
//
// 消息类事件映射为 EventMessage 并填充 Message/Sender/Channel；其余事件
// 一律映射为 EventNotice。命令解析由 router 负责，这里不做处理。
func (a *Adapter) convert(env *envelope) *bot.Event {
	ev := &bot.Event{
		ID:       env.Header.EventID,
		Type:     bot.EventNotice,
		Platform: PlatformName,
		BotID:    a.opts.Name,
		Time:     nowUTC(env.Header.CreateTime),
		Raw:      json.RawMessage(env.raw),
	}
	if ev.ID == "" {
		// 平台未给事件 ID 时生成兜底 ID，保证事件总线去重可用。
		ev.ID = a.fallbackID()
	}

	sender := &bot.User{
		ID:    firstNonEmpty(env.Event.Sender.SenderID.OpenID, env.Event.Sender.SenderID.UnionID, env.Event.Sender.SenderID.UserID),
		IsBot: env.Event.Sender.SenderType == senderTypeApp,
	}
	sender.Name = firstNonEmpty(env.Event.Sender.Nickname, env.Event.Sender.SenderID.Nickname, sender.ID)
	if sender.ID == "" && sender.Name == "" {
		sender = nil
	}
	ev.Sender = sender

	if msg := env.Event.Message; msg.MessageID != "" || msg.ChatID != "" {
		ev.Channel = &bot.Channel{
			ID:   msg.ChatID,
			Kind: kindOf(msg.ChatType),
		}
	}

	if !strings.HasPrefix(env.Header.EventType, eventTypeMessagePrefix) {
		return ev
	}

	msg := env.Event.Message
	ev.Type = bot.EventMessage
	ev.Message = &bot.Message{
		ID:       msg.MessageID,
		Kind:     kindOf(msg.ChatType),
		Segments: parseContent(msg.MessageType, msg.Content, msg.Mentions),
	}
	if ev.Channel == nil {
		ev.Channel = &bot.Channel{ID: msg.ChatID, Kind: kindOf(msg.ChatType)}
	}
	return ev
}

// kindOf 把飞书 chat_type 映射为统一会话类型。
func kindOf(chatType string) bot.MessageKind {
	switch chatType {
	case chatTypeP2P:
		return bot.MessagePrivate
	case chatTypeGroup:
		return bot.MessageGroup
	default:
		return bot.MessageChannel
	}
}

// parseContent 把飞书消息内容解析为统一消息段。
//
// 已支持 text（占位符 @_user_N 转为 SegAt）、post（富文本，统一为
// SegMarkdown，因为飞书 post 本身是结构化富文本且本适配器声明支持
// Markdown，Event.Text/PlainText 会把 Markdown 当纯文本，路由不受影响）、
// image、file；其它类型返回空段（忽略该消息内容）。
func parseContent(messageType, content string, mentions []mention) []bot.Segment {
	switch messageType {
	case messageTypeText:
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(content), &payload); err != nil {
			return nil
		}
		return splitMentions(payload.Text, mentions)

	case messageTypePost:
		text, ok := parsePost(content)
		if !ok {
			return nil
		}
		return []bot.Segment{{Type: bot.SegMarkdown, Data: map[string]any{bot.KeyText: text}}}

	case messageTypeImage:
		var payload struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(content), &payload); err != nil || payload.ImageKey == "" {
			return nil
		}
		return []bot.Segment{{Type: bot.SegImage, Data: map[string]any{bot.KeyFile: payload.ImageKey}}}

	case messageTypeFile:
		var payload struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if err := json.Unmarshal([]byte(content), &payload); err != nil || payload.FileKey == "" {
			return nil
		}
		data := map[string]any{bot.KeyFile: payload.FileKey}
		if payload.FileName != "" {
			data[bot.KeyFileName] = payload.FileName
		}
		return []bot.Segment{{Type: bot.SegFile, Data: data}}

	default:
		return nil
	}
}

// splitMentions 把文本中的 "@_user_N" 占位符替换为 SegAt 段。
//
// 命中的占位符按 mentions 中的 open_id 生成 SegAt（带显示名），未命中的
// 占位符直接删除，避免把内部标记暴露给插件。
func splitMentions(text string, mentions []mention) []bot.Segment {
	segs := make([]bot.Segment, 0, len(mentions)+1)
	var buf strings.Builder

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		segs = append(segs, bot.Segment{Type: bot.SegText, Data: map[string]any{bot.KeyText: buf.String()}})
		buf.Reset()
	}

	for len(text) > 0 {
		idx := strings.Index(text, mentionPrefix)
		if idx < 0 {
			buf.WriteString(text)
			break
		}
		buf.WriteString(text[:idx])
		rest := text[idx:]
		digits := len(mentionPrefix)
		for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		key := rest[:digits]
		text = rest[digits:]
		if digits == len(mentionPrefix) {
			// 前缀后无数字，不是占位符，按普通文本保留。
			buf.WriteString(key)
			continue
		}
		flush()
		m, ok := findMention(mentions, key)
		if !ok {
			// 未命中的占位符直接丢弃，不暴露给上层。
			continue
		}
		data := map[string]any{bot.KeyUserID: firstNonEmpty(m.ID.OpenID, m.ID.UnionID, m.ID.UserID)}
		if m.Name != "" {
			data[bot.KeyUserName] = m.Name
		}
		segs = append(segs, bot.Segment{Type: bot.SegAt, Data: data})
	}
	flush()
	return segs
}

// findMention 按占位符 key 查找被 @ 用户。
func findMention(mentions []mention, key string) (mention, bool) {
	for i := range mentions {
		if mentions[i].Key == key {
			return mentions[i], true
		}
	}
	return mention{}, false
}

// postContent 是飞书富文本消息的 content 结构。
type postContent struct {
	// Title 是标题。
	Title string `json:"title"`
	// Content 是段落列表，每个段落又是一组元素。
	Content [][]postElement `json:"content"`
}

// postElement 是富文本元素。飞书会按 locale 包装一层（如 {"zh_cn": {...}}），
// 解析时两者都尝试。
type postElement struct {
	// Tag 是元素类型，例如 "text"、"a"、"at"、"img"。
	Tag string `json:"tag"`
	// Text 是文本内容。
	Text string `json:"text"`
	// Href 是链接地址，tag 为 "a" 时有效。
	Href string `json:"href"`
	// UserID 是 @ 的目标用户，tag 为 "at" 时有效。
	UserID string `json:"user_id"`
	// UserName 是 @ 的目标显示名。
	UserName string `json:"user_name"`
	// ImageKey 是图片标识，tag 为 "img" 时有效。
	ImageKey string `json:"image_key"`
}

// parsePost 把飞书富文本解析为 Markdown 文本。
func parsePost(content string) (string, bool) {
	var outer postContent
	if err := json.Unmarshal([]byte(content), &outer); err != nil {
		return "", false
	}
	if len(outer.Content) == 0 {
		// 带 locale 包装：{"zh_cn":{"title":...,"content":[...]}}
		var byLocale map[string]postContent
		if err := json.Unmarshal([]byte(content), &byLocale); err != nil {
			return "", false
		}
		for _, v := range byLocale {
			outer = v
			break
		}
	}
	var b strings.Builder
	if outer.Title != "" {
		b.WriteString("# ")
		b.WriteString(outer.Title)
		b.WriteString("\n")
	}
	for i, para := range outer.Content {
		if i > 0 {
			b.WriteString("\n")
		}
		for j := range para {
			switch para[j].Tag {
			case "a":
				b.WriteString("[")
				b.WriteString(para[j].Text)
				b.WriteString("](")
				b.WriteString(para[j].Href)
				b.WriteString(")")
			case "at":
				b.WriteString("@")
				b.WriteString(firstNonEmpty(para[j].UserName, para[j].UserID))
			case "img":
				b.WriteString("![image](")
				b.WriteString(para[j].ImageKey)
				b.WriteString(")")
			default:
				b.WriteString(para[j].Text)
			}
		}
	}
	return b.String(), true
}

// nowUTC 把毫秒时间戳字符串解析为 UTC 时间；解析失败时返回当前时间。
func nowUTC(millis string) time.Time {
	if millis != "" {
		if v, err := strconv.ParseInt(millis, 10, 64); err == nil {
			return time.UnixMilli(v).UTC()
		}
	}
	return time.Now().UTC()
}

// fallbackID 为缺少平台事件 ID 的事件生成唯一兜底 ID。
func (a *Adapter) fallbackID() string {
	return PlatformName + "-" + a.opts.Name + "-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10) +
		"-" + strconv.FormatUint(a.seq.Add(1), 10)
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
