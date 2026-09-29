package adaptermgr

import (
	"bytes"
	"context"
	"errors"
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
	bindings, err := Build(cfg, testDeps(mem, httpClient, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

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
	if list[0].Info.Metadata.Name != bare {
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
	_, err := Build(cfg, testDeps(storage.NewMemory(), nil, logger))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

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
	_, err := Build(cfg, testDeps(storage.NewMemory(), nil, nil))
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "b1") {
		t.Fatalf("构造失败错误 = %v", err)
	}

	const nilName = "test-nil-adapter"
	bot.RegisterAdapter(bot.AdapterMetadata{Name: nilName, Platforms: []string{"p"}},
		func(bot.AdapterContext) (bot.Adapter, error) { return nil, nil })
	cfg.Bots[0].Adapter = nilName
	if _, err := Build(cfg, testDeps(storage.NewMemory(), nil, nil)); err == nil ||
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
	_, err := Build(cfg, testDeps(storage.NewMemory(), nil, logger))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if !strings.Contains(buf.String(), "未在元信息中声明") {
		t.Fatalf("未声明的平台名应告警: %s", buf.String())
	}
}

// TestBuildSkipsDisabledAdapter 覆盖 enabled: false 的装配语义：
// 被禁用的适配器不装配，其 bot 一并跳过，其他 bot 不受影响。
func TestBuildSkipsDisabledAdapter(t *testing.T) {
	const inproc = "test-enabled-inproc"
	registerTestAdapter(t, bot.AdapterMetadata{Name: inproc, Platforms: []string{"recording"}})

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// go.mod 的 go 指令低于 1.26，这里用临时变量取地址。
	disabled := false
	cfg := &config.Config{
		Bots: []config.BotConfig{
			{Name: "inproc-bot", Adapter: inproc},
			{Name: "off-bot", Adapter: "test-disabled-adapter"},
		},
		Adapters: map[string]config.AdapterConfig{
			"test-disabled-adapter": {Enabled: &disabled},
		},
	}

	bindings, err := Build(cfg, testDeps(storage.NewMemory(), nil, logger))
	if err != nil {
		t.Fatalf("禁用的适配器不应阻塞装配: %v", err)
	}

	list := bindings.List()
	if len(list) != 1 || list[0].BotID != "inproc-bot" {
		t.Fatalf("绑定 = %+v", list)
	}
	if got := buf.String(); !strings.Contains(got, "off-bot") || !strings.Contains(got, "已禁用") {
		t.Fatalf("应记录被跳过的 bot 与禁用状态: %s", got)
	}
}

// TestBuildSkipsDisabledInProcessAdapter 覆盖进程内适配器被禁用的情况：
// adapters 段只写 enabled 即可禁用。
func TestBuildSkipsDisabledInProcessAdapter(t *testing.T) {
	const name = "test-disabled-inproc"
	registerTestAdapter(t, bot.AdapterMetadata{Name: name, Platforms: []string{"recording"}})

	logger := slog.New(slog.DiscardHandler)
	disabled, enabled := false, true
	cfg := &config.Config{
		Bots:     []config.BotConfig{{Name: "b1", Adapter: name}},
		Adapters: map[string]config.AdapterConfig{name: {Enabled: &disabled}},
	}

	bindings, err := Build(cfg, testDeps(storage.NewMemory(), nil, logger))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := bindings.List(); len(got) != 0 {
		t.Fatalf("禁用的进程内适配器不应产生绑定: %+v", got)
	}

	// 启用后立即可用，说明禁用只是跳过而非破坏配置。
	cfg.Adapters[name] = config.AdapterConfig{Enabled: &enabled}
	bindings, err = Build(cfg, testDeps(storage.NewMemory(), nil, logger))
	if err != nil {
		t.Fatalf("启用后 Build: %v", err)
	}
	if got := bindings.List(); len(got) != 1 || got[0].BotID != "b1" {
		t.Fatalf("启用后绑定 = %+v", got)
	}
}

// TestValidateSkipsDisabledUnknownAdapter 覆盖「禁用即不校验」：
// 禁用条目即使名字不存在也不报错，启用后必须报错。
func TestValidateSkipsDisabledUnknownAdapter(t *testing.T) {
	disabled, enabled := false, true
	cfg := &config.Config{
		Bots:     []config.BotConfig{{Name: "ghost-bot", Adapter: "test-ghost-adapter"}},
		Adapters: map[string]config.AdapterConfig{"test-ghost-adapter": {Enabled: &disabled}},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("禁用的未知适配器不应报错: %v", err)
	}
	_, err := Build(cfg, testDeps(storage.NewMemory(), nil, nil))
	if err != nil {
		t.Fatalf("禁用的未知适配器不应阻塞装配: %v", err)
	}

	cfg.Adapters["test-ghost-adapter"] = config.AdapterConfig{Enabled: &enabled}
	err = Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "test-ghost-adapter") {
		t.Fatalf("启用后的未知适配器应报错: %v", err)
	}
}

// TestBuildWarnsUnregisteredDeclaration 覆盖进程内声明拼错名字的情况。
func TestBuildWarnsUnregisteredDeclaration(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	const botAdapter = "test-decl-bot-adapter"
	registerTestAdapter(t, bot.AdapterMetadata{Name: botAdapter, Platforms: []string{"recording"}})

	enabled := true
	cfg := &config.Config{
		Bots:     []config.BotConfig{{Name: "b1", Adapter: botAdapter}},
		Adapters: map[string]config.AdapterConfig{"test-unregistered-decl": {Enabled: &enabled}},
	}
	_, err := Build(cfg, testDeps(storage.NewMemory(), nil, logger))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(buf.String(), "未注册") {
		t.Fatalf("未注册的声明应告警: %s", buf.String())
	}
}

func TestBuildSkipsDisabledBot(t *testing.T) {
	const (
		adapter = "test-bot-enabled"
		meta    = "test-bot-enabled-ghost"
	)
	ad := registerTestAdapter(t, bot.AdapterMetadata{
		Name: adapter, Platforms: []string{"recording"}, Options: []string{"listen_addr"},
	})

	// go.mod 的 go 指令低于 1.26，这里用临时变量取地址。
	disabled := false
	cfg := &config.Config{Bots: []config.BotConfig{
		{Name: "live-bot", Adapter: adapter},
		{Name: "off-bot", Adapter: adapter, Enabled: &disabled},
	}}

	bindings, err := Build(cfg, testDeps(storage.NewMemory(), nil, nil))
	if err != nil {
		t.Fatalf("停用的实例不应阻塞装配: %v", err)
	}

	list := bindings.List()
	if len(list) != 1 || list[0].BotID != "live-bot" {
		t.Fatalf("停用的实例不应出现在绑定中: %+v", list)
	}
	if ad.ac.BotID != "live-bot" {
		t.Fatalf("工厂只应为启用的实例调用: %+v", ad.ac)
	}

	// 停用的实例不参与适配器校验：未知适配器也不应报错。
	cfg.Bots = append(cfg.Bots, config.BotConfig{Name: "ghost-bot", Adapter: meta, Enabled: &disabled})
	if err := Validate(cfg); err != nil {
		t.Fatalf("停用实例的适配器不应参与校验: %v", err)
	}
}

func TestValidateDisabledBotSkipsReservedOption(t *testing.T) {
	const adapter = "test-bot-enabled-reserved"
	registerTestAdapter(t, bot.AdapterMetadata{
		Name: adapter, Platforms: []string{"recording"}, Permissions: []bot.Permission{bot.PermNetwork},
	})

	// 未声明 net_listen：启用时报错，停用后跳过。
	botCfg := config.BotConfig{
		Name: "off-bot", Adapter: adapter,
		Settings: map[string]any{bot.OptListenAddr: "127.0.0.1:0"},
	}
	cfg := &config.Config{Bots: []config.BotConfig{botCfg}}
	if err := Validate(cfg); err == nil {
		t.Fatal("启用时配置保留键应报错")
	}

	disabled := false
	cfg.Bots[0].Enabled = &disabled
	if err := Validate(cfg); err != nil {
		t.Fatalf("停用实例不应触发保留键校验: %v", err)
	}
	if _, err := Build(cfg, testDeps(storage.NewMemory(), nil, nil)); err != nil {
		t.Fatalf("停用实例应可装配（产出为空）: %v", err)
	}
}

func TestValidateRegistrationRejects(t *testing.T) {
	if err := validateRegistrationRejects(nil); err != nil {
		t.Fatalf("无被拒注册时不应报错: %v", err)
	}

	err := validateRegistrationRejects([]bot.AdapterRegistrationError{
		{Name: "", Reason: "注册名为空"},
		{Name: "half", Reason: "工厂为 nil"},
	})
	if err == nil {
		t.Fatal("存在被拒注册时应报错")
	}
	for _, want := range []string{"<空名>", "注册名为空", "half", "工厂为 nil"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误 = %v, 期望包含 %q", err, want)
		}
	}
}
