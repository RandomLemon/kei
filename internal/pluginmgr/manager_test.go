package pluginmgr

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

type fakePlugin struct {
	name     string
	perms    []bot.Permission
	calls    *[]string
	failOn   string
	panicOn  string
	lastCtx  bot.PluginContext
	setupErr error
}

func (p *fakePlugin) Metadata() bot.Metadata {
	return bot.Metadata{Name: p.name, Version: "v1", Permissions: p.perms}
}

func (p *fakePlugin) record(phase string) {
	if p.calls != nil {
		*p.calls = append(*p.calls, p.name+":"+phase)
	}
}

func (p *fakePlugin) Setup(ctx context.Context, _ bot.Registrar) error {
	p.record("setup")
	if p.panicOn == "setup" {
		panic("setup boom")
	}
	if pc, ok := bot.PluginContextFrom(ctx); ok {
		p.lastCtx = pc
	}
	if p.failOn == "setup" {
		return errors.New("setup failed")
	}
	return p.setupErr
}

func (p *fakePlugin) Start(context.Context) error {
	p.record("start")
	if p.panicOn == "start" {
		panic("start boom")
	}
	if p.failOn == "start" {
		return errors.New("start failed")
	}
	return nil
}

func (p *fakePlugin) Stop(context.Context) error {
	p.record("stop")
	if p.failOn == "stop" {
		return errors.New("stop failed")
	}
	return nil
}

func newManager(t *testing.T, plugins ...bot.Plugin) (*Manager, *storage.Memory) {
	t.Helper()
	store := storage.NewMemory()
	t.Cleanup(func() { _ = store.Close() })

	mgr := New(Deps{
		Storage:    store,
		HTTPClient: &http.Client{Timeout: time.Second},
		Logger:     slog.New(slog.DiscardHandler),
		Configs:    map[string]*bot.Config{"cfg": bot.NewConfig(map[string]any{"k": "v"})},
		API: func(name string, meta bot.Metadata) bot.BotAPI {
			return &stubAPI{name: name, store: store}
		},
		Catalog: func() []bot.Metadata { return nil },
	})
	for _, p := range plugins {
		if err := mgr.Add(p); err != nil {
			t.Fatalf("Add(%s): %v", p.Metadata().Name, err)
		}
	}
	return mgr, store
}

type stubAPI struct {
	name  string
	store bot.Storage
}

func (s *stubAPI) Send(context.Context, bot.Target, *bot.Message) (*bot.SendResult, error) {
	return &bot.SendResult{}, nil
}
func (s *stubAPI) Reply(context.Context, *bot.Event, *bot.Message) (*bot.SendResult, error) {
	return &bot.SendResult{}, nil
}
func (s *stubAPI) Logger() *slog.Logger { return slog.New(slog.DiscardHandler) }
func (s *stubAPI) Storage() bot.Storage { return s.store }

func TestLifecycleOrderAndReverseStop(t *testing.T) {
	var calls []string
	a := &fakePlugin{name: "a", calls: &calls}
	b := &fakePlugin{name: "b", calls: &calls}
	mgr, _ := newManager(t, a, b)

	ctx := context.Background()
	if err := mgr.Setup(ctx, func(string) bot.Registrar { return bot.NewRecordingRegistrar() }); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := mgr.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := mgr.Stop(ctx); err != nil {
		t.Fatalf("重复 Stop 应幂等: %v", err)
	}

	want := []string{"a:setup", "b:setup", "a:start", "b:start", "b:stop", "a:stop"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("生命周期顺序 = %v, want %v", calls, want)
	}
}

