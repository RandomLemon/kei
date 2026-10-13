package bot

import "context"

// Reply 是回复构建器，由引擎在调用 Handler 时注入。
//
// 所有链式方法都返回 Reply 自身以便连续构建；一次 Handler 调用只应调用
// 一次 Send，重复 Send 会重复发送。构建过程不加锁，Reply 不可跨 goroutine 使用。
//
// 引擎注入的默认实现在事件携带消息 ID 时自动引用原消息（在消息首位追加
// SegReply）；平台不支持引用时该段在降级阶段被丢弃。
//
// 需要文件段、本地路径或平台文件标识的图片、表情、显式引用或平台发送结果时，
// 用可选接口 ExtendedReply（引擎注入的默认实现与 NoopReply 都实现它）。
type Reply interface {
	// Text 追加纯文本段。
	Text(string) Reply
	// Markdown 追加 Markdown 段。
	Markdown(string) Reply
	// Image 追加图片段，参数为图片 URL，写入 Segment.Data[KeyURL]；
	// 平台文件标识不受支持（见 SegImage 的数据键约定）。
	Image(string) Reply
	// At 追加 @ 段。
	At(userID string) Reply
	// Card 追加结构化卡片段，载荷由平台适配器解释。
	Card(card any) Reply
	// Send 把已构建的消息段发送到当前事件所在会话。
	Send(ctx context.Context) error
}

// ExtendedReply 是 Reply 的可选扩展能力，引擎注入的默认构建器与 NoopReply
// 都实现它。
//
// 第三方 Reply 实现可以不实现该接口，因此使用前必须类型断言，未实现时降级到
// Reply 的基础方法：
//
//	if ext, ok := r.(bot.ExtendedReply); ok {
//		_, err = ext.Quote(msgID).File(url, name).SendResult(ctx)
//	} else {
//		err = r.Text(url).Send(ctx)
//	}
//
// 扩展方法返回 ExtendedReply（而非 Reply）以便连续构建与以 SendResult 收尾。
type ExtendedReply interface {
	Reply
	// ImageFile 追加图片段，参数为本地文件路径或平台文件标识（写入 KeyFile）。
	ImageFile(file string) ExtendedReply
	// File 追加文件段，url 为可下载地址、name 为文件名（写入 KeyURL/KeyFileName）。
	File(url, name string) ExtendedReply
	// FileID 追加文件段，file 为本地文件路径或平台文件标识（写入 KeyFile/KeyFileName）。
	FileID(file, name string) ExtendedReply
	// Face 追加表情段，id 为平台表情 ID（写入 KeyFaceID）。
	Face(id string) ExtendedReply
	// Quote 追加引用段，引用指定消息 ID（写入 KeyMessageID）；显式引用优先，
	// 消息中已有引用段时 Send/SendResult 不再追加对事件消息的自动引用。
	Quote(messageID string) ExtendedReply
	// SendResult 发送已构建的消息并返回平台发送结果，失败语义与 Send 相同。
	SendResult(ctx context.Context) (*SendResult, error)
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

// 确保 NoopReply 满足 Reply 与 ExtendedReply。
var (
	_ Reply         = (*NoopReply)(nil)
	_ ExtendedReply = (*NoopReply)(nil)
)

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

// ImageFile 追加图片段（本地文件路径或平台文件标识）。
func (r *NoopReply) ImageFile(file string) ExtendedReply {
	if file == "" {
		return r
	}
	return r.seg(SegImage, KeyFile, file)
}

// File 追加文件段（可下载地址 + 文件名）。
func (r *NoopReply) File(url, name string) ExtendedReply {
	if url == "" {
		return r
	}
	data := map[string]any{KeyURL: url}
	if name != "" {
		data[KeyFileName] = name
	}
	return r.segData(SegFile, data)
}

// FileID 追加文件段（本地文件路径或平台文件标识 + 文件名）。
func (r *NoopReply) FileID(file, name string) ExtendedReply {
	if file == "" {
		return r
	}
	data := map[string]any{KeyFile: file}
	if name != "" {
		data[KeyFileName] = name
	}
	return r.segData(SegFile, data)
}

// Face 追加表情段。
func (r *NoopReply) Face(id string) ExtendedReply {
	if id == "" {
		return r
	}
	return r.seg(SegFace, KeyFaceID, id)
}

// Quote 追加引用段。
func (r *NoopReply) Quote(messageID string) ExtendedReply {
	if messageID == "" {
		return r
	}
	return r.seg(SegReply, KeyMessageID, messageID)
}

// Send 记录发送动作但不产生任何副作用，始终返回 nil。
func (r *NoopReply) Send(context.Context) error { return nil }

// SendResult 记录发送动作但不产生任何副作用，始终返回空结果与 nil。
func (r *NoopReply) SendResult(context.Context) (*SendResult, error) {
	return &SendResult{}, nil
}

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
	return r.segData(t, map[string]any{key: val})
}

func (r *NoopReply) segData(t SegmentType, data map[string]any) *NoopReply {
	if r == nil {
		return r
	}
	r.segs = append(r.segs, Segment{Type: t, Data: data})
	return r
}
