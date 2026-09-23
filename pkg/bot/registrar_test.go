package bot

import (
	"regexp"
	"testing"
)

func TestSegmentSupports(t *testing.T) {
	caps := Capabilities{Text: true, Image: true}
	tests := []struct {
		seg  SegmentType
		want bool
	}{
		{SegText, true},
		{SegImage, true},
		{SegMarkdown, false},
		{SegCard, false},
		{SegFile, false},
		{SegFace, true}, // 表情降级为文本
		{SegmentType("unknown"), false},
	}
	for _, tc := range tests {
		if got := caps.Supports(tc.seg); got != tc.want {
			t.Errorf("Supports(%q) = %v, want %v", tc.seg, got, tc.want)
		}
	}
}

func TestDegrade(t *testing.T) {
	full := Capabilities{Text: true, Markdown: true, Image: true, At: true, Card: true, File: true, Reply: true}

	t.Run("全能力时原样返回", func(t *testing.T) {
		msg := &Message{Segments: []Segment{{Type: SegMarkdown, Data: map[string]any{KeyText: "# t"}}}}
		got := Degrade(msg, full)
		if got != msg {
			t.Fatalf("Degrade() 应返回原消息指针，得到新对象")
		}
	})

	t.Run("不支持的能力降级为文本", func(t *testing.T) {
		msg := &Message{Segments: []Segment{
			{Type: SegMarkdown, Data: map[string]any{KeyText: "**bold**"}},
			{Type: SegImage, Data: map[string]any{KeyURL: "http://x/y.png"}},
			{Type: SegAt, Data: map[string]any{KeyUserID: "u1", KeyUserName: "小明"}},
			{Type: SegCard, Data: map[string]any{KeyCard: map[string]any{"title": "t"}}},
			{Type: SegReply, Data: map[string]any{KeyMessageID: "m1"}},
			{Type: SegFile, Data: map[string]any{KeyURL: "http://x/f.zip", KeyFileName: "f.zip"}},
			{Type: SegText, Data: map[string]any{KeyText: "tail"}},
		}}
		got := Degrade(msg, Capabilities{Text: true})
		want := []string{"**bold**", "[image] http://x/y.png", "@小明", `[card] {"title":"t"}`, "[file] f.zip http://x/f.zip", "tail"}
		if len(got.Segments) != len(want) {
			t.Fatalf("降级后段数 = %d, want %d (%v)", len(got.Segments), len(want), got.Segments)
		}
		for i, seg := range got.Segments {
			if seg.Type != SegText {
				t.Fatalf("段 %d 类型 = %q, want text", i, seg.Type)
			}
			if text, _ := seg.Data[KeyText].(string); text != want[i] {
				t.Fatalf("段 %d 文本 = %q, want %q", i, text, want[i])
			}
		}
		if len(msg.Segments) != 7 {
			t.Fatalf("Degrade 不得修改入参消息，现长度为 %d", len(msg.Segments))
		}
	})

	t.Run("nil 消息", func(t *testing.T) {
		if got := Degrade(nil, full); got != nil {
			t.Fatalf("Degrade(nil) = %v, want nil", got)
		}
	})
}

func TestRuleMatches(t *testing.T) {
	ev := func() *Event {
		return &Event{
			Type: EventMessage, Platform: "mock", BotID: "b1",
			Command: &Command{Name: "Echo", Args: []string{"hi"}},
			Sender:  &User{ID: "u1"},
			Message: &Message{Kind: MessageGroup, Segments: []Segment{{Type: SegText, Data: map[string]any{KeyText: "hello 世界"}}}},
			Channel: &Channel{ID: "g1", Kind: MessageGroup},
		}
	}

	tests := []struct {
		name string
		rule Rule
		want bool
	}{
		{name: "事件类型不匹配", rule: Rule{EventType: EventNotice}, want: false},
		{name: "会话类型不匹配", rule: Rule{Kind: MessagePrivate}, want: false},
		{name: "平台不匹配", rule: Rule{Platforms: []string{"feishu"}}, want: false},
		{name: "命令大小写不敏感", rule: Rule{Command: "echo"}, want: true},
		{name: "命令不匹配", rule: Rule{Command: "say"}, want: false},
		{name: "正则命中", rule: Rule{Regex: regexp.MustCompile(`^hello`)}, want: true},
		{name: "正则未命中", rule: Rule{Regex: regexp.MustCompile(`^bye`)}, want: false},
		{name: "关键词命中（忽略大小写）", rule: Rule{Keywords: []string{"HELLO", "x"}}, want: true},
		{name: "关键词未命中", rule: Rule{Keywords: []string{"nope"}}, want: false},
		{name: "bot 白名单为空表示匹配不到", rule: Rule{BotIDs: []string{}}, want: false},
		{name: "bot 白名单命中", rule: Rule{BotIDs: []string{"b1"}}, want: true},
		{name: "bot 白名单未命中", rule: Rule{BotIDs: []string{"b2"}}, want: false},
		{name: "nil 白名单不过滤", rule: Rule{BotIDs: nil}, want: true},
		{name: "自定义断言为假", rule: Rule{Match: func(*Event) bool { return false }}, want: false},
		{name: "全部条件命中", rule: Rule{EventType: EventMessage, Kind: MessageGroup, Platforms: []string{"mock"}, Command: "echo"}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Matches(ev()); got != tc.want {
				t.Fatalf("Matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMetadataHasPermission(t *testing.T) {
	meta := Metadata{Permissions: []Permission{PermSendMessage, PermNetwork}}
	if !meta.HasPermission(PermSendMessage) || meta.HasPermission(PermStorage) {
		t.Fatal("HasPermission 判定错误")
	}
	all := Metadata{Permissions: []Permission{PermAll}}
	if !all.HasPermission(PermStorage) {
		t.Fatal("PermAll 应包含所有权限")
	}
}

func TestRegisterPlugin(t *testing.T) {
	before := len(RegisteredPlugins())
	RegisterPlugin(nil) // 不得 panic
	if got := len(RegisteredPlugins()); got != before {
		t.Fatalf("注册 nil 插件后数量 = %d, want %d", got, before)
	}
}
