package bot

import (
	"fmt"
	"unicode/utf8"
)

// SegmentData 读取段数据中指定键的值并渲染为字符串，是 Segment.Data 的唯一取值约定。
//
// 规则：seg.Data 为 nil、键缺失或值为 nil 时返回空串；值本身是 string 时原样返回；
// 其余类型一律按 fmt.Sprint 格式化（数字、布尔因此会渲染成文本）。
//
// Segment.Data 的契约取值是标量（见 docs/domain-model.md 5.6），容器类型不属于契约，
// 对其只承诺 fmt.Sprint 的通用行为，调用方不应依赖其具体格式。
//
// 注意：本函数是「宽松取值」，供适配器与日志渲染使用；Message.PlainText 对文本段仍按
// 严格类型断言读取 KeyText（消息契约要求 KeyText 只能是 string），路由匹配行为不受
// 本函数影响。
func SegmentData(seg Segment, key string) string {
	v := seg.Data[key]
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// FirstNonEmpty 返回第一个非空字符串；全部为空时返回空串。
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Truncate 把 s 截断到不超过 limit 字节并在末尾追加 "..."，用于日志或错误信息中
// 回显响应体，避免单条大响应刷屏。
//
// limit <= 0 或 len(s) <= limit 时原样返回；需要截断时不会切开 UTF-8 序列，会向前
// 回退到最近的一个符文起始字节。
func Truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "..."
}
