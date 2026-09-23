package router

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

var (
	errBoom = errors.New("boom")
	errBang = errors.New("bang")
)

// textEvent 构造一条带纯文本段的群聊消息事件。
func textEvent(text string) *bot.Event {
	return &bot.Event{
		ID:       "ev-1",
		Type:     bot.EventMessage,
		Platform: "test",
		BotID:    "bot-1",
		Time:     time.Now().UTC(),
		Message: &bot.Message{
			Kind:     bot.MessageGroup,
			Segments: []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}}},
		},
	}
}

// noticeEvent 构造一条通知事件，它没有 Message。
func noticeEvent() *bot.Event {
	return &bot.Event{
		ID:       "ev-2",
		Type:     bot.EventNotice,
		Platform: "test",
		BotID:    "bot-1",
		Time:     time.Now().UTC(),
	}
}

// recorder 按顺序记录被调用的处理单元，用于断言执行顺序。
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (rec *recorder) hit(tag string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.calls = append(rec.calls, tag)
}

func (rec *recorder) order() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.calls)
}

// tagMiddleware 返回一个记录自身名字后调用 next 的中间件。
func tagMiddleware(rec *recorder, tag string) bot.Middleware {
	return func(next bot.Handler) bot.Handler {
		return func(ctx context.Context, e *bot.Event, r bot.Reply) error {
			rec.hit(tag)
			return next(ctx, e, r)
		}
	}
}

// tagHandler 返回一个记录自身名字的处理器。
func tagHandler(rec *recorder, tag string) bot.Handler {
	return func(context.Context, *bot.Event, bot.Reply) error {
		rec.hit(tag)
		return nil
	}
}

// replyFactory 记录工厂为每次规则调用创建的 Reply 实例。
type replyFactory struct {
	mu    sync.Mutex
	all   []*bot.NoopReply
	calls int
}

func (f *replyFactory) new(ctx context.Context, ev *bot.Event) bot.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	r := bot.NewNoopReply()
	f.all = append(f.all, r)
	return r
}

func (f *replyFactory) snapshot() ([]*bot.NoopReply, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.all), f.calls
}

// TestDispatchParsesCommand 验证 Dispatch 会用配置的前缀解析命令并写回事件。
func TestDispatchParsesCommand(t *testing.T) {
	var got bot.Command
	r := New(Options{})
	reg := r.Registrar("echo")
	reg.OnCommand("echo", func(_ context.Context, ev *bot.Event, _ bot.Reply) error {
		got = *ev.Command
		return nil
	})

	ev := textEvent("  /echo   hi  there ")
	if err := r.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if ev.Command == nil {
		t.Fatal("Dispatch 未把解析出的命令写回事件")
	}
	if got.Name != "echo" {
		t.Errorf("命令名 = %q, 期望 echo", got.Name)
	}
	if want := []string{"hi", "there"}; !slices.Equal(got.Args, want) {
		t.Errorf("参数 = %q, 期望 %q", got.Args, want)
	}
	if got.Raw != "hi  there" {
		t.Errorf("Raw = %q, 期望保留原始空白 %q", got.Raw, "hi  there")
	}
}

// TestDispatchKeepsParsedCommand 验证事件已带 Command 时不会被重新解析覆盖。
func TestDispatchKeepsParsedCommand(t *testing.T) {
	r := New(Options{})
	reg := r.Registrar("manual")
	reg.OnCommand("manual", func(_ context.Context, ev *bot.Event, _ bot.Reply) error {
		if ev.Command.Name != "manual" {
			t.Errorf("命令名 = %q, 期望保留 manual", ev.Command.Name)
		}
		return nil
	})

	ev := textEvent("/echo hi")
	ev.Command = &bot.Command{Name: "manual", Args: []string{"x"}}
	if err := r.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
}

