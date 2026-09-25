package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/adapters/mock"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/engine"
	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/plugins/echo"
)

// testPlugin 是用于引擎集成测试的可编程插件。
type testPlugin struct {
	name     string
	perms    []bot.Permission
	panicOn  string
	block    time.Duration
	canceled chan struct{}

	mu      sync.Mutex
	handled []string
}

func newTestPlugin(name string) *testPlugin {
	return &testPlugin{name: name, canceled: make(chan struct{}, 1)}
}

func (p *testPlugin) Metadata() bot.Metadata {
	return bot.Metadata{Name: p.name, Version: "v1", Permissions: p.perms}
}

func (p *testPlugin) Setup(ctx context.Context, reg bot.Registrar) error {
	reg.OnCommand("echo", func(ctx context.Context, e *bot.Event, r bot.Reply) error {
		return p.handle(ctx, e, r, true)
	}, bot.WithPriority(100))
	reg.OnAll(func(ctx context.Context, e *bot.Event, r bot.Reply) error {
		return p.handle(ctx, e, r, false)
	}, bot.WithPriority(-100))
	return nil
}

func (p *testPlugin) Start(context.Context) error { return nil }
func (p *testPlugin) Stop(context.Context) error  { return nil }

func (p *testPlugin) handle(ctx context.Context, e *bot.Event, r bot.Reply, isCommand bool) error {
	if p.panicOn == "command" && isCommand {
		panic("plugin exploded")
	}
	if p.block > 0 {
		select {
		case <-time.After(p.block):
		case <-ctx.Done():
			select {
			case p.canceled <- struct{}{}:
			default:
			}
			return ctx.Err()
		}
	}

	p.mu.Lock()
	p.handled = append(p.handled, e.Text())
	p.mu.Unlock()

	if isCommand {
		return r.Text(strings.Join(e.Command.Args, " ")).Send(ctx)
	}
	return r.Text(e.Text()).Send(ctx)
}

func (p *testPlugin) handledTexts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.handled...)
}

type harness struct {
	eng    *engine.Engine
	ads    map[string]*mock.Adapter
	cancel context.CancelFunc
	done   chan error

	stopOnce sync.Once
	stopErr  error
}

func startEngine(t *testing.T, cfg *config.Config, plugins []bot.Plugin, ads map[string]*mock.Adapter) *harness {
	t.Helper()

	names := make([]string, 0, len(ads))
	for name := range ads {
		names = append(names, name)
	}
	sort.Strings(names)

	bindings := make([]engine.AdapterBinding, 0, len(names))
	for _, name := range names {
		bindings = append(bindings, engine.AdapterBinding{BotID: name, Adapter: ads[name]})
	}

	logger := slog.New(slog.DiscardHandler)
	eng, err := engine.New(engine.Options{
		Config:   cfg,
		Logger:   logger,
		Adapters: bindings,
		Plugins:  plugins,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()

	for _, ad := range ads {
		deadline := time.Now().Add(3 * time.Second)
		for !ad.Started() {
			if time.Now().After(deadline) {
				t.Fatal("适配器未在超时内启动")
			}
			time.Sleep(time.Millisecond)
		}
	}

	h := &harness{eng: eng, ads: ads, cancel: cancel, done: done}
	t.Cleanup(func() { h.stop(t) })
	return h
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.stopOnce.Do(func() {
		h.cancel()
		select {
		case h.stopErr = <-h.done:
		case <-time.After(5 * time.Second):
			h.stopErr = errors.New("Run 未在超时内退出")
		}
	})
	if h.stopErr != nil {
		t.Errorf("Run: %v", h.stopErr)
	}
}

func mockConfig(bots ...config.BotConfig) *config.Config {
	return &config.Config{
		Bots:    bots,
		Plugins: map[string]config.PluginConfig{},
	}
}

func newMock(name string) *mock.Adapter {
	return mock.New(mock.Options{Name: name, Platform: "mock"})
}

// textMessage 构造一条只含文本的消息，避免测试误用空消息（空消息会被引擎拒绝）。
func textMessage(text string) *bot.Message {
	return &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
	}}
}

