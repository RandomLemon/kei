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
			appendText(str(seg.Data[KeyText]))
		case SegImage:
			appendText("[image] " + firstNonEmpty(str(seg.Data[KeyURL]), str(seg.Data[KeyFile])))
		case SegFile:
			appendText("[file] " + joinNonEmpty(" ", str(seg.Data[KeyFileName]), firstNonEmpty(str(seg.Data[KeyURL]), str(seg.Data[KeyFile]))))
		case SegAt:
			appendText("@" + firstNonEmpty(str(seg.Data[KeyUserName]), str(seg.Data[KeyUserID])))
		case SegFace:
			appendText("[face] " + str(seg.Data[KeyFaceID]))
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

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
