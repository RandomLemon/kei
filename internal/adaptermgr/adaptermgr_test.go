package adaptermgr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

// recordingAdapter 记录构造它的 AdapterContext，便于断言权限裁剪结果。
type recordingAdapter struct {
	name string
	ac   bot.AdapterContext
}

func (a *recordingAdapter) Name() string                               { return a.name }
func (a *recordingAdapter) Start(context.Context, bot.EventSink) error { return nil }
func (a *recordingAdapter) Stop(context.Context) error                 { return nil }
func (a *recordingAdapter) Capabilities() bot.Capabilities             { return bot.Capabilities{Text: true} }

func (a *recordingAdapter) Send(context.Context, *bot.SendRequest) (*bot.SendResult, error) {
	return &bot.SendResult{MessageID: "ok"}, nil
}

// registerTestAdapter 注册一个测试适配器，名字必须全局唯一（注册表是进程级的）。
func registerTestAdapter(t *testing.T, meta bot.AdapterMetadata) *recordingAdapter {
	t.Helper()
	ad := &recordingAdapter{name: "recording"}
	bot.RegisterAdapter(meta, func(ac bot.AdapterContext) (bot.Adapter, error) {
		ad.ac = ac
		return ad, nil
	})
	return ad
}

func testDeps(storageImpl bot.Storage, httpClient *http.Client, logger *slog.Logger) Deps {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return Deps{Logger: logger, Storage: storageImpl, HTTPClient: httpClient}
}

func TestValidateRegistryRules(t *testing.T) {
	tests := []struct {
		name  string
		metas []bot.AdapterMetadata
		want  string
	}{
		{
			name:  "重名",
			metas: []bot.AdapterMetadata{{Name: "dup", Platforms: []string{"p"}}, {Name: "dup", Platforms: []string{"p"}}},
			want:  "注册名重复",
		},
		{
			name:  "缺 Platforms",
			metas: []bot.AdapterMetadata{{Name: "noplat"}},
			want:  "未声明 Platforms",
		},
		{
			name:  "声明 PermAll",
			metas: []bot.AdapterMetadata{{Name: "all", Platforms: []string{"p"}, Permissions: []bot.Permission{bot.PermAll}}},
			want:  "不得声明",
		},
		{
			name:  "合法",
			metas: []bot.AdapterMetadata{{Name: "ok", Platforms: []string{"p"}, Permissions: []bot.Permission{bot.PermNetwork}}},
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRegistryOf(tc.metas)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("不应报错: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v, 期望包含 %q", err, tc.want)
			}
		})
	}
}

func TestValidateUnknownAdapter(t *testing.T) {
	cfg := &config.Config{Bots: []config.BotConfig{{Name: "b1", Adapter: "definitely-missing"}}}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("未知适配器应报错")
	}
	for _, want := range []string{"definitely-missing", "已注册"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误 = %v, 期望包含 %q", err, want)
		}
	}
}

func TestValidateReservedOptionRequiresNetListen(t *testing.T) {
	const name = "test-reserved-option"
	registerTestAdapter(t, bot.AdapterMetadata{
		Name:        name,
		Platforms:   []string{"p"},
		Permissions: []bot.Permission{bot.PermNetwork},
		Options:     []string{bot.OptListenAddr},
	})

	cfg := &config.Config{Bots: []config.BotConfig{{
		Name: "b1", Adapter: name, Settings: map[string]any{bot.OptListenAddr: "127.0.0.1:0"},
	}}}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), string(bot.PermNetListen)) {
		t.Fatalf("未声明 net_listen 却配置 listen_addr 应报错: %v", err)
	}

	// 空值不算配置，不触发保留键校验。
	cfg.Bots[0].Settings[bot.OptListenAddr] = ""
	if err := Validate(cfg); err != nil {
		t.Fatalf("空 listen_addr 不应报错: %v", err)
	}
}

func TestBuildTrimsDependencies(t *testing.T) {
	const (
		bare = "test-trim-bare"
		full = "test-trim-full"
	)
	bareAd := registerTestAdapter(t, bot.AdapterMetadata{Name: bare, Platforms: []string{"recording"}})
	fullAd := registerTestAdapter(t, bot.AdapterMetadata{
		Name: full, Platforms: []string{"recording"},
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermStorage},
	})

	mem := storage.NewMemory()
	httpClient := &http.Client{}
	cfg := &config.Config{Bots: []config.BotConfig{
		{Name: "bare-bot", Adapter: bare},
		{Name: "full-bot", Adapter: full},
	}}
	bindings, err := Build(context.Background(), cfg, testDeps(mem, httpClient, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = bindings.Close() })

	if bareAd.ac.HTTPClient != nil {
		t.Fatal("未声明 network 时 HTTPClient 应为 nil")
	}
	if _, err := bareAd.ac.Storage.Get(context.Background(), "k"); !errors.Is(err, storage.ErrPermissionDenied) {
		t.Fatalf("未声明 storage 时应注入拒绝式存储, err = %v", err)
	}
	if bareAd.ac.BotID != "bare-bot" || bareAd.ac.Logger == nil || bareAd.ac.Config == nil {
		t.Fatalf("AdapterContext = %+v", bareAd.ac)
	}

	if fullAd.ac.HTTPClient != httpClient {
		t.Fatal("声明 network 时应注入 HTTPClient")
	}
	if err := fullAd.ac.Storage.Set(context.Background(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("声明 storage 时应可写: %v", err)
	}

	list := bindings.List()
	if len(list) != 2 || list[0].BotID != "bare-bot" || list[1].BotID != "full-bot" {
		t.Fatalf("绑定顺序 = %+v", list)
	}
	if list[0].Info.External || list[0].Info.Metadata.Name != bare {
		t.Fatalf("绑定信息 = %+v", list[0].Info)
	}
}

