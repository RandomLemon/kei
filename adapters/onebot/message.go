package onebot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/RandomLemon/kei/pkg/bot"
)

// onebotSegment 是 OneBot v11 array 格式的消息段。
type onebotSegment struct {
	// Type 是段类型，例如 text、image、at。
	Type string `json:"type"`
	// Data 是段数据，键名由 OneBot 协议定义。
	Data map[string]any `json:"data"`
}

// parseMessage 把 OneBot 的 message 字段转换为统一的 bot.Message。
//
// message 既可能是段数组（array 格式），也可能是 CQ 码字符串；字符串形式无法
// 可靠地逆解析，因此整体作为单个文本段交给上层处理。未知段类型会被跳过并记录
// warn 日志，不会导致整条事件解析失败。
func parseMessage(raw []byte, kind bot.MessageKind, log *slog.Logger) (*bot.Message, error) {
	msg := &bot.Message{Kind: kind}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return msg, nil
	}

	switch trimmed[0] {
	case '"':
		var text string
		if err := decodeJSON(trimmed, &text); err != nil {
			return nil, fmt.Errorf("onebot: 解析 message 字符串失败: %w", err)
		}
		if text != "" {
			msg.Segments = append(msg.Segments, bot.Segment{
				Type: bot.SegText,
				Data: map[string]any{bot.KeyText: text},
			})
		}
		return msg, nil

	case '[':
		var segs []onebotSegment
		if err := decodeJSON(trimmed, &segs); err != nil {
			return nil, fmt.Errorf("onebot: 解析 message 数组失败: %w", err)
		}
		for i := range segs {
			seg, ok := convertSegment(segs[i], log)
			if !ok {
				continue
			}
			msg.Segments = append(msg.Segments, seg)
		}
		return msg, nil

	default:
		return nil, fmt.Errorf("onebot: message 字段既不是数组也不是字符串")
	}
}

// convertSegment 把单个 OneBot 段转换为统一段。第二个返回值表示是否成功转换，
// false 时调用方应跳过该段。
func convertSegment(in onebotSegment, log *slog.Logger) (bot.Segment, bool) {
	data := in.Data
	switch in.Type {
	case "text":
		return bot.Segment{
			Type: bot.SegText,
			Data: map[string]any{bot.KeyText: jsonString(data["text"])},
		}, true

	case "image":
		d := map[string]any{}
		setIfNotEmpty(d, bot.KeyURL, jsonString(data["url"]))
		setIfNotEmpty(d, bot.KeyFile, jsonString(data["file"]))
		return bot.Segment{Type: bot.SegImage, Data: d}, true

	case "at":
		// OneBot 标准键为 qq，部分实现使用 user_id，两者都接受。
		d := map[string]any{
			bot.KeyUserID: firstNonEmpty(jsonString(data["qq"]), jsonString(data["user_id"])),
		}
		setIfNotEmpty(d, bot.KeyUserName, jsonString(data["name"]))
		return bot.Segment{Type: bot.SegAt, Data: d}, true

	case "face":
		return bot.Segment{
			Type: bot.SegFace,
			Data: map[string]any{bot.KeyFaceID: jsonString(data["id"])},
		}, true

	case "reply":
		return bot.Segment{
			Type: bot.SegReply,
			Data: map[string]any{bot.KeyMessageID: jsonString(data["id"])},
		}, true

	case "file":
		d := map[string]any{}
		setIfNotEmpty(d, bot.KeyFile, jsonString(data["file"]))
		setIfNotEmpty(d, bot.KeyURL, jsonString(data["url"]))
		setIfNotEmpty(d, bot.KeyFileName, firstNonEmpty(jsonString(data["name"]), jsonString(data["file_name"])))
		return bot.Segment{Type: bot.SegFile, Data: d}, true

	default:
		if log != nil {
			log.Warn("onebot: 跳过不支持的消息段", "type", in.Type)
		}
		return bot.Segment{}, false
	}
}

// buildArray 把统一消息转换为 OneBot array 格式的段列表。
//
// 入参消息应已按平台能力降级（见 bot.Degrade），此处仍对 Markdown 段做兜底，
// 避免未降级的调用方产生不可发送的载荷。无法映射的段会被跳过并记录 warn。
func buildArray(msg *bot.Message, log *slog.Logger) []onebotSegment {
	out := make([]onebotSegment, 0, len(msg.Segments))
	for i := range msg.Segments {
		seg := msg.Segments[i]
		d := map[string]any{}
		switch seg.Type {
		case bot.SegText, bot.SegMarkdown:
			out = append(out, onebotSegment{Type: "text", Data: map[string]any{"text": dataString(seg, bot.KeyText)}})
			continue

		case bot.SegImage:
			url := dataString(seg, bot.KeyURL)
			d["file"] = firstNonEmpty(dataString(seg, bot.KeyFile), url)
			setIfNotEmpty(d, "url", url)

		case bot.SegAt:
			d["qq"] = dataString(seg, bot.KeyUserID)
			setIfNotEmpty(d, "name", dataString(seg, bot.KeyUserName))

		case bot.SegFace:
			d["id"] = dataString(seg, bot.KeyFaceID)

		case bot.SegReply:
			d["id"] = dataString(seg, bot.KeyMessageID)

		case bot.SegFile:
			url := dataString(seg, bot.KeyURL)
			d["file"] = firstNonEmpty(dataString(seg, bot.KeyFile), url)
			setIfNotEmpty(d, "url", url)
			setIfNotEmpty(d, "name", dataString(seg, bot.KeyFileName))

		default:
			if log != nil {
				log.Warn("onebot: 跳过无法发送的消息段", "type", string(seg.Type))
			}
			continue
		}
		out = append(out, onebotSegment{Type: segTypeName(seg.Type), Data: d})
	}
	return out
}

// segTypeName 返回统一段类型在 OneBot 协议中的名称。
func segTypeName(t bot.SegmentType) string {
	switch t {
	case bot.SegAt:
		return "at"
	case bot.SegFace:
		return "face"
	case bot.SegReply:
		return "reply"
	case bot.SegFile:
		return "file"
	case bot.SegImage:
		return "image"
	default:
		return "text"
	}
}

// dataString 读取段数据中指定键的字符串值。
func dataString(seg bot.Segment, key string) string {
	if seg.Data == nil {
		return ""
	}
	return jsonString(seg.Data[key])
}

// jsonString 把 JSON 反序列化得到的任意值转换为字符串。
func jsonString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
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

// setIfNotEmpty 在值非空时写入 map，避免产生空字段。
func setIfNotEmpty(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}
