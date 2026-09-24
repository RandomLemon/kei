package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/adapters/mock"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/engine"
	"github.com/RandomLemon/kei/pkg/bot"
)

// catalogProbePlugin 在 Setup 中捕获 PluginContext.Adapters，并把事件写入 seen。
type catalogProbePlugin struct {
	catalog chan bot.AdapterCatalog
	seen    chan *bot.Event
}

func newCatalogProbePlugin() *catalogProbePlugin {
	return &catalogProbePlugin{
		catalog: make(chan bot.AdapterCatalog, 1),
		seen:    make(chan *bot.Event, 4),
	}
}

func (p *catalogProbePlugin) Metadata() bot.Metadata {
	return bot.Metadata{Name: "catalog-probe", Version: "v1"}
}

func (p *catalogProbePlugin) Setup(ctx context.Context, reg bot.Registrar) error {
	pc, ok := bot.PluginContextFrom(ctx)
	if !ok {
		return errors.New("缺少 PluginContext")
	}
	select {
	case p.catalog <- pc.Adapters:
	default:
	}
	reg.OnAll(func(_ context.Context, e *bot.Event, _ bot.Reply) error {
		select {
		case p.seen <- e:
		default:
		}
		return nil
	}, bot.WithPriority(-100))
	return nil
}

func (p *catalogProbePlugin) Start(context.Context) error { return nil }
func (p *catalogProbePlugin) Stop(context.Context) error  { return nil }

// runEngineWith 启动引擎并在测试结束时优雅停止。
//
// Run 是异步的：插件 Setup（注册路由规则）与适配器启动都发生在其中，因此这里
// 等到适配器就绪再返回，保证调用方注入的事件一定已经能被路由。
func runEngineWith(t *testing.T, opts engine.Options) *engine.Engine {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	eng, err := engine.New(opts)
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

	for _, b := range opts.Adapters {
		waiter, ok := b.Adapter.(interface{ Started() bool })
		if !ok {
			continue
		}
		deadline := time.Now().Add(3 * time.Second)
		for !waiter.Started() {
			if time.Now().After(deadline) {
				t.Fatal("适配器未在超时内启动")
			}
			time.Sleep(time.Millisecond)
		}
	}
	return eng
}

// testBinding 构造一个带元信息的 mock 适配器绑定。
func testBinding(t *testing.T, botID string, external bool) (engine.AdapterBinding, *mock.Adapter) {
	t.Helper()
	ad := newMock(botID)
	return engine.AdapterBinding{
		BotID:   botID,
		Adapter: ad,
		Metadata: bot.AdapterMetadata{
			Name:        "mock",
			Version:     "v0.1.0",
			Platforms:   []string{"mock"},
			Permissions: []bot.Permission{bot.PermNetListen},
			Options:     []string{"platform", bot.OptListenAddr},
		},
		External: external,
	}, ad
}

func TestEngineAdapterCatalog(t *testing.T) {
	binding, ad := testBinding(t, "mock-main", true)
	plugin := newCatalogProbePlugin()

	eng := runEngineWith(t, engine.Options{
		Config:   mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}),
		Adapters: []engine.AdapterBinding{binding},
		Plugins:  []bot.Plugin{plugin},
	})

	infos := eng.Adapters()
	if len(infos) != 1 {
		t.Fatalf("Adapters() = %+v", infos)
	}
	info := infos[0]
	if info.BotID != "mock-main" || !info.External || info.Metadata.Name != "mock" {
		t.Fatalf("绑定信息 = %+v", info)
	}
	if got := strings.Join(info.Metadata.Platforms, ","); got != "mock" {
		t.Fatalf("平台 = %q", got)
	}

	// 快照隔离：调用方修改返回值不得影响引擎内部状态。
	info.Metadata.Platforms[0] = "mutated"
	info.Metadata.Options[0] = "mutated"
	again := eng.Adapters()
	if again[0].Metadata.Platforms[0] != "mock" || again[0].Metadata.Options[0] != "platform" {
		t.Fatalf("快照未隔离: %+v", again[0].Metadata)
	}

	select {
	case catalog := <-plugin.catalog:
		if catalog == nil {
			t.Fatal("插件上下文中缺少适配器目录")
		}
		got := catalog.Adapters()
		if len(got) != 1 || got[0].Metadata.Name != "mock" {
			t.Fatalf("插件看到的目录 = %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("插件 Setup 未注入适配器目录")
	}

	if ad.Name() != "mock" {
		t.Fatalf("适配器平台名 = %q", ad.Name())
	}
}

func TestEngineEmitFeedsEventBusAndPlugins(t *testing.T) {
	binding, _ := testBinding(t, "myim-main", true)
	plugin := newCatalogProbePlugin()

	eng := runEngineWith(t, engine.Options{
		Config:   mockConfig(config.BotConfig{Name: "myim-main", Adapter: "mock"}),
		Adapters: []engine.AdapterBinding{binding},
		Plugins:  []bot.Plugin{plugin},
	})

	before := eng.Bus().Stats().Published
	ev := &bot.Event{
		ID:       "ext-1",
		Type:     bot.EventMessage,
		Platform: "mock",
		BotID:    "myim-main",
		Sender:   &bot.User{ID: "u1"},
		Message:  &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hello"}}}},
	}
	if err := eng.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := eng.Bus().Stats().Published; got != before+1 {
		t.Fatalf("Published = %d, want %d", got, before+1)
	}

	select {
	case got := <-plugin.seen:
		if got.ID != "ext-1" || got.Text() != "hello" {
			t.Fatalf("插件收到的事件 = %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("事件未送达插件")
	}
}
