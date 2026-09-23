package echo

import (
	"context"
	"testing"

	"github.com/RandomLemon/kei/pkg/bot"
)

func setup(t *testing.T) (*bot.RecordingRegistrar, *bot.Rule) {
	t.Helper()
	reg := bot.NewRecordingRegistrar()
	if err := (&Plugin{}).Setup(context.Background(), reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	rule, ok := reg.Command("echo")
	if !ok {
		t.Fatal("未注册 echo 命令")
	}
	return reg, rule
}

func TestEchoRepliesWithJoinedArgs(t *testing.T) {
	_, rule := setup(t)

	ev := &bot.Event{
		Type:    bot.EventMessage,
		Command: &bot.Command{Name: "echo", Args: []string{"hello", "world"}},
		Message: &bot.Message{Kind: bot.MessageGroup},
	}
	reply := bot.NewNoopReply()
	if err := rule.Handler(context.Background(), ev, reply); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if got := reply.PlainText(); got != "hello world" {
		t.Fatalf("回复 = %q, want %q", got, "hello world")
	}
}

func TestEchoWithoutArgsShowsUsage(t *testing.T) {
	_, rule := setup(t)

	ev := &bot.Event{Type: bot.EventMessage, Command: &bot.Command{Name: "echo"}, Message: &bot.Message{Kind: bot.MessagePrivate}}
	reply := bot.NewNoopReply()
	if err := rule.Handler(context.Background(), ev, reply); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if got := reply.PlainText(); got == "" {
		t.Fatal("无参数时应返回用法提示")
	}
}

func TestMetadataAndLifecycle(t *testing.T) {
	p := &Plugin{}
	meta := p.Metadata()
	if meta.Name != "echo" || meta.Version == "" {
		t.Fatalf("Metadata = %+v", meta)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