func TestEchoIntegrationRoundTrip(t *testing.T) {
	ad := newMock("mock-main")
	h := startEngine(t, mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}), []bot.Plugin{&echo.Plugin{}}, map[string]*mock.Adapter{"mock-main": ad})

	if _, err := ad.InjectText(context.Background(), "/echo hello"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}
	if !ad.WaitSent(1, 3*time.Second) {
		t.Fatal("未在超时内收到回复")
	}

	sent := ad.Sent()[0]
	if got := sent.Message.PlainText(); got != "hello" {
		t.Fatalf("回复内容 = %q, want hello", got)
	}
	if sent.Target.BotID != "mock-main" || sent.Target.Platform != "mock" || sent.Target.ChannelID != "mock-group" {
		t.Fatalf("回复目标 = %+v", sent.Target)
	}
	if sent.Message.Kind != bot.MessageGroup {
		t.Fatalf("回复会话类型 = %q", sent.Message.Kind)
	}
	if got := h.eng.Plugins(); len(got) != 1 || got[0].Name != "echo" {
		t.Fatalf("Plugins() = %+v", got)
	}
}

func TestGracefulShutdownDrainsQueuedEvents(t *testing.T) {
	ad := newMock("mock-main")
	plugin := newTestPlugin("p")
	h := startEngine(t, mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}), []bot.Plugin{plugin}, map[string]*mock.Adapter{"mock-main": ad})

	const total = 25
	for i := range total {
		if _, err := ad.InjectText(context.Background(), "m-"+itoa(i)); err != nil {
			t.Fatalf("InjectText(%d): %v", i, err)
		}
	}

	h.stop(t) // cancel 后 Run 必须排空已入队事件再退出

	if got := len(ad.Sent()); got != total {
		t.Fatalf("关闭前应处理完所有事件: 回复数 = %d, want %d", got, total)
	}
}

func TestSessionOrderingIsPreserved(t *testing.T) {
	ad := newMock("mock-main")
	plugin := newTestPlugin("p")
	startEngine(t, mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}), []bot.Plugin{plugin}, map[string]*mock.Adapter{"mock-main": ad})

	const total = 50
	for i := range total {
		if _, err := ad.InjectText(context.Background(), "m-"+itoa(i)); err != nil {
			t.Fatalf("InjectText(%d): %v", i, err)
		}
	}
	if !ad.WaitSent(total, 5*time.Second) {
		t.Fatalf("只收到 %d 条回复", len(ad.Sent()))
	}

	for i, sent := range ad.Sent() {
		if got, want := sent.Message.PlainText(), "m-"+itoa(i); got != want {
			t.Fatalf("第 %d 条回复 = %q, want %q（同一会话必须保序）", i, got, want)
		}
	}
}

func TestDuplicateEventIDIsDropped(t *testing.T) {
	ad := newMock("mock-main")
	plugin := newTestPlugin("p")
	startEngine(t, mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}), []bot.Plugin{plugin}, map[string]*mock.Adapter{"mock-main": ad})

	for range 3 {
		ev := &bot.Event{
			ID:       "fixed-id",
			Type:     bot.EventMessage,
			Message:  &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: "dup"}}}},
			Sender:   &bot.User{ID: "u1"},
			Channel:  &bot.Channel{ID: "g1", Kind: bot.MessageGroup},
			Platform: "mock",
			BotID:    "mock-main",
		}
		if err := ad.Inject(context.Background(), ev); err != nil {
			t.Fatalf("Inject: %v", err)
		}
	}
	if !ad.WaitSent(1, 3*time.Second) {
		t.Fatal("未收到回复")
	}
	time.Sleep(100 * time.Millisecond)

	if got := len(ad.Sent()); got != 1 {
		t.Fatalf("相同事件 ID 应只处理一次, 回复数 = %d", got)
	}
	if got := plugin.handledTexts(); len(got) != 1 {
		t.Fatalf("Handler 调用次数 = %d, want 1", len(got))
	}
}

