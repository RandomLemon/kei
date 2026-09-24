package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/RandomLemon/kei/internal/adaptermgr"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/engine"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/plugins/echo"
)

// thirdPartyAdapter 模拟第三方进程内适配器：只依赖 pkg/bot，事件由适配器自行产生。
//
// 它就是「新增平台无需改动核心代码」的最小形态：注册元信息与工厂，核心按配置装配。
type thirdPartyAdapter struct {
	sink    bot.EventSink
	started chan struct{}
	sent    chan *bot.SendRequest
}

func (a *thirdPartyAdapter) Name() string { return "thirdparty-demo" }

func (a *thirdPartyAdapter) Capabilities() bot.Capabilities {
	return bot.Capabilities{Text: true, Private: true, Group: true}
}

func (a *thirdPartyAdapter) Start(ctx context.Context, sink bot.EventSink) error {
	if sink == nil {
		return errors.New("thirdparty-demo: nil sink")
	}
	a.sink = sink
	close(a.started)
	<-ctx.Done()
	return nil
}

func (a *thirdPartyAdapter) Stop(context.Context) error { return nil }

func (a *thirdPartyAdapter) Send(_ context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	select {
	case a.sent <- req:
	default:
	}
	return &bot.SendResult{MessageID: "third-1"}, nil
}

// inject 模拟平台推来一条文本消息，经 EventSink 投递到事件总线。
func (a *thirdPartyAdapter) inject(t *testing.T, text string) {
	t.Helper()
	ev := &bot.Event{
		ID:       "third-" + text,
		Type:     bot.EventMessage,
		Platform: "thirdparty-demo",
		BotID:    "demo-main",
		Time:     time.Now().UTC(),
		Sender:   &bot.User{ID: "u1", Name: "Third User"},
		Message: &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
		}},
	}
	if err := a.sink.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
}

func TestThirdPartyAdapterEndToEnd(t *testing.T) {
	const (
		adapterName = "thirdparty-demo"
		platform    = "thirdparty-demo"
	)
	ad := &thirdPartyAdapter{started: make(chan struct{}), sent: make(chan *bot.SendRequest, 4)}

	// 第三方适配器的全部接入成本：注册（真实场景在 init() 中完成）+ 一段配置。
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        adapterName,
		Version:     "v0.0.1",
		Author:      "third-party",
		Description: "第三方适配器接入验收",
		Platforms:   []string{platform},
		Permissions: []bot.Permission{bot.PermNetListen},
		Options:     []string{bot.OptListenAddr},
	}, func(ac bot.AdapterContext) (bot.Adapter, error) {
		if ac.BotID != "demo-main" {
			return nil, errors.New("thirdparty-demo: 非预期的 bot id")
		}
		return ad, nil
	})

	cfg := &config.Config{
		Bots: []config.BotConfig{{
			Name:     "demo-main",
			Adapter:  adapterName,
			Settings: map[string]any{bot.OptListenAddr: "127.0.0.1:0"},
		}},
		Plugins: map[string]config.PluginConfig{},
	}

	logger, _ := testLogger()
	bindings, err := adaptermgr.Build(context.Background(), cfg, adaptermgr.Deps{
		Logger:     logger,
		Storage:    storage.NewMemory(),
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("adaptermgr.Build: %v", err)
	}
	t.Cleanup(func() { _ = bindings.Close() })

	list := bindings.List()
	if len(list) != 1 {
		t.Fatalf("绑定 = %+v", list)
	}
	eng, err := engine.New(engine.Options{
		Config: cfg,
		Logger: logger,
		Adapters: []engine.AdapterBinding{{
			BotID:    list[0].BotID,
			Adapter:  list[0].Adapter,
			Metadata: list[0].Info.Metadata,
			External: list[0].Info.External,
		}},
		Plugins: []bot.Plugin{&echo.Plugin{}},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
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
	case <-ad.started:
	case <-time.After(3 * time.Second):
		t.Fatal("第三方适配器未启动")
	}

	ad.inject(t, "/echo third party")

	select {
	case req := <-ad.sent:
		if got := req.Message.PlainText(); got != "third party" {
			t.Fatalf("回复内容 = %q, want %q", got, "third party")
		}
		if req.BotID != "demo-main" || req.Target.Platform != platform {
			t.Fatalf("回复目标 = %+v", req.Target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未在超时内收到回复")
	}

	infos := eng.Adapters()
	if len(infos) != 1 || infos[0].Metadata.Name != adapterName || infos[0].External {
		t.Fatalf("适配器目录 = %+v", infos)
	}
}
