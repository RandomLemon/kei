package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/engine"
	"github.com/RandomLemon/kei/pkg/bot"
)

// TestShutdownIncompleteSkipsPluginStop 验证总线未排空时引擎返回
// ErrShutdownIncomplete，且不停止插件/不关闭自有存储（避免与在途 handler 竞态）。
func TestShutdownIncompleteSkipsPluginStop(t *testing.T) {
	ad := newMock("mock-main")
	entered := make(chan struct{})
	handlerDone := make(chan struct{})
	release := make(chan struct{})
	var (
		enterOnce sync.Once
		relOnce   sync.Once
	)
	unblock := func() { relOnce.Do(func() { close(release) }) }
	defer unblock()

	stopCalled := make(chan struct{}, 1)
	plugin := &bot.FuncPlugin{
		Meta: bot.Metadata{Name: "blocker"},
		OnSetup: func(_ context.Context, reg bot.Registrar) error {
			reg.OnAll(func(context.Context, *bot.Event, bot.Reply) error {
				enterOnce.Do(func() { close(entered) })
				<-release
				close(handlerDone)
				return nil
			})
			return nil
		},
		OnStop: func(context.Context) error {
			stopCalled <- struct{}{}
			return nil
		},
	}

	eng, err := engine.New(engine.Options{
		Config:          mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}),
		Logger:          slog.New(slog.DiscardHandler),
		Adapters:        []engine.AdapterBinding{{BotID: "mock-main", Adapter: ad}},
		Plugins:         []bot.Plugin{plugin},
		ShutdownTimeout: 20 * time.Millisecond,
		EventTimeout:    time.Hour,
		RuleTimeout:     time.Hour,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for !ad.Started() {
		if time.Now().After(deadline) {
			t.Fatal("适配器未启动")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := ad.InjectText(ctx, "block"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Handler 未开始执行")
	}
	cancel()
	select {
	case runErr := <-done:
		if !errors.Is(runErr, engine.ErrShutdownIncomplete) {
			t.Fatalf("Run 错误 = %v, want ErrShutdownIncomplete", runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未在超时内退出")
	}
	select {
	case <-stopCalled:
		t.Fatal("总线未排空时不应停止插件")
	default:
	}

	unblock()
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Handler 未退出")
	}
}