func TestHandlerPanicIsIsolated(t *testing.T) {
	ad := newMock("mock-main")
	plugin := &testPlugin{name: "p", panicOn: "command", canceled: make(chan struct{}, 1)}
	startEngine(t, mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}), []bot.Plugin{plugin}, map[string]*mock.Adapter{"mock-main": ad})

	if _, err := ad.InjectText(context.Background(), "/echo boom"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}
	if _, err := ad.InjectText(context.Background(), "after-panic"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}
	if !ad.WaitSent(2, 3*time.Second) {
		t.Fatalf("panic 后引擎仍应继续处理事件, 已发送 %d 条", len(ad.Sent()))
	}

	// 命令 Handler panic 后不应产生它自己的回复（后续兜底规则可以照常执行）。
	for _, sent := range ad.Sent() {
		if got := sent.Message.PlainText(); got == "boom" {
			t.Fatalf("panic 的 Handler 不应发送回复: %+v", ad.Sent())
		}
	}
	// 后续事件必须被正常处理。
	if got := ad.Sent()[len(ad.Sent())-1].Message.PlainText(); got != "after-panic" {
		t.Fatalf("最后一条回复 = %q, want after-panic", got)
	}
}

func TestHandlerTimeoutCancelsContext(t *testing.T) {
	ad := newMock("mock-main")
	plugin := &testPlugin{name: "p", block: 5 * time.Second, canceled: make(chan struct{}, 1)}

	eng, err := engine.New(engine.Options{
		Config:   mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}),
		Logger:   slog.New(slog.DiscardHandler),
		Adapters: []engine.AdapterBinding{{BotID: "mock-main", Adapter: ad}},
		Plugins:  []bot.Plugin{plugin},
		// 规则级超时：必须让阻塞的 Handler 感知 ctx 取消。
		RuleTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for !ad.Started() {
		if time.Now().After(deadline) {
			t.Fatal("适配器未启动")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := ad.InjectText(context.Background(), "slow"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}

	// 兜底规则超时为默认 10s，用事件整体的 EventTimeout 之外无法等待，故直接观察取消信号。
	select {
	case <-plugin.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Handler 未在超时内被取消")
	}
}

func TestPerBotPluginWhitelist(t *testing.T) {
	ads := map[string]*mock.Adapter{"mock-a": newMock("mock-a"), "mock-b": newMock("mock-b")}
	cfg := mockConfig(
		config.BotConfig{Name: "mock-a", Adapter: "mock", Plugins: []string{"p"}},
		config.BotConfig{Name: "mock-b", Adapter: "mock", Plugins: []string{"other"}},
	)
	plugin := newTestPlugin("p")
	startEngine(t, cfg, []bot.Plugin{plugin}, ads)

	if _, err := ads["mock-a"].InjectText(context.Background(), "from-a"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}
	if _, err := ads["mock-b"].InjectText(context.Background(), "from-b"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}

	if !ads["mock-a"].WaitSent(1, 3*time.Second) {
		t.Fatal("白名单内的 bot 应收到回复")
	}
	time.Sleep(100 * time.Millisecond)

	if got := len(ads["mock-b"].Sent()); got != 0 {
		t.Fatalf("白名单外的 bot 不应收到回复, 回复数 = %d", got)
	}
	if got := plugin.handledTexts(); len(got) != 1 || got[0] != "from-a" {
		t.Fatalf("Handler 处理日志 = %v", got)
	}
}

func TestEngineRequiresValidOptions(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	ad := newMock("mock-main")

	if _, err := engine.New(engine.Options{Logger: logger}); err == nil {
		t.Fatal("缺少 Config 应返回错误")
	}
	if _, err := engine.New(engine.Options{
		Config:   mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}),
		Logger:   logger,
		Adapters: []engine.AdapterBinding{{BotID: "mock-main"}},
	}); err == nil {
		t.Fatal("nil 适配器应返回错误")
	}
	if _, err := engine.New(engine.Options{
		Config:   mockConfig(),
		Logger:   logger,
		Adapters: []engine.AdapterBinding{{Adapter: ad}},
	}); err == nil {
		t.Fatal("空 BotID 应返回错误")
	}
	if _, err := engine.New(engine.Options{
		Config:   mockConfig(),
		Logger:   logger,
		Adapters: []engine.AdapterBinding{{BotID: "b", Adapter: ad}, {BotID: "b", Adapter: ad}},
	}); err == nil {
		t.Fatal("重复 BotID 应返回错误")
	}
}

