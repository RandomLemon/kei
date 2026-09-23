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
	if api.msg.ID != "in-1" {
		t.Fatalf("Message.ID = %q, want in-1", api.msg.ID)
	}
	if len(api.msg.Segments) != 3 {
		t.Fatalf("段数 = %d, want 3", len(api.msg.Segments))
	}
	if api.target.ChannelID != "g1" || api.target.BotID != "b1" {
		t.Fatalf("目标 = %+v", api.target)
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