func TestBuildWarnsUnknownOptions(t *testing.T) {
	const name = "test-warn-options"
	registerTestAdapter(t, bot.AdapterMetadata{
		Name: name, Platforms: []string{"recording"}, Options: []string{"known"},
	})

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{Bots: []config.BotConfig{{
		Name: "b1", Adapter: name, Settings: map[string]any{"known": 1, "typo_key": 2},
	}}}
	bindings, err := Build(context.Background(), cfg, testDeps(storage.NewMemory(), nil, logger))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = bindings.Close() })

	got := buf.String()
	if !strings.Contains(got, "typo_key") {
		t.Fatalf("未知配置键未告警: %s", got)
	}
	if strings.Contains(got, `key=known`) {
		t.Fatalf("已知配置键不应告警: %s", got)
	}
}

func TestBuildFactoryFailures(t *testing.T) {
	const name = "test-bad-factory"
	bot.RegisterAdapter(bot.AdapterMetadata{Name: name, Platforms: []string{"p"}},
		func(bot.AdapterContext) (bot.Adapter, error) { return nil, errors.New("boom") })

	cfg := &config.Config{Bots: []config.BotConfig{{Name: "b1", Adapter: name}}}
	_, err := Build(context.Background(), cfg, testDeps(storage.NewMemory(), nil, nil))
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "b1") {
		t.Fatalf("构造失败错误 = %v", err)
	}

	const nilName = "test-nil-adapter"
	bot.RegisterAdapter(bot.AdapterMetadata{Name: nilName, Platforms: []string{"p"}},
		func(bot.AdapterContext) (bot.Adapter, error) { return nil, nil })
	cfg.Bots[0].Adapter = nilName
	if _, err := Build(context.Background(), cfg, testDeps(storage.NewMemory(), nil, nil)); err == nil ||
		!strings.Contains(err.Error(), "nil") {
		t.Fatalf("工厂返回 nil 应报错: %v", err)
	}
}

func TestBuildWarnsUndeclaredPlatform(t *testing.T) {
	const name = "test-undeclared-platform"
	registerTestAdapter(t, bot.AdapterMetadata{Name: name, Platforms: []string{"declared"}})

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{Bots: []config.BotConfig{{Name: "b1", Adapter: name}}}
	bindings, err := Build(context.Background(), cfg, testDeps(storage.NewMemory(), nil, logger))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = bindings.Close() })

	if !strings.Contains(buf.String(), "未在元信息中声明") {
		t.Fatalf("未声明的平台名应告警: %s", buf.String())
	}
}

// fakeEmitter 是实现了 Emit 的最小 BotAPI。
type fakeEmitter struct {
	got *bot.Event
}

func (f *fakeEmitter) Send(context.Context, bot.Target, *bot.Message) (*bot.SendResult, error) {
	return nil, nil
}
func (f *fakeEmitter) Reply(context.Context, *bot.Event, *bot.Message) (*bot.SendResult, error) {
	return nil, nil
}
func (f *fakeEmitter) Logger() *slog.Logger { return slog.New(slog.DiscardHandler) }
func (f *fakeEmitter) Storage() bot.Storage { return storage.Denied() }
func (f *fakeEmitter) Emit(_ context.Context, ev *bot.Event) error {
	f.got = ev
	return nil
}

// plainAPI 是不提供 Emit 的 BotAPI。
type plainAPI struct{}

func (plainAPI) Send(context.Context, bot.Target, *bot.Message) (*bot.SendResult, error) {
	return nil, nil
}
func (plainAPI) Reply(context.Context, *bot.Event, *bot.Message) (*bot.SendResult, error) {
	return nil, nil
}
func (plainAPI) Logger() *slog.Logger { return slog.New(slog.DiscardHandler) }
func (plainAPI) Storage() bot.Storage { return storage.Denied() }

func TestEmitFuncChecksOwnership(t *testing.T) {
	bindings := &Bindings{
		log:      slog.New(slog.DiscardHandler),
		external: map[string][]string{"myim": {"myim-main"}},
	}

	api := &fakeEmitter{}
	emit, err := bindings.EmitFunc(api)
	if err != nil {
		t.Fatalf("EmitFunc: %v", err)
	}

	ev := &bot.Event{ID: "e1", Platform: "myim", BotID: "myim-main"}
	if err := emit(context.Background(), "myim", ev); err != nil {
		t.Fatalf("合法事件被拒: %v", err)
	}
	if api.got != ev {
		t.Fatalf("事件未透传: %+v", api.got)
	}

	if err := emit(context.Background(), "unknown", ev); err == nil {
		t.Fatal("未声明的适配器应被拒")
	}
	if err := emit(context.Background(), "myim", &bot.Event{ID: "e2", BotID: "other-bot"}); err == nil {
		t.Fatal("越权 bot 的事件应被拒")
	}
	if err := emit(context.Background(), "myim", nil); err == nil {
		t.Fatal("nil 事件应被拒")
	}

	if _, err := bindings.EmitFunc(plainAPI{}); err == nil {
		t.Fatal("BotAPI 未实现 Emit 时应报错")
	}
}

// countingCloser 记录 Close 调用次数。
type countingCloser struct{ n int }

func (c *countingCloser) Close() error {
	c.n++
	return nil
}

func TestBindingsCloseIsIdempotent(t *testing.T) {
	c := &countingCloser{}
	bindings := &Bindings{log: slog.New(slog.DiscardHandler), closers: []io.Closer{c}}
	if err := bindings.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := bindings.Close(); err != nil {
		t.Fatalf("第二次 Close: %v", err)
	}
	if c.n != 1 {
		t.Fatalf("Close 调用次数 = %d, want 1", c.n)
	}
}