func TestEngineSendResolvesAdapter(t *testing.T) {
	ad := newMock("mock-main")
	h := startEngine(t, mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}), nil, map[string]*mock.Adapter{"mock-main": ad})
	ctx := context.Background()

	var errs []error
	record := func(_ *bot.SendResult, err error) { errs = append(errs, err) }

	record(h.eng.Send(ctx, bot.Target{Platform: "mock", ChannelID: "c1"}, textMessage("hi")))
	record(h.eng.Send(ctx, bot.Target{Platform: "mock", BotID: "mock-main", ChannelID: "c1"}, textMessage("hi")))
	record(h.eng.Send(ctx, bot.Target{Platform: "feishu", ChannelID: "c1"}, textMessage("hi")))
	record(h.eng.Send(ctx, bot.Target{Platform: "mock", BotID: "missing", ChannelID: "c1"}, textMessage("hi")))
	record(h.eng.Send(ctx, bot.Target{ChannelID: "c1"}, textMessage("hi")))
	record(h.eng.Send(ctx, bot.Target{Platform: "mock"}, nil))

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("同平台单 bot 时应能自动解析: %v", errs[:2])
	}
	for i, err := range errs[2:] {
		if err == nil {
			t.Fatalf("第 %d 次调用应返回错误", i+2)
		}
	}
	if got := len(ad.Sent()); got != 2 {
		t.Fatalf("发送记录数 = %d, want 2", got)
	}
	if got := ad.Sent()[0].Message.PlainText(); got != "hi" {
		t.Fatalf("发送内容 = %q, want hi", got)
	}
}

// TestSendRejectsMessageWithoutSegments 覆盖「降级后没有可发送的段」：空消息与
// 全部段都被丢弃的消息都必须报错，绝不能静默发出空消息。
func TestSendRejectsMessageWithoutSegments(t *testing.T) {
	ad := newMock("mock-main")
	h := startEngine(t, mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}), nil, map[string]*mock.Adapter{"mock-main": ad})

	if _, err := h.eng.Send(context.Background(), bot.Target{Platform: "mock", BotID: "mock-main"}, &bot.Message{Kind: bot.MessageGroup}); err == nil {
		t.Fatal("空消息应报错")
	}

	// 引用段在 mock 上受支持，改成把适配器能力限制为仅文本，再发一条纯引用消息。
	textOnly := mock.New(mock.Options{Name: "text-only", Platform: "textonly", Capabilities: bot.Capabilities{Text: true}})
	cfg := mockConfig(config.BotConfig{Name: "text-only", Adapter: "mock"})
	h2 := startEngine(t, cfg, nil, map[string]*mock.Adapter{"text-only": textOnly})
	msg := &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
		{Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: "m0"}},
	}}
	if _, err := h2.eng.Send(context.Background(), bot.Target{Platform: "textonly", BotID: "text-only"}, msg); err == nil {
		t.Fatal("降级后为空的引用消息应报错")
	}
	if got := len(textOnly.Sent()); got != 0 {
		t.Fatalf("不应产生发送记录: %d", got)
	}
}

func TestMultiBotSamePlatformNeedsExplicitBotID(t *testing.T) {
	ads := map[string]*mock.Adapter{"mock-a": newMock("mock-a"), "mock-b": newMock("mock-b")}
	cfg := mockConfig(
		config.BotConfig{Name: "mock-a", Adapter: "mock"},
		config.BotConfig{Name: "mock-b", Adapter: "mock"},
	)
	h := startEngine(t, cfg, nil, ads)

	if _, err := h.eng.Send(context.Background(), bot.Target{Platform: "mock"}, textMessage("hi")); err == nil {
		t.Fatal("同平台多 bot 且未指定 BotID 时应报错")
	}
	if _, err := h.eng.Send(context.Background(), bot.Target{Platform: "mock", BotID: "mock-b"}, textMessage("hi")); err != nil {
		t.Fatalf("指定 BotID 后应成功: %v", err)
	}
	if _, err := h.eng.Send(context.Background(), bot.Target{Platform: "mock", BotID: "mock-a"}, textMessage("hi")); err != nil {
		t.Fatalf("指定 BotID 后应成功: %v", err)
	}
	if got := len(ads["mock-b"].Sent()); got != 1 {
		t.Fatalf("mock-b 发送记录数 = %d, want 1", got)
	}
}

func TestRunRejectsSecondCall(t *testing.T) {
	ad := newMock("mock-main")
	h := startEngine(t, mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}), nil, map[string]*mock.Adapter{"mock-main": ad})

	if err := h.eng.Run(context.Background()); err == nil {
		t.Fatal("重复 Run 应返回错误")
	}
}