// TestDispatchCustomPrefixes 验证自定义前缀生效且默认前缀被替换。
func TestDispatchCustomPrefixes(t *testing.T) {
	r := New(Options{CommandPrefixes: []string{"!"}})
	var hits []string
	reg := r.Registrar("p")
	reg.OnCommand("echo", func(_ context.Context, ev *bot.Event, _ bot.Reply) error {
		hits = append(hits, ev.Command.Name)
		return nil
	})

	if err := r.Dispatch(context.Background(), textEvent("/echo hi")); err != nil {
		t.Fatalf("Dispatch /echo 返回错误: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("默认前缀 / 不应生效, 实际命中 %v", hits)
	}
	if err := r.Dispatch(context.Background(), textEvent("!echo hi")); err != nil {
		t.Fatalf("Dispatch !echo 返回错误: %v", err)
	}
	if !slices.Equal(hits, []string{"echo"}) {
		t.Fatalf("命中 = %v, 期望 [echo]", hits)
	}
}

// TestDispatchOrder 验证命中规则按优先级降序、同优先级按注册顺序执行。
func TestDispatchOrder(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnAll(tagHandler(rec, "low-1"))
	reg.OnAll(tagHandler(rec, "low-2"))
	reg.OnAll(tagHandler(rec, "high-1"), bot.WithPriority(10))
	reg.OnAll(tagHandler(rec, "high-2"), bot.WithPriority(10))
	reg.OnAll(tagHandler(rec, "mid"), bot.WithPriority(5))

	if err := r.Dispatch(context.Background(), textEvent("anything")); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	want := []string{"high-1", "high-2", "mid", "low-1", "low-2"}
	if got := rec.order(); !slices.Equal(got, want) {
		t.Errorf("执行顺序 = %v, 期望 %v", got, want)
	}
}

// TestDispatchAllMatchesRun 验证多个不同类别的命中规则都会执行，而不是只执行第一条。
func TestDispatchAllMatchesRun(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnCommand("echo", tagHandler(rec, "command"))
	reg.OnKeyword([]string{"hi"}, tagHandler(rec, "keyword"))
	reg.OnRegex(`^/echo\s+hi$`, tagHandler(rec, "regex"))

	if err := r.Dispatch(context.Background(), textEvent("/echo hi")); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	want := []string{"command", "keyword", "regex"}
	if got := rec.order(); !slices.Equal(got, want) {
		t.Errorf("命中集合 = %v, 期望 %v", got, want)
	}
}

// TestDispatchNoMatch 验证无规则命中时返回 nil 且不调用任何处理器。
func TestDispatchNoMatch(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	r.Registrar("p").OnCommand("echo", tagHandler(rec, "command"))

	if err := r.Dispatch(context.Background(), textEvent("hello world")); err != nil {
		t.Fatalf("无命中应返回 nil, 实际 %v", err)
	}
	if len(rec.order()) != 0 {
		t.Errorf("无命中不应调用处理器, 实际 %v", rec.order())
	}
	if err := r.Dispatch(context.Background(), nil); err != nil {
		t.Errorf("nil 事件应返回 nil, 实际 %v", err)
	}
}

// TestOnRegexAndRegistrationErrors 验证非法正则不 panic、记入 RegistrationErrors，
// 且此时其它规则依然可注册、可命中；同时确认失败的注册不占用 ID 序号。
func TestOnRegexAndRegistrationErrors(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnRegex(`(unclosed`, tagHandler(rec, "bad"))
	reg.OnRegex(`^hi\s+(\S+)$`, tagHandler(rec, "ok"))

	errs := r.RegistrationErrors()
	if len(errs) != 1 {
		t.Fatalf("RegistrationErrors 数量 = %d, 期望 1: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "编译失败") {
		t.Errorf("错误信息未说明正则编译失败: %v", errs[0])
	}

	if err := r.Dispatch(context.Background(), textEvent("hi there")); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"ok"}) {
		t.Fatalf("命中 = %v, 期望 [ok]", got)
	}

	rules := r.Rules()
	if len(rules) != 1 {
		t.Fatalf("规则数 = %d, 期望 1", len(rules))
	}
	if rules[0].ID != "p:regex:0" {
		t.Errorf("失败的正则不应占用序号, 实际 ID = %q, 期望 p:regex:0", rules[0].ID)
	}

	// 返回的切片是副本：修改它不影响路由器内部累计的错误。
	errs[0] = nil
	if got := r.RegistrationErrors(); len(got) != 1 || got[0] == nil {
		t.Errorf("RegistrationErrors 未返回副本: %v", got)
	}
}

