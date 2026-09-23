package bot

import "context"

// Reply 是回复构建器，由引擎在调用 Handler 时注入。
//
// 所有链式方法都返回 Reply 自身以便连续构建；一次 Handler 调用只应调用
// 一次 Send，重复 Send 会重复发送。构建过程不加锁，Reply 不可跨 goroutine 使用。
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

// NoopReply 是只记录内容、不真正发送的 Reply 实现。
//
// 用于两处：路由器未配置 ReplyFactory 时的兜底；插件单元测试中断言
// Handler 构造出的消息内容。
type NoopReply struct {
	segs []Segment
	kind MessageKind
}

// 确保 NoopReply 满足 Reply。
var _ Reply = (*NoopReply)(nil)

// NewNoopReply 构造空回复构建器。
func NewNoopReply() *NoopReply { return &NoopReply{} }

// Text 追加纯文本段。
func (r *NoopReply) Text(s string) Reply {
	if s == "" {
		return r
	}
	return r.seg(SegText, KeyText, s)
}

// Markdown 追加 Markdown 段。
func (r *NoopReply) Markdown(s string) Reply {
	if s == "" {
		return r
	}
	return r.seg(SegMarkdown, KeyText, s)
}

// Image 追加图片段。
func (r *NoopReply) Image(url string) Reply {
	if url == "" {
		return r
	}
	return r.seg(SegImage, KeyURL, url)
}

// At 追加 @ 段。
func (r *NoopReply) At(userID string) Reply {
	if userID == "" {
		return r
	}
	return r.seg(SegAt, KeyUserID, userID)
}

// Card 追加卡片段。
func (r *NoopReply) Card(card any) Reply {
	if card == nil {
		return r
	}
	return r.seg(SegCard, KeyCard, card)
}

// Send 记录发送动作但不产生任何副作用，始终返回 nil。
func (r *NoopReply) Send(context.Context) error { return nil }

// Segments 返回已构建的消息段快照。
func (r *NoopReply) Segments() []Segment {
	if r == nil {
		return nil
	}
	return append([]Segment(nil), r.segs...)
}

// Message 返回已构建的消息；会话类型为调用方通过 SetKind 设置的值。
func (r *NoopReply) Message() *Message {
	if r == nil {
		return nil
	}
	return &Message{Kind: r.kind, Segments: append([]Segment(nil), r.segs...)}
}

// PlainText 返回已构建消息中所有文本段的拼接结果。
func (r *NoopReply) PlainText() string {
	if r == nil {
		return ""
	}
	m := Message{Segments: r.segs}
	return m.PlainText()
}

// SetKind 设置消息的会话类型，仅影响 Message 的返回值。
func (r *NoopReply) SetKind(k MessageKind) *NoopReply {
	r.kind = k
	return r
}

func (r *NoopReply) seg(t SegmentType, key string, val any) *NoopReply {
	if r == nil {
		return r
	}
	r.segs = append(r.segs, Segment{Type: t, Data: map[string]any{key: val}})
	return r
}