func TestReplyWithoutEventFails(t *testing.T) {
	ad := newMock("mock-main")
	h := startEngine(t, mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"}), nil, map[string]*mock.Adapter{"mock-main": ad})

	if _, err := h.eng.Reply(context.Background(), nil, &bot.Message{}); err == nil {
		t.Fatal("nil 事件应返回错误")
	}
}

func TestAdapterStartFailureSurfaces(t *testing.T) {
	cfg := mockConfig(config.BotConfig{Name: "mock-main", Adapter: "mock"})
	bad := mock.New(mock.Options{Name: "mock-main", Platform: "mock", ListenAddr: "127.0.0.1:-1"})
	eng, err := engine.New(engine.Options{
		Config:   cfg,
		Logger:   slog.New(slog.DiscardHandler),
		Adapters: []engine.AdapterBinding{{BotID: "mock-main", Adapter: bad}},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = eng.Run(ctx)
	if err == nil {
		t.Fatal("适配器启动失败应作为 Run 的错误返回")
	}
	if !strings.Contains(err.Error(), "mock-main") {
		t.Fatalf("错误信息应包含 bot 名: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestDisabledBotExcludedFromPluginWhitelist 验证 enabled: false 的实例不参与
// 插件白名单收窄，也不会把允许它的事件错误地限制掉。
func TestDisabledBotExcludedFromPluginWhitelist(t *testing.T) {
	ads := map[string]*mock.Adapter{"mock-a": newMock("mock-a"), "mock-b": newMock("mock-b")}
	disabled := false
	cfg := mockConfig(
		config.BotConfig{Name: "mock-a", Adapter: "mock", Plugins: []string{"p"}},
		// 唯一配置了白名单的实例被停用：mock-a 未配白名单，应视为允许全部插件。
		config.BotConfig{Name: "mock-b", Adapter: "mock", Plugins: []string{"other"}, Enabled: &disabled},
	)
	plugin := newTestPlugin("p")
	startEngine(t, cfg, []bot.Plugin{plugin}, ads)

	if _, err := ads["mock-a"].InjectText(context.Background(), "from-a"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}
	if !ads["mock-a"].WaitSent(1, 3*time.Second) {
		t.Fatal("mock-a 未配白名单，插件应可回复")
	}
	if got := plugin.handledTexts(); len(got) != 1 || got[0] != "from-a" {
		t.Fatalf("Handler 处理日志 = %v", got)
	}
}

// TestDisabledBotWhitelistNarrowing 验证启用实例的白名单仍照常收窄规则范围。
func TestDisabledBotWhitelistNarrowing(t *testing.T) {
	ads := map[string]*mock.Adapter{"mock-a": newMock("mock-a"), "mock-b": newMock("mock-b")}
	disabled := false
	cfg := mockConfig(
		config.BotConfig{Name: "mock-a", Adapter: "mock", Plugins: []string{"p"}},
		config.BotConfig{Name: "mock-b", Adapter: "mock", Plugins: []string{"other"}},
		config.BotConfig{Name: "mock-c", Adapter: "mock", Plugins: []string{"p"}, Enabled: &disabled},
	)
	plugin := newTestPlugin("p")
	h := startEngine(t, cfg, []bot.Plugin{plugin}, ads)

	// 规则适用范围必须排除被停用的 mock-c（它不可能收到事件）。
	for _, rule := range h.eng.Router().Rules() {
		for _, id := range rule.BotIDs {
			if id == "mock-c" {
				t.Fatalf("停用实例不应出现在规则白名单: %+v", rule.BotIDs)
			}
		}
	}

	// 白名单仍照常生效：mock-b 不允许插件 p。
	if _, err := ads["mock-a"].InjectText(context.Background(), "from-a"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}
	if _, err := ads["mock-b"].InjectText(context.Background(), "from-b"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}
	if !ads["mock-a"].WaitSent(1, 3*time.Second) {
		t.Fatal("白名单内的 bot 应收到回复")
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(ads["mock-b"].Sent()); got != 0 {
		t.Fatalf("白名单外的 bot 不应收到回复, 回复数 = %d", got)
	}
}