// TestOnKeywordCaseInsensitive 验证关键词匹配忽略大小写。
func TestOnKeywordCaseInsensitive(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	r.Registrar("p").OnKeyword([]string{"WeLcOmE"}, tagHandler(rec, "kw"))

	if err := r.Dispatch(context.Background(), textEvent("say welcome home")); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"kw"}) {
		t.Fatalf("命中 = %v, 期望 [kw]", got)
	}
}

// TestDispatchEventType 验证事件类型触发，非消息事件同样可被路由。
func TestDispatchEventType(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnEvent(bot.EventNotice, tagHandler(rec, "notice"))
	reg.OnEvent(bot.EventRequest, tagHandler(rec, "request"))

	if err := r.Dispatch(context.Background(), noticeEvent()); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"notice"}) {
		t.Fatalf("命中 = %v, 期望 [notice]", got)
	}
}

// TestDispatchPlatformFilter 验证 WithPlatforms 限定生效平台。
func TestDispatchPlatformFilter(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnAll(tagHandler(rec, "qq-only"), bot.WithPlatforms("qq"))
	reg.OnAll(tagHandler(rec, "feishu"), bot.WithPlatforms("feishu"))

	ev := noticeEvent()
	ev.Platform = "feishu"
	if err := r.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"feishu"}) {
		t.Fatalf("命中 = %v, 期望 [feishu]", got)
	}
}

// TestOnAllFallback 验证 OnAll 不设过滤条件，且 opts 在默认值之后应用。
func TestOnAllFallback(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnAll(tagHandler(rec, "fallback"))
	reg.OnAll(tagHandler(rec, "message-only"), bot.WithEventType(bot.EventMessage))
	// OnCommand 默认 EventMessage，WithEventType 在默认值之后应用，应覆盖为 notice。
	reg.OnCommand("ping", tagHandler(rec, "overridden"), bot.WithEventType(bot.EventNotice))

	ev := noticeEvent()
	ev.Command = &bot.Command{Name: "ping"}
	if err := r.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"fallback", "overridden"}) {
		t.Fatalf("命中 = %v, 期望 [fallback overridden]", got)
	}
}

// TestMiddlewareOrder 验证全局中间件在最外层，且各组内先注册的先执行。
func TestMiddlewareOrder(t *testing.T) {
	rec := &recorder{}
	r := New(Options{Middlewares: []bot.Middleware{
		tagMiddleware(rec, "g1"),
		tagMiddleware(rec, "g2"),
	}})
	reg := r.Registrar("p")
	reg.Use(tagMiddleware(rec, "p1"))
	reg.Use(tagMiddleware(rec, "p2"))
	reg.OnCommand("echo", tagHandler(rec, "handler"))

	if err := r.Dispatch(context.Background(), textEvent("/echo hi")); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	want := []string{"g1", "g2", "p1", "p2", "handler"}
	if got := rec.order(); !slices.Equal(got, want) {
		t.Errorf("中间件顺序 = %v, 期望 %v", got, want)
	}
}

// TestUseScope 验证插件级 Use 只影响调用之后注册的规则。
func TestUseScope(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnAll(tagHandler(rec, "before"))
	reg.Use(tagMiddleware(rec, "mw"))
	reg.OnAll(tagHandler(rec, "after"))

	if err := r.Dispatch(context.Background(), noticeEvent()); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	want := []string{"before", "mw", "after"}
	if got := rec.order(); !slices.Equal(got, want) {
		t.Errorf("执行序列 = %v, 期望 %v", got, want)
	}
}

