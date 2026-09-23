// Package message 提供消息与消息段的构建器。
//
// 插件通过本包构造 bot.Message，再交给 bot.BotAPI 或 bot.Reply 发送，
// 从而避免在各处手写 map 字面量。
package message

import "github.com/RandomLemon/kei/pkg/bot"

// Text 构造纯文本段。
func Text(s string) bot.Segment {
	return bot.Segment{Type: bot.SegText, Data: map[string]any{bot.KeyText: s}}
}

// Markdown 构造 Markdown 段，平台不支持时会降级为纯文本。
func Markdown(s string) bot.Segment {
	return bot.Segment{Type: bot.SegMarkdown, Data: map[string]any{bot.KeyText: s}}
}

// Image 通过 URL 构造图片段。
func Image(url string) bot.Segment {
	return bot.Segment{Type: bot.SegImage, Data: map[string]any{bot.KeyURL: url}}
}

// ImageFile 通过本地文件路径或平台文件标识构造图片段。
func ImageFile(file string) bot.Segment {
	return bot.Segment{Type: bot.SegImage, Data: map[string]any{bot.KeyFile: file}}
}

// At 构造 @ 段。
func At(userID string) bot.Segment {
	return bot.Segment{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: userID}}
}

// AtName 构造带显示名的 @ 段，平台不支持时会降级为 "@名字"。
func AtName(userID, name string) bot.Segment {
	return bot.Segment{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: userID, bot.KeyUserName: name}}
}

// Face 构造表情段。
func Face(id string) bot.Segment {
	return bot.Segment{Type: bot.SegFace, Data: map[string]any{bot.KeyFaceID: id}}
}

// Reply 构造引用段。
func Reply(messageID string) bot.Segment {
	return bot.Segment{Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: messageID}}
}

// Card 构造卡片段。
func Card(card any) bot.Segment {
	return bot.Segment{Type: bot.SegCard, Data: map[string]any{bot.KeyCard: card}}
}

// File 构造文件段。
func File(url, name string) bot.Segment {
	return bot.Segment{Type: bot.SegFile, Data: map[string]any{bot.KeyURL: url, bot.KeyFileName: name}}
}

// New 按会话类型构造消息。
func New(kind bot.MessageKind, segs ...bot.Segment) *bot.Message {
	return &bot.Message{Kind: kind, Segments: append([]bot.Segment(nil), segs...)}
}

// Group 构造群聊消息。
func Group(segs ...bot.Segment) *bot.Message {
	return New(bot.MessageGroup, segs...)
}

// Private 构造私聊消息。
func Private(segs ...bot.Segment) *bot.Message {
	return New(bot.MessagePrivate, segs...)
}

// Channel 构造频道消息。
func Channel(segs ...bot.Segment) *bot.Message {
	return New(bot.MessageChannel, segs...)
}

// Plain 用会话类型与文本构造单段消息。
func Plain(kind bot.MessageKind, text string) *bot.Message {
	return &bot.Message{Kind: kind, Segments: []bot.Segment{Text(text)}}
}