func TestDuplicateAndInvalidPlugins(t *testing.T) {
	mgr, _ := newManager(t)
	if err := mgr.Add(&fakePlugin{name: "dup"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := mgr.Add(&fakePlugin{name: "dup"}); err == nil {
		t.Fatal("重名插件应返回错误")
	}
	if err := mgr.Add(nil); err == nil {
		t.Fatal("nil 插件应返回错误")
	}
	if err := mgr.Add(&fakePlugin{}); err == nil {
		t.Fatal("空名字插件应返回错误")
	}
}

func TestSetupAndStartFailureHandling(t *testing.T) {
	t.Run("Setup 失败阻断启动", func(t *testing.T) {
		var calls []string
		mgr, _ := newManager(t, &fakePlugin{name: "bad", calls: &calls, failOn: "setup"})
		err := mgr.Setup(context.Background(), func(string) bot.Registrar { return bot.NewRecordingRegistrar() })
		if err == nil || !strings.Contains(err.Error(), "setup failed") {
			t.Fatalf("Setup 错误 = %v", err)
		}
	})

	t.Run("Setup panic 被隔离为错误", func(t *testing.T) {
		mgr, _ := newManager(t, &fakePlugin{name: "boom", panicOn: "setup"})
		err := mgr.Setup(context.Background(), func(string) bot.Registrar { return bot.NewRecordingRegistrar() })
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Fatalf("Setup panic 错误 = %v", err)
		}
	})

	t.Run("Start 失败时回滚已启动插件", func(t *testing.T) {
		var calls []string
		mgr, _ := newManager(t,
			&fakePlugin{name: "ok", calls: &calls},
			&fakePlugin{name: "bad", calls: &calls, failOn: "start"},
		)
		ctx := context.Background()
		if err := mgr.Setup(ctx, func(string) bot.Registrar { return bot.NewRecordingRegistrar() }); err != nil {
			t.Fatalf("Setup: %v", err)
		}
		if err := mgr.Start(ctx); err == nil {
			t.Fatal("Start 应返回错误")
		}
		want := []string{"ok:setup", "bad:setup", "ok:start", "bad:start", "ok:stop"}
		if strings.Join(calls, ",") != strings.Join(want, ",") {
			t.Fatalf("回滚顺序 = %v, want %v", calls, want)
		}
	})
}

func TestPermissionScopedContext(t *testing.T) {
	tests := []struct {
		name         string
		perms        []bot.Permission
		wantHTTP     bool
		wantStorage  bool
		configurable bool
	}{
		{name: "无权限", perms: nil, wantHTTP: false, wantStorage: false},
		{name: "仅有网络权限", perms: []bot.Permission{bot.PermNetwork}, wantHTTP: true, wantStorage: false},
		{name: "仅有存储权限", perms: []bot.Permission{bot.PermStorage}, wantHTTP: false, wantStorage: true},
		{name: "全部权限", perms: []bot.Permission{bot.PermAll}, wantHTTP: true, wantStorage: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePlugin{name: "p", perms: tc.perms}
			mgr, store := newManager(t, p)
			if err := mgr.Setup(context.Background(), func(string) bot.Registrar { return bot.NewRecordingRegistrar() }); err != nil {
				t.Fatalf("Setup: %v", err)
			}

			if got := p.lastCtx.HTTPClient != nil; got != tc.wantHTTP {
				t.Fatalf("HTTPClient 存在 = %v, want %v", got, tc.wantHTTP)
			}
			err := p.lastCtx.Storage.Set(context.Background(), "k", []byte("v"), time.Minute)
			if tc.wantStorage {
				if err != nil {
					t.Fatalf("有存储权限时 Set 失败: %v", err)
				}
				if _, err := store.Get(context.Background(), "k"); err != nil {
					t.Fatalf("存储未生效: %v", err)
				}
			} else if !errors.Is(err, storage.ErrPermissionDenied) {
				t.Fatalf("无存储权限时 Set = %v, want ErrPermissionDenied", err)
			}
			if p.lastCtx.Name != "p" || p.lastCtx.Logger == nil || p.lastCtx.Bot == nil || p.lastCtx.Catalog == nil {
				t.Fatalf("PluginContext 不完整: %+v", p.lastCtx)
			}
		})
	}
}

func TestPluginCatalog(t *testing.T) {
	mgr, _ := newManager(t, &fakePlugin{name: "one"}, &fakePlugin{name: "two"})

	metas := mgr.Plugins()
	if len(metas) != 2 || metas[0].Name != "one" || metas[1].Name != "two" {
		t.Fatalf("Plugins() = %+v", metas)
	}
	metas[0].Name = "mutated"
	if got, _ := mgr.Metadata("one"); got.Name != "one" {
		t.Fatal("Plugins() 必须返回快照")
	}
	if _, ok := mgr.Metadata("missing"); ok {
		t.Fatal("Metadata 不应命中缺失插件")
	}
	if mgr.Len() != 2 {
		t.Fatalf("Len = %d, want 2", mgr.Len())
	}
}

func TestSetupRequiresRegistrarFactory(t *testing.T) {
	mgr, _ := newManager(t, &fakePlugin{name: "p"})
	if err := mgr.Setup(context.Background(), nil); err == nil {
		t.Fatal("regFor 为 nil 时应返回错误")
	}
	if err := mgr.Setup(context.Background(), func(string) bot.Registrar { return nil }); err == nil {
		t.Fatal("注册器为 nil 时应返回错误")
	}
}
