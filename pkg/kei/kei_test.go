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

	// 命令前缀用例单独注册一个适配器，工厂每次装配返回新实例（见 prefixAdapter）。
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        "embedprefix",
		Version:     "v0.0.1",
		Description: "pkg/kei 命令前缀端到端测试适配器",
		Platforms:   []string{"embedprefix"},
	}, func(bot.AdapterContext) (bot.Adapter, error) {
		a := &prefixAdapter{started: make(chan struct{}), sent: make(chan *bot.SendRequest, 4)}
		prefixNew <- a
		return a, nil
	})
}

// prefixAdapter 是命令前缀端到端用例用的最小适配器，工厂每次装配返回新实例。
//
// 不能复用包级单例 embedInstance：它的 started channel 只能 close 一次，第二个
// 端到端 Run 用例会 panic（close of closed channel）。
type prefixAdapter struct {
	sink    bot.EventSink
	started chan struct{}
	sent    chan *bot.SendRequest
}

func (a *prefixAdapter) Name() string { return "embedprefix" }

func (a *prefixAdapter) Capabilities() bot.Capabilities {
	return bot.Capabilities{Text: true, Private: true, Group: true}
}

func (a *prefixAdapter) Start(ctx context.Context, sink bot.EventSink) error {
	a.sink = sink
	close(a.started)
	<-ctx.Done()
	return nil
}

func (a *prefixAdapter) Stop(context.Context) error { return nil }

func (a *prefixAdapter) Send(_ context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	select {
	case a.sent <- req:
	default:
	}
	return &bot.SendResult{MessageID: "embedprefix-1"}, nil
}

// prefixNew 接收工厂产出的新实例，供命令前缀用例注入事件。
var prefixNew = make(chan *prefixAdapter, 4)

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

// TestRunEndToEndCommandPrefixes 断言配置 engine.command_prefixes 真的透传到引擎：
// 注入 "!hi" 应收到回复，而默认前缀 "/hi" 不再命中。
func TestRunEndToEndCommandPrefixes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: []byte("bots:\n  - name: prefix-main\n    adapter: embedprefix\nengine:\n  command_prefixes: [\"!\"]\n"),
			Plugins: []bot.Plugin{&bot.FuncPlugin{
				Meta: bot.Metadata{Name: "inline-prefix", Version: "v0.0.1"},
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

	var adapter *prefixAdapter
	select {
	case adapter = <-prefixNew:
	case <-time.After(3 * time.Second):
		t.Fatal("适配器未装配")
	}
	select {
	case <-adapter.started:
	case <-time.After(3 * time.Second):
		t.Fatal("适配器未启动")
	}

	emit := func(id, text string) {
		ev := &bot.Event{
			ID:       id,
			Type:     bot.EventMessage,
			Platform: "embedprefix",
			BotID:    "prefix-main",
			Time:     time.Now().UTC(),
			Sender:   &bot.User{ID: "u1", Name: "Prefix User"},
			Message: &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
				{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
			}},
		}
		if err := adapter.sink.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit(%q): %v", text, err)
		}
	}

	emit("e2e-prefix-1", "!hi")
	select {
	case req := <-adapter.sent:
		if got := req.Message.PlainText(); got != "pong" {
			t.Fatalf("回复内容 = %q, want pong", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("配置的前缀 ! 未生效：未在超时内收到回复")
	}

	// 默认前缀 "/" 必须失效：短窗口内不应有任何回复。
	emit("e2e-prefix-2", "/hi")
	select {
	case req := <-adapter.sent:
		t.Fatalf("默认前缀 / 不应命中，却收到 %q", req.Message.PlainText())
	case <-time.After(500 * time.Millisecond):
	}
}
