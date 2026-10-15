package bot

import (
	"encoding/json"
	"fmt"
)

// Degrade 按平台能力降级消息段，返回可直接发送的消息。
//
// 降级规则：Markdown 转纯文本；图片/文件转带链接的文本；@ 转 "@名字"；
// 卡片转为 JSON 文本；引用段在不支持时丢弃。所有段都被支持时原样返回，
// 不产生拷贝。入参消息不会被修改。
func Degrade(msg *Message, caps Capabilities) *Message {
	if msg == nil {
		return nil
	}
	need := false
	for i := range msg.Segments {
		if !caps.Supports(msg.Segments[i].Type) {
			need = true
			break
		}
	}
	if !need {
		return msg
	}

	out := &Message{ID: msg.ID, Kind: msg.Kind, Segments: make([]Segment, 0, len(msg.Segments))}
	appendText := func(s string) {
		if s == "" {
			return
		}
		out.Segments = append(out.Segments, Segment{Type: SegText, Data: map[string]any{KeyText: s}})
	}

	for _, seg := range msg.Segments {
		if caps.Supports(seg.Type) {
			out.Segments = append(out.Segments, seg)
			continue
		}
		switch seg.Type {
		case SegMarkdown:
			appendText(SegmentData(seg, KeyText))
		case SegImage:
			appendText("[image] " + FirstNonEmpty(SegmentData(seg, KeyURL), SegmentData(seg, KeyFile)))
		case SegFile:
			appendText("[file] " + joinNonEmpty(" ", SegmentData(seg, KeyFileName), FirstNonEmpty(SegmentData(seg, KeyURL), SegmentData(seg, KeyFile))))
		case SegAt:
			appendText("@" + FirstNonEmpty(SegmentData(seg, KeyUserName), SegmentData(seg, KeyUserID)))
		case SegFace:
			appendText("[face] " + SegmentData(seg, KeyFaceID))
		case SegCard:
			appendText("[card] " + jsonText(seg.Data[KeyCard]))
		case SegReply:
			// 平台不支持引用时丢弃，消息本身仍然发送。
		default:
			appendText("[" + string(seg.Type) + "]")
		}
	}
	return out
}

func jsonText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func joinNonEmpty(sep string, vals ...string) string {
	out := ""
	for _, v := range vals {
		if v == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += v
	}
	return out
}
