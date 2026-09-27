package kei

import (
	"context"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// embedAdapter 是门面端到端测试用的最小适配器：只依赖 pkg/bot。
type embedAdapter struct {
	sink    bot.EventSink
	started chan struct{}
	sent    chan *bot.SendRequest
}

func (a *embedAdapter) Name() string { return "embedtest" }

func (a *embedAdapter) Capabilities() bot.Capabilities {
	return bot.Capabilities{Text: true, Private: true, Group: true}
}

func (a *embedAdapter) Start(ctx context.Context, sink bot.EventSink) error {
	a.sink = sink
	close(a.started)
	<-ctx.Done()
	return nil
}

func (a *embedAdapter) Stop(context.Context) error { return nil }

func (a *embedAdapter) Send(_ context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	select {
	case a.sent <- req:
	default:
	}
	return &bot.SendResult{MessageID: "embed-1"}, nil
}

// embedInstance 是装配阶段返回的包级捕获实例，供测试注入事件与观察发送。
var embedInstance = &embedAdapter{started: make(chan struct{}), sent: make(chan *bot.SendRequest, 4)}

func init() {
	// 使用方接入第三方适配器的形态：注册（通常在 init 中）+ 一段配置。
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        "embedtest",
		Version:     "v0.0.1",
		Description: "pkg/kei 门面端到端测试适配器",
		Platforms:   []string{"embedtest"},
		Permissions: []bot.Permission{bot.PermNetListen},
		Options:     []string{bot.OptListenAddr},
	}, func(bot.AdapterContext) (bot.Adapter, error) { return embedInstance, nil })
}

// TestRunEndToEndWithInlinePlugin 用「import + 几行代码」的形态跑通全链路：
// 内联 FuncPlugin + 进程内适配器 + 一段内联 YAML，验证 Run 的装配与优雅退出。
func TestRunEndToEndWithInlinePlugin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: []byte("bots:\n  - name: embed-main\n    adapter: embedtest\n"),
			Plugins: []bot.Plugin{&bot.FuncPlugin{
				Meta: bot.Metadata{Name: "inline", Version: "v0.0.1"},
				OnSetup: func(_ context.Context, reg bot.Registrar) error {
					reg.OnCommand("hi", func(ctx context.Context, _ *bot.Event, r bot.Reply) error {
						return r.Text("pong").Send(ctx)
					})
					return nil
				},
			}},
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run 未在超时内退出")
		}
	})

	select {
	case <-embedInstance.started:
	case <-time.After(3 * time.Second):
		t.Fatal("适配器未启动")
	}

	ev := &bot.Event{
		ID:       "e2e-1",
		Type:     bot.EventMessage,
		Platform: "embedtest",
		BotID:    "embed-main",
		Time:     time.Now().UTC(),
		Sender:   &bot.User{ID: "u1", Name: "Embed User"},
		Message: &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: "/hi"}},
		}},
	}
	if err := embedInstance.sink.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	select {
	case req := <-embedInstance.sent:
		if got := req.Message.PlainText(); got != "pong" {
			t.Fatalf("回复内容 = %q, want %q", got, "pong")
		}
		if req.BotID != "embed-main" {
			t.Fatalf("回复 BotID = %q, want embed-main", req.BotID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未在超时内收到回复")
	}
}