// TestUseIsPerPlugin 验证插件中间件不泄漏到其它插件。
func TestUseIsPerPlugin(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	alpha := r.Registrar("alpha")
	alpha.Use(tagMiddleware(rec, "alpha-mw"))
	alpha.OnAll(tagHandler(rec, "alpha-rule"))
	r.Registrar("beta").OnAll(tagHandler(rec, "beta-rule"))

	if err := r.Dispatch(context.Background(), noticeEvent()); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	want := []string{"alpha-mw", "alpha-rule", "beta-rule"}
	if got := rec.order(); !slices.Equal(got, want) {
		t.Errorf("执行序列 = %v, 期望 %v", got, want)
	}
}

// TestMiddlewareReadsRoute 验证中间件能从 ctx 读到完整的路由信息。
func TestMiddlewareReadsRoute(t *testing.T) {
	var got *bot.Route
	r := New(Options{Middlewares: []bot.Middleware{
		func(next bot.Handler) bot.Handler {
			return func(ctx context.Context, e *bot.Event, rp bot.Reply) error {
				route, ok := bot.RouteFrom(ctx)
				if !ok {
					return errors.New("ctx 中缺少 Route")
				}
				got = route
				return next(ctx, e, rp)
			}
		},
	}})
	reg := r.Registrar("weather")
	reg.OnCommand("forecast", tagHandler(&recorder{}, "h"), bot.WithAdmin(), bot.WithPriority(7))

	if err := r.Dispatch(context.Background(), textEvent("/forecast 北京")); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got == nil {
		t.Fatal("中间件未读到 Route")
	}
	want := bot.Route{
		Plugin:    "weather",
		RuleID:    "weather:command:0",
		Trigger:   "command:forecast",
		Priority:  7,
		AdminOnly: true,
	}
	if got.Plugin != want.Plugin || got.RuleID != want.RuleID || got.Trigger != want.Trigger ||
		got.Priority != want.Priority || got.AdminOnly != want.AdminOnly {
		t.Errorf("Route = %+v, 期望 %+v", *got, want)
	}
	if len(got.RegexMatches) != 0 {
		t.Errorf("命令规则的 RegexMatches 应为空, 实际 %q", got.RegexMatches)
	}
}

// TestRouteTriggerDescriptions 验证各类规则的 Trigger 描述，其中关键词类为空时退化为 all。
func TestRouteTriggerDescriptions(t *testing.T) {
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnCommand("echo", tagHandler(&recorder{}, "c"))
	reg.OnRegex(`^hi`, tagHandler(&recorder{}, "re"))
	reg.OnKeyword([]string{"a", "b"}, tagHandler(&recorder{}, "kw"))
	reg.OnEvent(bot.EventNotice, tagHandler(&recorder{}, "ev"))
	reg.OnAll(tagHandler(&recorder{}, "all"))

	want := []string{"command:echo", "regex:^hi", "keyword:a,b", "event:notice", "all"}
	var got []string
	for _, rule := range r.Rules() {
		got = append(got, triggerOf(rule))
	}
	if !slices.Equal(got, want) {
		t.Errorf("Trigger = %v, 期望 %v", got, want)
	}
}

// TestRegexCaptures 验证正则捕获组通过 Route.RegexMatches 传给 Handler。
func TestRegexCaptures(t *testing.T) {
	var matches []string
	r := New(Options{})
	reg := r.Registrar("weather")
	reg.OnRegex(`^天气\s+(\S+)$`, func(ctx context.Context, _ *bot.Event, _ bot.Reply) error {
		route, ok := bot.RouteFrom(ctx)
		if !ok {
			return errors.New("ctx 中缺少 Route")
		}
		matches = route.RegexMatches
		return nil
	})

	if err := r.Dispatch(context.Background(), textEvent("天气 北京")); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if len(matches) != 2 || matches[1] != "北京" {
		t.Fatalf("RegexMatches = %q, 期望 [天气 北京 北京]", matches)
	}
}

