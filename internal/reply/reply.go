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

// 确保 Builder 满足 bot.Reply。
var _ bot.Reply = (*Builder)(nil)

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

// Send 向事件所在会话发送已构建的消息；没有消息段时返回错误而不发送。
func (b *Builder) Send(ctx context.Context) error {
	if b.api == nil {
		return errors.New("reply: no bot api configured")
	}
	if len(b.segs) == 0 {
		return errors.New("reply: empty message")
	}
	msg := &bot.Message{Kind: b.kind, Segments: b.segs}
	if b.ev != nil && b.ev.Message != nil {
		msg.ID = b.ev.Message.ID
	}
	_, err := b.api.Reply(ctx, b.ev, msg)
	return err
}

// Segments 返回已构建消息段的快照。
func (b *Builder) Segments() []bot.Segment {
	return append([]bot.Segment(nil), b.segs...)
}

func (b *Builder) add(t bot.SegmentType, key string, val any) *Builder {
	b.segs = append(b.segs, bot.Segment{Type: t, Data: map[string]any{key: val}})
	return b
}
