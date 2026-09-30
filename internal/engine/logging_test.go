package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/RandomLemon/kei/adapters/mock"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/pkg/bot"
)

// newLogTestEngine 构造一个只绑定 mock 适配器、日志写入 buf 的引擎。
func newLogTestEngine(t *testing.T, buf *bytes.Buffer) *Engine {
	t.Helper()
	cfg := &config.Config{
		Bots:    []config.BotConfig{{Name: "mock-main", Adapter: "mock"}},
		Plugins: map[string]config.PluginConfig{},
	}
	eng, err := New(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Adapters: []AdapterBinding{
			{BotID: "mock-main", Adapter: mock.New(mock.Options{Name: "mock-main", Platform: "mock"})},
		},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return eng
}

func TestDescribeMessage(t *testing.T) {
	t.Run("all segment kinds", func(t *testing.T) {
		msg := &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi"}},
			{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "u1", bot.KeyUserName: "Alice"}},
			{Type: bot.SegImage, Data: map[string]any{bot.KeyURL: "https://x/y.png"}},
			{Type: bot.SegFile, Data: map[string]any{bot.KeyFile: "f1", bot.KeyFileName: "a.txt"}},
			{Type: bot.SegFace, Data: map[string]any{bot.KeyFaceID: "1"}},
			{Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: "m9"}},
			{Type: bot.SegCard, Data: map[string]any{bot.KeyCard: map[string]any{"k": "v"}}},
			{Type: bot.SegmentType("future")},
		}}
		want := "text:hi at:u1(Alice) image:https://x/y.png file:a.txt face:1 reply:m9 card <future>"
		if got := describeMessage(msg); got != want {
			t.Fatalf("describeMessage = %q, want %q", got, want)
		}
	})

	t.Run("nil and image file fallback", func(t *testing.T) {
		if got := describeMessage(nil); got != "" {
			t.Fatalf("nil message = %q, want empty", got)
		}
		msg := &bot.Message{Segments: []bot.Segment{
			{Type: bot.SegImage, Data: map[string]any{bot.KeyFile: "local.png"}},
		}}
		if got := describeMessage(msg); got != "image:local.png" {
			t.Fatalf("image fallback = %q", got)
		}
	})
}

func TestHandleLogsIncomingMessageAtInfo(t *testing.T) {
	var buf bytes.Buffer
	eng := newLogTestEngine(t, &buf)

	ev := &bot.Event{
		ID:       "e1",
		Type:     bot.EventMessage,
		Platform: "mock",
		BotID:    "mock-main",
		Sender:   &bot.User{ID: "u1", Name: "Alice"},
		Channel:  &bot.Channel{ID: "g1", Name: "Group"},
		Message: &bot.Message{ID: "m1", Kind: bot.MessageGroup, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hello"}},
		}},
	}
	eng.handle(context.Background(), ev)

	out := buf.String()
	for _, want := range []string{
		"level=INFO", "msg=收到消息",
		"user=u1", "user_name=Alice", "channel=g1", "channel_name=Group",
		"message_id=m1", "content=text:hello",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("收到消息日志缺少 %q:\n%s", want, out)
		}
	}
}

func TestHandleSkipsNonMessageEvent(t *testing.T) {
	var buf bytes.Buffer
	eng := newLogTestEngine(t, &buf)

	eng.handle(context.Background(), &bot.Event{ID: "e2", Type: bot.EventNotice, Platform: "mock", BotID: "mock-main"})
	if strings.Contains(buf.String(), "收到消息") {
		t.Fatalf("非消息事件不应产生 INFO 日志:\n%s", buf.String())
	}
}

func TestSendRequestLogsOutgoingMessageAtInfo(t *testing.T) {
	var buf bytes.Buffer
	eng := newLogTestEngine(t, &buf)

	msg := &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: "reply"}},
		{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "u1"}},
	}}
	res, err := eng.SendRequest(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: "mock", BotID: "mock-main", ChannelID: "g1", UserID: "u1", Kind: bot.MessageGroup},
		Message: msg,
		ReplyTo: "m0",
	})
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if res == nil || res.MessageID == "" {
		t.Fatalf("SendResult = %+v", res)
	}

	out := buf.String()
	for _, want := range []string{
		"level=INFO", "msg=发送消息",
		"bot=mock-main", "channel=g1", "user=u1",
		"content=\"text:reply at:u1\"", "reply_to=m0", "message_id=" + res.MessageID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("发送消息日志缺少 %q:\n%s", want, out)
		}
	}
}
