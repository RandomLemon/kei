package reply

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/RandomLemon/kei/pkg/bot"
)

type fakeAPI struct {
	target bot.Target
	msg    *bot.Message
	calls  int
	err    error
}

func (f *fakeAPI) Send(context.Context, bot.Target, *bot.Message) (*bot.SendResult, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeAPI) Reply(_ context.Context, ev *bot.Event, msg *bot.Message) (*bot.SendResult, error) {
	f.calls++
	f.msg = msg
	f.target = bot.TargetFromEvent(ev)
	if f.err != nil {
		return nil, f.err
	}
	return &bot.SendResult{MessageID: "m1"}, nil
}

func (f *fakeAPI) Logger() *slog.Logger { return slog.Default() }

func (f *fakeAPI) Storage() bot.Storage { return nil }

func TestBuilderSendsAccumulatedSegments(t *testing.T) {
	api := &fakeAPI{}
	ev := &bot.Event{
		Platform: "mock", BotID: "b1",
		Message: &bot.Message{ID: "in-1", Kind: bot.MessageGroup},
		Channel: &bot.Channel{ID: "g1", Kind: bot.MessageGroup},
		Sender:  &bot.User{ID: "u1"},
	}

	b := New(api, ev)
	// 空参数不应产生段。
	b.Text("").Image("").At("").Card(nil)
	if len(b.Segments()) != 0 {
		t.Fatalf("空参数产生的段: %+v", b.Segments())
	}
	if err := b.Text("hi ").At("u2").Markdown("**x**").Send(context.Background()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("Reply 调用次数 = %d, want 1", api.calls)
	}
	if api.msg.Kind != bot.MessageGroup {
		t.Fatalf("Kind = %q, want group", api.msg.Kind)
	}
	if api.msg.ID != "" {
		t.Fatalf("Message.ID = %q, want 空（待发消息不携带入站消息 ID）", api.msg.ID)
	}
	if len(api.msg.Segments) != 4 {
		t.Fatalf("段数 = %d, want 4（自动引用 + 3 个内容段）", len(api.msg.Segments))
	}
	quote := api.msg.Segments[0]
	if quote.Type != bot.SegReply || quote.Data[bot.KeyMessageID] != "in-1" {
		t.Fatalf("首个段 = %+v, want SegReply(in-1)", quote)
	}
	if api.target.ChannelID != "g1" || api.target.BotID != "b1" {
		t.Fatalf("目标 = %+v", api.target)
	}
}

// TestBuilderAutoQuote 覆盖事件消息 ID 触发的自动引用：事件没有消息 ID 时
// 不追加引用段；有 ID 时每次 Send 都在首位插入一条引用段，且不写入已累积的
// 段列表（重复 Send 不累积）。
func TestBuilderAutoQuote(t *testing.T) {
	api := &fakeAPI{}

	noID := New(api, &bot.Event{Message: &bot.Message{Kind: bot.MessageGroup}})
	if err := noID.Text("x").Send(context.Background()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := len(api.msg.Segments); got != 1 {
		t.Fatalf("无消息 ID 时段数 = %d, want 1", got)
	}

	b := New(api, &bot.Event{Message: &bot.Message{ID: "m-7", Kind: bot.MessageGroup}})
	b.Text("hi")
	for i := 1; i <= 2; i++ {
		if err := b.Send(context.Background()); err != nil {
			t.Fatalf("第 %d 次 Send: %v", i, err)
		}
		if got := len(api.msg.Segments); got != 2 {
			t.Fatalf("第 %d 次 Send 段数 = %d, want 2", i, got)
		}
		quote := api.msg.Segments[0]
		if quote.Type != bot.SegReply || quote.Data[bot.KeyMessageID] != "m-7" {
			t.Fatalf("第 %d 次 Send 首个段 = %+v, want SegReply(m-7)", i, quote)
		}
	}
	if got := b.Segments(); len(got) != 1 || got[0].Type != bot.SegText {
		t.Fatalf("引用段不应写入已累积的段列表: %+v", got)
	}
}

func TestBuilderErrors(t *testing.T) {
	ev := &bot.Event{Message: &bot.Message{Kind: bot.MessagePrivate}}

	if err := New(nil, ev).Text("x").Send(context.Background()); err == nil {
		t.Fatal("未配置 BotAPI 时应返回错误")
	}
	if err := New(&fakeAPI{}, ev).Send(context.Background()); err == nil {
		t.Fatal("空消息应返回错误而不发送")
	}

	// 事件带消息 ID 也一样：引用段不算内容，不允许只发引用。
	emptyAPI := &fakeAPI{}
	withID := &bot.Event{Message: &bot.Message{ID: "m1", Kind: bot.MessagePrivate}}
	if err := New(emptyAPI, withID).Send(context.Background()); err == nil {
		t.Fatal("空消息（即使事件带消息 ID）应返回错误而不发送")
	}
	if emptyAPI.calls != 0 {
		t.Fatalf("空消息不应调用 Reply: %d", emptyAPI.calls)
	}

	want := errors.New("boom")
	api := &fakeAPI{err: want}
	if err := New(api, ev).Text("x").Send(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Send = %v, want %v", err, want)
	}
}

func TestBuilderDefaultsToPrivate(t *testing.T) {
	api := &fakeAPI{}
	b := New(api, &bot.Event{})
	if err := b.Text("x").Send(context.Background()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if api.msg.Kind != bot.MessagePrivate {
		t.Fatalf("Kind = %q, want private", api.msg.Kind)
	}
}
