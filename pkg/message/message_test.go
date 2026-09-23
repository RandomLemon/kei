package message

import (
	"testing"

	"github.com/RandomLemon/kei/pkg/bot"
)

func TestSegmentBuilders(t *testing.T) {
	tests := []struct {
		name    string
		seg     bot.Segment
		wantTyp bot.SegmentType
		wantKey string
		wantVal any
	}{
		{name: "文本", seg: Text("hi"), wantTyp: bot.SegText, wantKey: bot.KeyText, wantVal: "hi"},
		{name: "Markdown", seg: Markdown("# t"), wantTyp: bot.SegMarkdown, wantKey: bot.KeyText, wantVal: "# t"},
		{name: "图片 URL", seg: Image("http://x/y.png"), wantTyp: bot.SegImage, wantKey: bot.KeyURL, wantVal: "http://x/y.png"},
		{name: "图片文件", seg: ImageFile("/tmp/y.png"), wantTyp: bot.SegImage, wantKey: bot.KeyFile, wantVal: "/tmp/y.png"},
		{name: "At", seg: At("u1"), wantTyp: bot.SegAt, wantKey: bot.KeyUserID, wantVal: "u1"},
		{name: "表情", seg: Face("1"), wantTyp: bot.SegFace, wantKey: bot.KeyFaceID, wantVal: "1"},
		{name: "引用", seg: Reply("m1"), wantTyp: bot.SegReply, wantKey: bot.KeyMessageID, wantVal: "m1"},
		{name: "文件", seg: File("http://x/f.zip", "f.zip"), wantTyp: bot.SegFile, wantKey: bot.KeyURL, wantVal: "http://x/f.zip"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.seg.Type != tc.wantTyp {
				t.Fatalf("Type = %q, want %q", tc.seg.Type, tc.wantTyp)
			}
			if got := tc.seg.Data[tc.wantKey]; got != tc.wantVal {
				t.Fatalf("Data[%q] = %v, want %v", tc.wantKey, got, tc.wantVal)
			}
		})
	}

	at := AtName("u1", "小明")
	if at.Data[bot.KeyUserName] != "小明" {
		t.Fatalf("AtName 缺少显示名: %+v", at.Data)
	}
	card := Card(map[string]any{"title": "t"})
	if card.Type != bot.SegCard || card.Data[bot.KeyCard] == nil {
		t.Fatalf("Card = %+v", card)
	}
	if f := File("http://x/f.zip", "f.zip"); f.Data[bot.KeyFileName] != "f.zip" {
		t.Fatalf("File 缺少文件名: %+v", f.Data)
	}
}

func TestMessageBuilders(t *testing.T) {
	group := Group(Text("a"), Text("b"))
	if group.Kind != bot.MessageGroup || len(group.Segments) != 2 {
		t.Fatalf("Group = %+v", group)
	}
	if got := group.PlainText(); got != "ab" {
		t.Fatalf("PlainText = %q", got)
	}

	if got := Private(Text("x")).Kind; got != bot.MessagePrivate {
		t.Fatalf("Private Kind = %q", got)
	}
	if got := Channel(Text("x")).Kind; got != bot.MessageChannel {
		t.Fatalf("Channel Kind = %q", got)
	}
	if got := Plain(bot.MessageGroup, "hi"); got.Kind != bot.MessageGroup || len(got.Segments) != 1 {
		t.Fatalf("Plain = %+v", got)
	}
}

func TestNewCopiesSegments(t *testing.T) {
	segs := []bot.Segment{Text("a")}
	msg := New(bot.MessageGroup, segs...)
	segs[0] = Text("mutated")

	if got := msg.PlainText(); got != "a" {
		t.Fatalf("New 必须拷贝入参切片, PlainText = %q", got)
	}
}
