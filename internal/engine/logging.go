package engine

import (
	"context"
	"strings"

	"github.com/RandomLemon/kei/pkg/bot"
)

// logIncomingMessage 以 INFO 记录一条收到的消息事件。
//
// 事件带消息体时才记录（非消息事件不产生日志，避免心跳等高频事件噪声）。
// 字段：platform/bot/event_id/message_id/kind/user/user_name/channel/channel_name/content，
// 其中 content 是 describeMessage 渲染的消息内容，便于直接阅读。
//
// 该日志无条件写入根日志器，可见性由 log.level 控制：默认 info 级别可见，
// 设为 warn/error 即关闭。
func (e *Engine) logIncomingMessage(ctx context.Context, ev *bot.Event) {
	if ev == nil || ev.Message == nil {
		return
	}
	var userID, userName string
	if ev.Sender != nil {
		userID, userName = ev.Sender.ID, ev.Sender.Name
	}
	var channelID, channelName string
	if ev.Channel != nil {
		channelID, channelName = ev.Channel.ID, ev.Channel.Name
	}
	e.log.InfoContext(ctx, "收到消息",
		"platform", ev.Platform,
		"bot", ev.BotID,
		"event_id", ev.ID,
		"message_id", ev.Message.ID,
		"kind", string(ev.Message.Kind),
		"user", userID,
		"user_name", userName,
		"channel", channelID,
		"channel_name", channelName,
		"content", describeMessage(ev.Message),
	)
}

// logOutgoingMessage 以 INFO 记录一条实际发出的消息。
//
// 只在适配器 Send 成功后调用，字段：platform/bot/kind/channel/user/content，
// 另有 reply_to、message_id（平台返回时）。req.Message 是降级后实际发送的消息。
func (e *Engine) logOutgoingMessage(ctx context.Context, platform, botID string, req *bot.SendRequest, res *bot.SendResult) {
	args := []any{
		"platform", platform,
		"bot", botID,
		"kind", string(req.Target.Kind),
		"channel", req.Target.ChannelID,
		"user", req.Target.UserID,
		"content", describeMessage(req.Message),
	}
	if req.ReplyTo != "" {
		args = append(args, "reply_to", req.ReplyTo)
	}
	if res != nil && res.MessageID != "" {
		args = append(args, "message_id", res.MessageID)
	}
	e.log.InfoContext(ctx, "发送消息", args...)
}

// describeMessage 把消息渲染为单行文本，供日志记录。
//
// 每个段渲染为 `type:值`，多个段用空格连接；文本与 Markdown 段直接输出原文，
// 其余段输出平台无关的引用（URL/文件标识、用户 ID、消息 ID 等）。卡片段只记
// 类型名而不展开载荷，避免日志膨胀。空值段渲染为 `type:`。
func describeMessage(m *bot.Message) string {
	if m == nil {
		return ""
	}
	parts := make([]string, 0, len(m.Segments))
	for _, seg := range m.Segments {
		parts = append(parts, describeSegment(seg))
	}
	return strings.Join(parts, " ")
}

// describeSegment 渲染单个消息段。
func describeSegment(seg bot.Segment) string {
	switch seg.Type {
	case bot.SegText:
		return "text:" + dataString(seg, bot.KeyText)
	case bot.SegMarkdown:
		return "markdown:" + dataString(seg, bot.KeyText)
	case bot.SegAt:
		id := dataString(seg, bot.KeyUserID)
		if name := dataString(seg, bot.KeyUserName); name != "" {
			return "at:" + id + "(" + name + ")"
		}
		return "at:" + id
	case bot.SegImage:
		return "image:" + firstNonEmpty(dataString(seg, bot.KeyURL), dataString(seg, bot.KeyFile))
	case bot.SegFace:
		return "face:" + dataString(seg, bot.KeyFaceID)
	case bot.SegReply:
		return "reply:" + dataString(seg, bot.KeyMessageID)
	case bot.SegFile:
		return "file:" + firstNonEmpty(
			dataString(seg, bot.KeyFileName),
			dataString(seg, bot.KeyURL),
			dataString(seg, bot.KeyFile),
		)
	case bot.SegCard:
		return "card"
	default:
		// 未知段类型只记类型名，不展开 Data，契约要求消费方忽略未知类型。
		return "<" + string(seg.Type) + ">"
	}
}

// dataString 读取段数据中的字符串值；键缺失或类型不符时返回空串。
func dataString(seg bot.Segment, key string) string {
	s, _ := seg.Data[key].(string)
	return s
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
