// Package reply 实现 bot.Reply 的默认构建器。
//
// 构建器把链式调用累积为消息段，直到 Send 时才通过 BotAPI 发送一次。
package reply

import (
	"context"
	"errors"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Builder 是 bot.Reply 的默认实现，由引擎为每一次规则调用创建。
type Builder struct {
	api  bot.BotAPI
	ev   *bot.Event
	kind bot.MessageKind
	segs []bot.Segment
}

// 确保 Builder 满足 bot.Reply 与 bot.ExtendedReply。
var (
	_ bot.Reply         = (*Builder)(nil)
	_ bot.ExtendedReply = (*Builder)(nil)
)

// New 构造针对某个事件的回复构建器。
func New(api bot.BotAPI, ev *bot.Event) *Builder {
	b := &Builder{api: api, ev: ev}
	if ev != nil {
		if ev.Message != nil {
			b.kind = ev.Message.Kind
		} else if ev.Channel != nil {
			b.kind = ev.Channel.Kind
		}
	}
	if b.kind == "" {
		b.kind = bot.MessagePrivate
	}
	return b
}

// Text 追加纯文本段。
func (b *Builder) Text(s string) bot.Reply {
	if s == "" {
		return b
	}
	return b.add(bot.SegText, bot.KeyText, s)
}

// Markdown 追加 Markdown 段。
func (b *Builder) Markdown(s string) bot.Reply {
	if s == "" {
		return b
	}
	return b.add(bot.SegMarkdown, bot.KeyText, s)
}

// Image 追加图片段。
func (b *Builder) Image(url string) bot.Reply {
	if url == "" {
		return b
	}
	return b.add(bot.SegImage, bot.KeyURL, url)
}

// At 追加 @ 段。
func (b *Builder) At(userID string) bot.Reply {
	if userID == "" {
		return b
	}
	return b.add(bot.SegAt, bot.KeyUserID, userID)
}

// Card 追加卡片段。
func (b *Builder) Card(card any) bot.Reply {
	if card == nil {
		return b
	}
	return b.add(bot.SegCard, bot.KeyCard, card)
}

// ImageFile 追加图片段，参数为本地文件路径或平台文件标识（写入 KeyFile）。
func (b *Builder) ImageFile(file string) bot.ExtendedReply {
	if file == "" {
		return b
	}
	return b.add(bot.SegImage, bot.KeyFile, file)
}

// File 追加文件段，url 为可下载地址、name 为文件名（写入 KeyURL/KeyFileName）。
func (b *Builder) File(url, name string) bot.ExtendedReply {
	if url == "" {
		return b
	}
	data := map[string]any{bot.KeyURL: url}
	if name != "" {
		data[bot.KeyFileName] = name
	}
	return b.addData(bot.SegFile, data)
}

// FileID 追加文件段，file 为本地文件路径或平台文件标识、name 为文件名
// （写入 KeyFile/KeyFileName）。
func (b *Builder) FileID(file, name string) bot.ExtendedReply {
	if file == "" {
		return b
	}
	data := map[string]any{bot.KeyFile: file}
	if name != "" {
		data[bot.KeyFileName] = name
	}
	return b.addData(bot.SegFile, data)
}

// Face 追加表情段，id 为平台表情 ID（写入 KeyFaceID）。
func (b *Builder) Face(id string) bot.ExtendedReply {
	if id == "" {
		return b
	}
	return b.add(bot.SegFace, bot.KeyFaceID, id)
}

// Quote 追加引用段，引用指定消息 ID（写入 KeyMessageID）。
//
// 显式引用优先：消息中已有引用段时，Send/SendResult 不再追加对事件消息的
// 自动引用。
func (b *Builder) Quote(messageID string) bot.ExtendedReply {
	if messageID == "" {
		return b
	}
	return b.add(bot.SegReply, bot.KeyMessageID, messageID)
}

// Send 向事件所在会话发送已构建的消息；没有消息段时返回错误而不发送。
//
// 语义与 SendResult 相同，只是丢弃发送结果，便于 `return r.Text(...).Send(ctx)`。
func (b *Builder) Send(ctx context.Context) error {
	_, err := b.SendResult(ctx)
	return err
}

// SendResult 向事件所在会话发送已构建的消息并返回平台发送结果；没有消息段时
// 返回错误而不发送。
//
// 事件携带消息 ID 且消息中还没有引用段时自动在段列表首位追加 SegReply 引用
// 该消息（显式 Quote 的段优先，不重复追加）：平台声明 Capabilities.Reply 时
// 引用原消息，否则该段在降级阶段被丢弃、其余内容照常发送。引用段按次构建，
// 不写入已累积的段列表，重复发送不会累积。
func (b *Builder) SendResult(ctx context.Context) (*bot.SendResult, error) {
	if b.api == nil {
		return nil, errors.New("reply: no bot api configured")
	}
	if len(b.segs) == 0 {
		return nil, errors.New("reply: empty message")
	}
	segs := b.segs
	if b.ev != nil && b.ev.Message != nil && b.ev.Message.ID != "" && !hasReplySegment(b.segs) {
		segs = make([]bot.Segment, 0, len(b.segs)+1)
		segs = append(segs, bot.Segment{
			Type: bot.SegReply,
			Data: map[string]any{bot.KeyMessageID: b.ev.Message.ID},
		})
		segs = append(segs, b.segs...)
	}
	msg := &bot.Message{Kind: b.kind, Segments: segs}
	return b.api.Reply(ctx, b.ev, msg)
}

// hasReplySegment 判断段列表中是否已有引用段；显式 Quote 优先于自动引用。
func hasReplySegment(segs []bot.Segment) bool {
	for i := range segs {
		if segs[i].Type == bot.SegReply {
			return true
		}
	}
	return false
}

// Segments 返回已构建消息段的快照。
func (b *Builder) Segments() []bot.Segment {
	return append([]bot.Segment(nil), b.segs...)
}

func (b *Builder) add(t bot.SegmentType, key string, val any) *Builder {
	return b.addData(t, map[string]any{key: val})
}

func (b *Builder) addData(t bot.SegmentType, data map[string]any) *Builder {
	b.segs = append(b.segs, bot.Segment{Type: t, Data: data})
	return b
}