// TestReplyPerRule 验证每条命中规则拿到独立的 Reply，互不污染。
func TestReplyPerRule(t *testing.T) {
	factory := &replyFactory{}
	r := New(Options{Replies: factory.new})
	reg := r.Registrar("p")
	reg.OnAll(func(ctx context.Context, _ *bot.Event, rp bot.Reply) error {
		return rp.Text("alpha").Send(ctx)
	})
	reg.OnAll(func(ctx context.Context, _ *bot.Event, rp bot.Reply) error {
		return rp.Text("beta").Send(ctx)
	})

	if err := r.Dispatch(context.Background(), noticeEvent()); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	all, calls := factory.snapshot()
	if calls != 2 || len(all) != 2 {
		t.Fatalf("工厂调用 %d 次、产生 %d 个 Reply, 期望各 2", calls, len(all))
	}
	if all[0] == all[1] {
		t.Fatal("两条规则复用了同一个 Reply 实例")
	}
	if got := all[0].PlainText(); got != "alpha" {
		t.Errorf("第一条规则的回复 = %q, 期望 alpha", got)
	}
	if got := all[1].PlainText(); got != "beta" {
		t.Errorf("第二条规则的回复 = %q, 期望 beta", got)
	}
}

// TestReplyFactoryFallback 验证未配置工厂时使用 NoopReply，Handler 不因此失败。
func TestReplyFactoryFallback(t *testing.T) {
	r := New(Options{})
	var kind string
	r.Registrar("p").OnAll(func(ctx context.Context, _ *bot.Event, rp bot.Reply) error {
		if _, ok := rp.(*bot.NoopReply); !ok {
			t.Errorf("Reply 类型 = %T, 期望 *bot.NoopReply", rp)
		}
		kind = fmt.Sprintf("%T", rp)
		return rp.Text("x").Send(ctx)
	})

	if err := r.Dispatch(context.Background(), noticeEvent()); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if kind == "" {
		t.Fatal("Handler 未被调用")
	}
}

