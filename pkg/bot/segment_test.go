package bot

import (
	"encoding/json"
	"testing"
)

func TestSegmentData(t *testing.T) {
	tests := []struct {
		name string
		seg  Segment
		key  string
		want string
	}{
		{
			name: "Data 为 nil",
			seg:  Segment{Type: SegText},
			key:  KeyText,
			want: "",
		},
		{
			name: "键缺失",
			seg:  Segment{Data: map[string]any{KeyURL: "http://e/x"}},
			key:  KeyText,
			want: "",
		},
		{
			name: "值为 nil",
			seg:  Segment{Data: map[string]any{KeyText: nil}},
			key:  KeyText,
			want: "",
		},
		{
			name: "string 原样",
			seg:  Segment{Data: map[string]any{KeyText: "你好"}},
			key:  KeyText,
			want: "你好",
		},
		{
			name: "float64 整数",
			seg:  Segment{Data: map[string]any{KeyMessageID: float64(9001)}},
			key:  KeyMessageID,
			want: "9001",
		},
		{
			name: "float64 小数",
			seg:  Segment{Data: map[string]any{KeyText: 1.5}},
			key:  KeyText,
			want: "1.5",
		},
		{
			name: "int",
			seg:  Segment{Data: map[string]any{KeyFaceID: 7}},
			key:  KeyFaceID,
			want: "7",
		},
		{
			name: "int64",
			seg:  Segment{Data: map[string]any{KeyFaceID: int64(-9)}},
			key:  KeyFaceID,
			want: "-9",
		},
		{
			name: "json.Number",
			seg:  Segment{Data: map[string]any{KeyUserID: json.Number("10001")}},
			key:  KeyUserID,
			want: "10001",
		},
		{
			name: "bool true",
			seg:  Segment{Data: map[string]any{KeyUserID: true}},
			key:  KeyUserID,
			want: "true",
		},
		{
			name: "bool false 不被当作缺失",
			seg:  Segment{Data: map[string]any{KeyUserID: false}},
			key:  KeyUserID,
			want: "false",
		},
		{
			name: "容器不属契约，按 fmt.Sprint 通用行为",
			seg:  Segment{Data: map[string]any{KeyCard: []any{"a", "b"}}},
			key:  KeyCard,
			want: "[a b]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SegmentData(tc.seg, tc.key); got != tc.want {
				t.Errorf("SegmentData = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestFirstNonEmpty(t *testing.T) {
	tests := []struct {
		name string
		vals []string
		want string
	}{
		{name: "无参数", vals: nil, want: ""},
		{name: "全空", vals: []string{"", "", ""}, want: ""},
		{name: "第一个非空", vals: []string{"", "b", "c"}, want: "b"},
		{name: "首个即非空", vals: []string{"a", "b"}, want: "a"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstNonEmpty(tc.vals...); got != tc.want {
				t.Errorf("FirstNonEmpty = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		s     string
		limit int
		want  string
	}{
		{name: "limit 为 0 原样返回", s: "abcdef", limit: 0, want: "abcdef"},
		{name: "limit 为负原样返回", s: "abcdef", limit: -1, want: "abcdef"},
		{name: "不超长原样返回", s: "abc", limit: 3, want: "abc"},
		{name: "短于上限原样返回", s: "abc", limit: 10, want: "abc"},
		{name: "空串", s: "", limit: 5, want: ""},
		{name: "ASCII 截断", s: "abcdef", limit: 3, want: "abc..."},
		{name: "边界落在符文中间回退", s: "你好世界", limit: 4, want: "你..."},
		{name: "边界正好落在符文起点", s: "你好世界", limit: 6, want: "你好..."},
		{name: "全为多字节且上限小于首字", s: "你好", limit: 1, want: "..."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.s, tc.limit); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q，期望 %q", tc.s, tc.limit, got, tc.want)
			}
		})
	}
}
