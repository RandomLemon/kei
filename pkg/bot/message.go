package bot

import "strings"

// MessageKind 描述消息所在会话的类型。
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

// Message 是平台无关的消息，由若干消息段组成。
type Message struct {
	// ID 是消息 ID，发送前可为空。
	ID string
	// Kind 是消息所在会话类型。
	Kind MessageKind
	// Segments 是按顺序排列的消息段。
	Segments []Segment
}

// SegmentType 是消息段类型。
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

// Segment.Data 中约定的键名。
//
// 适配器与插件都必须使用这些常量而不是字面量，避免跨包字段漂移。
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

// Segment 是消息的一个片段。
type Segment struct {
	// Type 是片段类型。
	Type SegmentType
	// Data 是片段数据，键名使用本包的 Key* 常量。
	Data map[string]any
}

// Text 返回消息中所有文本段的拼接结果。
//
// Markdown 段按纯文本对待；其它类型忽略。用于路由的命令与关键词匹配。
func (m *Message) PlainText() string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, seg := range m.Segments {
		switch seg.Type {
		case SegText, SegMarkdown:
			if s, ok := seg.Data[KeyText].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}