// TestHandlerErrorsAggregated 验证多个 Handler 的错误被聚合，且不影响后续规则执行。
func TestHandlerErrorsAggregated(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnAll(func(context.Context, *bot.Event, bot.Reply) error {
		rec.hit("first")
		return errBoom
	})
	reg.OnAll(func(context.Context, *bot.Event, bot.Reply) error {
		rec.hit("second")
		return fmt.Errorf("包装: %w", errBang)
	})
	reg.OnAll(tagHandler(rec, "third"))

	err := r.Dispatch(context.Background(), noticeEvent())
	if err == nil {
		t.Fatal("Dispatch 应返回聚合错误")
	}
	if !errors.Is(err, errBoom) || !errors.Is(err, errBang) {
		t.Errorf("聚合错误无法识别全部原因: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"first", "second", "third"}) {
		t.Errorf("出错后仍应继续执行后续规则, 实际 %v", got)
	}
	if !strings.Contains(err.Error(), "p:all:0") || !strings.Contains(err.Error(), "p:all:1") {
		t.Errorf("聚合错误未标注规则 ID: %v", err)
	}
}

// TestRegisterValidation 验证 Register 的校验与自动 ID 生成。
func TestRegisterValidation(t *testing.T) {
	r := New(Options{})

	if err := r.Register(&bot.Rule{ID: "no-handler"}); err == nil {
		t.Error("Handler 为 nil 时应返回错误")
	}
	if err := r.Register(nil); err == nil {
		t.Error("nil 规则应返回错误")
	}

	handler := tagHandler(&recorder{}, "h")
	first := &bot.Rule{Plugin: "p", Command: "a", Handler: handler}
	if err := r.Register(first); err != nil {
		t.Fatalf("Register 返回错误: %v", err)
	}
	second := &bot.Rule{Plugin: "p", Command: "b", Handler: handler}
	if err := r.Register(second); err != nil {
		t.Fatalf("Register 返回错误: %v", err)
	}
	if first.ID != "p:command:0" || second.ID != "p:command:1" {
		t.Errorf("自动 ID = %q, %q, 期望 p:command:0, p:command:1", first.ID, second.ID)
	}

	if err := r.Register(&bot.Rule{ID: first.ID, Handler: handler}); err == nil {
		t.Error("重复 Rule.ID 应返回错误")
	}

	explicit := &bot.Rule{Plugin: "p", Handler: handler, Regex: mustCompile(`x`), ID: "custom"}
	if err := r.Register(explicit); err != nil {
		t.Fatalf("Register 返回错误: %v", err)
	}
	if explicit.ID != "custom" {
		t.Errorf("显式 ID 被覆盖为 %q", explicit.ID)
	}

	if errs := r.RegistrationErrors(); len(errs) != 0 {
		t.Errorf("Register 的错误不应重复累计到 RegistrationErrors: %v", errs)
	}
	if got := len(r.Rules()); got != 3 {
		t.Errorf("规则数 = %d, 期望 3", got)
	}
}

// TestAutoIDPerKind 验证不同类别的规则各自独立编号，且显式 ID 不占用序号。
func TestAutoIDPerKind(t *testing.T) {
	r := New(Options{})
	reg := r.Registrar("p")
	reg.OnCommand("a", tagHandler(&recorder{}, "1"))
	reg.OnCommand("b", tagHandler(&recorder{}, "2"))
	reg.OnRegex(`x`, tagHandler(&recorder{}, "3"))
	reg.OnKeyword([]string{"k"}, tagHandler(&recorder{}, "4"), bot.WithID("explicit"))
	reg.OnKeyword([]string{"k2"}, tagHandler(&recorder{}, "5"))
	reg.OnEvent(bot.EventNotice, tagHandler(&recorder{}, "6"))
	reg.OnAll(tagHandler(&recorder{}, "7"))

	var got []string
	for _, rule := range r.Rules() {
		got = append(got, rule.ID)
	}
	want := []string{"p:command:0", "p:command:1", "p:regex:0", "explicit", "p:keyword:0", "p:event:0", "p:all:0"}
	if !slices.Equal(got, want) {
		t.Errorf("自动 ID = %v, 期望 %v", got, want)
	}

	seen := make(map[string]struct{}, len(got))
	for _, id := range got {
		if _, dup := seen[id]; dup {
			t.Errorf("规则 ID 重复: %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestRulesCopyIsolation 验证 Rules 返回的副本与内部状态隔离。
func TestRulesCopyIsolation(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	r.Registrar("p").OnKeyword([]string{"hello"}, tagHandler(rec, "kw"))

	rules := r.Rules()
	if len(rules) != 1 {
		t.Fatalf("规则数 = %d, 期望 1", len(rules))
	}
	rules[0].ID = "hacked"
	rules[0].Keywords[0] = "nope"

	if err := r.Dispatch(context.Background(), textEvent("hello there")); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"kw"}) {
		t.Errorf("修改 Rules 返回值影响了路由器: 命中 %v", got)
	}
	if got := r.Rules()[0].ID; got != "p:keyword:0" {
		t.Errorf("规则 ID 被外部修改为 %q", got)
	}
}

// TestUpdateRules 验证 UpdateRules 直接改动内部规则对象、重排序并重建快照，
// 同时 Rules() 仍返回拷贝。
func TestUpdateRules(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	r.Registrar("alpha").OnAll(tagHandler(rec, "alpha"))
	r.Registrar("beta").OnAll(tagHandler(rec, "beta"))

	if err := r.UpdateRules(nil); err == nil {
		t.Error("fn 为 nil 时应返回错误")
	}
	if err := r.UpdateRules(func(rule *bot.Rule) {
		if rule.Plugin == "alpha" {
			rule.BotIDs = []string{"bot-a"}
		}
	}); err != nil {
		t.Fatalf("UpdateRules 返回错误: %v", err)
	}

	// bot-a 的事件两条规则都能命中。
	ev := noticeEvent()
	ev.BotID = "bot-a"
	if err := r.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Errorf("bot-a 命中 = %v, 期望 [alpha beta]", got)
	}

	// bot-b 的事件只命中未收窄的 beta。
	ev = noticeEvent()
	ev.BotID = "bot-b"
	if err := r.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"alpha", "beta", "beta"}) {
		t.Errorf("bot-b 命中 = %v, 期望 [alpha beta beta]", got)
	}

	// Rules() 依然是拷贝：写回 bot-b 并改 Priority 不影响内部状态。
	rules := r.Rules()
	for _, rule := range rules {
		if rule.Plugin == "alpha" {
			rule.BotIDs = []string{"bot-b"}
			rule.Priority = 99
		}
	}
	ev = noticeEvent()
	ev.BotID = "bot-b"
	if err := r.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"alpha", "beta", "beta", "beta"}) {
		t.Errorf("Rules() 返回值被写回内部状态, 实际 %v", got)
	}
}

// TestUpdateRulesReorders 验证 UpdateRules 修改 Priority 后会重新排序。
func TestUpdateRulesReorders(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	r.Registrar("p").OnAll(tagHandler(rec, "first"))
	r.Registrar("p").OnAll(tagHandler(rec, "second"))

	if err := r.UpdateRules(func(rule *bot.Rule) {
		if rule.ID == "p:all:0" {
			rule.Priority = 100
		}
	}); err != nil {
		t.Fatalf("UpdateRules 返回错误: %v", err)
	}
	if err := r.Dispatch(context.Background(), noticeEvent()); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("顺序 = %v, 期望 [first second]（first 提升优先级后仍在最前）", got)
	}

	// 反向调整：second 提到最高优先级。
	if err := r.UpdateRules(func(rule *bot.Rule) {
		rule.Priority = 0
		if rule.ID == "p:all:1" {
			rule.Priority = 100
		}
	}); err != nil {
		t.Fatalf("UpdateRules 返回错误: %v", err)
	}
	if err := r.Dispatch(context.Background(), noticeEvent()); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"first", "second", "second", "first"}) {
		t.Errorf("顺序 = %v, 期望末两条为 [second first]", got)
	}
}

// TestUpdateRulesRejectsNilHandler 验证 fn 清空 Handler 时报错且不发布新快照。
func TestUpdateRulesRejectsNilHandler(t *testing.T) {
	rec := &recorder{}
	r := New(Options{})
	r.Registrar("p").OnAll(tagHandler(rec, "keep"))

	if err := r.UpdateRules(func(rule *bot.Rule) { rule.Handler = nil }); err == nil {
		t.Fatal("Handler 为 nil 时应返回错误")
	}
	if err := r.Dispatch(context.Background(), noticeEvent()); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := rec.order(); !slices.Equal(got, []string{"keep"}) {
		t.Errorf("失败后不应发布新快照, 实际 %v", got)
	}
}

// TestConcurrentDispatchAndRegister 在 -race 下验证并发 Dispatch 与注册。
func TestConcurrentDispatchAndRegister(t *testing.T) {
	var counter sync.Map
	r := New(Options{})
	r.Registrar("p").OnCommand("echo", func(_ context.Context, ev *bot.Event, _ bot.Reply) error {
		counter.Store(ev.ID, true)
		return nil
	})

	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				ev := textEvent("/echo hi")
				ev.ID = fmt.Sprintf("ev-%d-%d", worker, i)
				if err := r.Dispatch(context.Background(), ev); err != nil {
					t.Errorf("Dispatch 返回错误: %v", err)
					return
				}
			}
		}()
	}
	// 注册线程与分发线程并发执行。
	wg.Add(1)
	go func() {
		defer wg.Done()
		reg := r.Registrar("q")
		for i := range 20 {
			reg.OnCommand(fmt.Sprintf("cmd%d", i), tagHandler(&recorder{}, "x"))
		}
		_ = r.Rules()
		_ = r.RegistrationErrors()
	}()
	wg.Wait()

	count := 0
	counter.Range(func(any, any) bool { count++; return true })
	if count != 400 {
		t.Errorf("处理事件数 = %d, 期望 400", count)
	}
	if got := len(r.Rules()); got != 21 {
		t.Errorf("规则数 = %d, 期望 21", got)
	}
}

func mustCompile(expr string) *regexp.Regexp {
	re, err := regexp.Compile(expr)
	if err != nil {
		panic(err)
	}
	return re
}
