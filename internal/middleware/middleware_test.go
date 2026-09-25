package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/internal/dedup"
	"github.com/RandomLemon/kei/internal/ratelimit"
	"github.com/RandomLemon/kei/pkg/bot"
)

// newEvent 构造一个字段齐全的测试事件。
func newEvent() *bot.Event {
	return &bot.Event{
		ID:       "evt-1",
		Type:     bot.EventMessage,
		Platform: "test",
		BotID:    "bot-1",
		Time:     time.Now().UTC(),
		Message:  &bot.Message{ID: "msg-1", Kind: bot.MessagePrivate},
		Sender:   &bot.User{ID: "user-1", Name: "用户"},
		Channel:  &bot.Channel{ID: "chan-1", Name: "群", Kind: bot.MessageGroup},
	}
}

// okHandler 是记录调用并返回 nil 的 handler。
func okHandler() bot.Handler {
	return func(context.Context, *bot.Event, bot.Reply) error { return nil }
}

// traceMW 返回一个在调用前后追加标记的中间件。
func traceMW(tag string, out *[]string) bot.Middleware {
	return func(next bot.Handler) bot.Handler {
		return func(ctx context.Context, e *bot.Event, r bot.Reply) error {
			*out = append(*out, tag+":before")
			err := next(ctx, e, r)
			*out = append(*out, tag+":after")
			return err
		}
	}
}

// TestChainOnionOrder 验证洋葱模型：mws[0] 最外层，最先进入、最后退出。
func TestChainOnionOrder(t *testing.T) {
	var seq []string
	h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
		seq = append(seq, "handler")
		return nil
	}, traceMW("a", &seq), traceMW("b", &seq), traceMW("c", &seq))

	if err := h(context.Background(), newEvent(), nil); err != nil {
		t.Fatalf("处理器返回错误: %v", err)
	}
	want := []string{"a:before", "b:before", "c:before", "handler", "c:after", "b:after", "a:after"}
	if len(seq) != len(want) {
		t.Fatalf("执行序列长度不符: got %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("执行序列不符: got %v, want %v", seq, want)
		}
	}
}

// TestChainEmptyPassthrough 验证空链返回透传中间件。
func TestChainEmptyPassthrough(t *testing.T) {
	called := 0
	h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
		called++
		return nil
	})
	if err := h(context.Background(), newEvent(), nil); err != nil {
		t.Fatalf("处理器返回错误: %v", err)
	}
	if called != 1 {
		t.Fatalf("透传链未调用 handler: called=%d", called)
	}
}

// TestRecoverPanic 验证 Recover 捕获 error 与非 error 两类 panic。
func TestRecoverPanic(t *testing.T) {
	cases := []struct {
		name string
		val  any
	}{
		{"string", "炸了"},
		{"error", errors.New("内部错误")},
		{"struct", struct{ Code int }{Code: 500}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
				panic(tc.val)
			}, Recover(slog.New(slog.DiscardHandler)))

			err := h(context.Background(), newEvent(), nil)
			if !errors.Is(err, ErrPanic) {
				t.Fatalf("panic 未被包装为 ErrPanic: %v", err)
			}
			if err == nil {
				t.Fatalf("panic 后应返回错误")
			}
		})
	}
}

// TestRecoverKeepsError 验证 Recover 不吞掉 next 正常返回的错误。
func TestRecoverKeepsError(t *testing.T) {
	sentinel := errors.New("业务失败")
	h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
		return sentinel
	}, Recover(slog.New(slog.DiscardHandler)))

	err := h(context.Background(), newEvent(), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("正常错误未被原样返回: %v", err)
	}
	if errors.Is(err, ErrPanic) {
		t.Fatalf("正常错误不应被标记为 panic: %v", err)
	}
}

// TestRecoverLogsStack 验证 panic 会被记录，且堆栈字段包含触发点。
func TestRecoverLogsStack(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := bot.WithRoute(context.Background(), &bot.Route{Plugin: "echo", RuleID: "r1"})

	h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
		panic("boom")
	}, Recover(logger))

	if err := h(ctx, newEvent(), nil); !errors.Is(err, ErrPanic) {
		t.Fatalf("期望 ErrPanic: %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry); err != nil {
		t.Fatalf("日志不是合法 JSON: %v (%q)", err, buf.String())
	}
	if entry["level"] != "ERROR" {
		t.Fatalf("panic 日志级别不符: %v", entry["level"])
	}
	if entry["plugin"] != "echo" || entry["rule"] != "r1" {
		t.Fatalf("panic 日志缺少路由字段: %v", entry)
	}
	stack, _ := entry["stack"].(string)
	if !strings.Contains(stack, "middleware_test") {
		t.Fatalf("堆栈未包含触发点: %q", stack)
	}
}

// TestLoggerFields 验证 Logger 输出结构化字段与错误级别。
func TestLoggerFields(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := bot.WithRoute(context.Background(), &bot.Route{Plugin: "weather", RuleID: "rule-7"})
	sentinel := errors.New("上游超时")

	h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
		return sentinel
	}, Logger(logger))

	if err := h(ctx, newEvent(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("Logger 不应改变错误: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("期望一条日志，实际 %d 条: %q", len(lines), buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("日志不是合法 JSON: %v", err)
	}
	want := map[string]any{
		"level":    "ERROR",
		"plugin":   "weather",
		"rule":     "rule-7",
		"platform": "test",
		"kind":     "group",
		"user":     "user-1",
		"channel":  "chan-1",
		"event_id": "evt-1",
	}
	for k, v := range want {
		if entry[k] != v {
			t.Errorf("日志字段 %s 不符: got %v, want %v", k, entry[k], v)
		}
	}
	if _, ok := entry["duration_ms"].(float64); !ok {
		t.Errorf("日志缺少 duration_ms: %v", entry["duration_ms"])
	}
	if entry["error"] != sentinel.Error() {
		t.Errorf("日志缺少 error 字段: %v", entry["error"])
	}
}

// TestLoggerSuccessIsDebug 验证成功调用记 Debug 级别。
func TestLoggerSuccessIsDebug(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := Apply(okHandler(), Logger(logger))

	if err := h(context.Background(), newEvent(), nil); err != nil {
		t.Fatalf("处理器返回错误: %v", err)
	}
	if !strings.Contains(buf.String(), `"level":"DEBUG"`) {
		t.Fatalf("成功调用应记 DEBUG: %q", buf.String())
	}
}

// lastEntry 解析最后一条 JSON 日志，便于断言多次记录后的最终级别。
func lastEntry(t *testing.T, buf *strings.Builder) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("日志不是合法 JSON: %v (%q)", err, lines[len(lines)-1])
	}
	return entry
}

// loggerCase 是 Logger 分级测试的一个用例。
type loggerCase struct {
	name       string
	wantLevel  string
	wantDenied bool
	wantRule   string
	run        func(ctx context.Context, log *slog.Logger) error
}

// TestLoggerErrorLevels 验证 Logger 按错误类型分级。
//
// 预期的策略拒绝（去重/限流/鉴权）记 WARN 并带 decision=denied；
// 超时、panic 与普通错误记 ERROR 且不带 decision，避免把正常拒绝当成故障。
func TestLoggerErrorLevels(t *testing.T) {
	adminRoute := &bot.Route{Plugin: "admin", RuleID: "reboot", AdminOnly: true}
	plainRoute := &bot.Route{Plugin: "echo", RuleID: "hi"}

	cases := []loggerCase{
		{
			name:      "未经授权的拒绝记 WARN",
			wantLevel: "WARN", wantDenied: true, wantRule: "reboot",
			run: func(ctx context.Context, log *slog.Logger) error {
				h := Apply(okHandler(), Logger(log), Auth(func(*bot.Event) bool { return false }))
				return h(bot.WithRoute(ctx, adminRoute), newEvent(), nil)
			},
		},
		{
			name:      "限流拒绝记 WARN",
			wantLevel: "WARN", wantDenied: true, wantRule: "hi",
			run: func(ctx context.Context, log *slog.Logger) error {
				limiter := ratelimit.New(0.01, 1, time.Minute)
				// 先耗尽令牌，使下一次 Allow 必定失败。
				if !limiter.Allow("user-1") {
					t.Fatal("预消耗令牌失败")
				}
				h := Apply(okHandler(), Logger(log), RateLimit(limiter, ScopeUser))
				return h(bot.WithRoute(ctx, plainRoute), newEvent(), nil)
			},
		},
		{
			name:      "重复事件记 WARN",
			wantLevel: "WARN", wantDenied: true, wantRule: "hi",
			run: func(ctx context.Context, log *slog.Logger) error {
				h := Apply(okHandler(), Logger(log), Dedup(dedup.New(16, time.Minute)))
				ctx = bot.WithRoute(ctx, plainRoute)
				if err := h(ctx, newEvent(), nil); err != nil {
					t.Fatalf("首次事件不应被拦截: %v", err)
				}
				return h(ctx, newEvent(), nil)
			},
		},
		{
			name:      "超时记 ERROR",
			wantLevel: "ERROR", wantRule: "hi",
			run: func(ctx context.Context, log *slog.Logger) error {
				h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
					time.Sleep(50 * time.Millisecond)
					return nil
				}, Logger(log), Timeout(5*time.Millisecond))
				return h(bot.WithRoute(ctx, plainRoute), newEvent(), nil)
			},
		},
		{
			name:      "panic 记 ERROR",
			wantLevel: "ERROR", wantRule: "hi",
			run: func(ctx context.Context, log *slog.Logger) error {
				h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
					panic("组件崩溃")
				}, Logger(log), Recover(log))
				return h(bot.WithRoute(ctx, plainRoute), newEvent(), nil)
			},
		},
		{
			name:      "普通错误记 ERROR",
			wantLevel: "ERROR", wantRule: "hi",
			run: func(ctx context.Context, log *slog.Logger) error {
				h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
					return errors.New("上游 500")
				}, Logger(log))
				return h(bot.WithRoute(ctx, plainRoute), newEvent(), nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			if err := tc.run(context.Background(), logger); err == nil {
				t.Fatal("期望 handler 返回错误")
			}
			entry := lastEntry(t, &buf)
			if entry["level"] != tc.wantLevel {
				t.Fatalf("日志级别不符: got %v, want %v (%v)", entry["level"], tc.wantLevel, entry)
			}
			if got, ok := entry["decision"]; ok != tc.wantDenied {
				t.Fatalf("decision 字段不符: got %v (want 存在=%v), 日志 %v", got, tc.wantDenied, entry)
			} else if tc.wantDenied && got != "denied" {
				t.Fatalf("decision 取值不符: %v", got)
			}
			if _, ok := entry["error"]; !ok {
				t.Fatalf("日志缺少 error 字段: %v", entry)
			}
			if entry["rule"] != tc.wantRule {
				t.Fatalf("rule 字段不符: got %v, want %v", entry["rule"], tc.wantRule)
			}
		})
	}
}

// TestTimeoutBlocks 验证超时返回 ErrTimeout、耗时受控且不泄漏 goroutine。
func TestTimeoutBlocks(t *testing.T) {
	base := runtime.NumGoroutine()
	release := make(chan struct{})
	defer close(release)

	h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
		// 故意无视 ctx：验证超时返回后 goroutine 仍能退出。
		<-release
		return nil
	}, Timeout(50*time.Millisecond))

	start := time.Now()
	err := h(context.Background(), newEvent(), nil)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("期望 ErrTimeout: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ErrTimeout 应包装 ctx.Err(): %v", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("超时耗时过长: %v", elapsed)
	}

	// 释放阻塞的 handler，验证其 goroutine 能正常退出（未卡在 chan 发送上）。
	release <- struct{}{}
	waitGoroutines(t, base)
}

// TestTimeoutFastHandler 验证未超时时的正常路径与错误透传。
func TestTimeoutFastHandler(t *testing.T) {
	h := Apply(func(context.Context, *bot.Event, bot.Reply) error { return nil }, Timeout(time.Second))
	if err := h(context.Background(), newEvent(), nil); err != nil {
		t.Fatalf("快速 handler 不应超时: %v", err)
	}

	sentinel := errors.New("快速失败")
	h = Apply(func(context.Context, *bot.Event, bot.Reply) error { return sentinel }, Timeout(time.Second))
	if err := h(context.Background(), newEvent(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("handler 错误应原样返回: %v", err)
	}
}

// TestTimeoutDisabled 验证 d <= 0 时不限时。
func TestTimeoutDisabled(t *testing.T) {
	h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
		time.Sleep(20 * time.Millisecond)
		return nil
	}, Timeout(0))
	if err := h(context.Background(), newEvent(), nil); err != nil {
		t.Fatalf("禁用超时后不应报错: %v", err)
	}
}

// TestDedupColdAndHot 验证首次放行、同规则重复拦截、不同规则互不影响。
func TestDedupColdAndHot(t *testing.T) {
	set := dedup.New(64, time.Minute)
	sink := &countingHandler{}
	chain := Chain(Dedup(set))

	ev := newEvent()
	ctxA := bot.WithRoute(context.Background(), &bot.Route{Plugin: "p", RuleID: "rule-a"})
	ctxB := bot.WithRoute(context.Background(), &bot.Route{Plugin: "p", RuleID: "rule-b"})

	// 同一事件的第一条规则：放行。
	if err := chain(sink.handle)(ctxA, ev, nil); err != nil {
		t.Fatalf("首次事件被拦截: %v", err)
	}
	// 同一事件的第二条规则：必须放行，否则多规则路由会互相误伤。
	if err := chain(sink.handle)(ctxB, ev, nil); err != nil {
		t.Fatalf("不同规则被误判重复: %v", err)
	}
	// 同一规则再次收到同一事件：拦截。
	err := chain(sink.handle)(ctxA, ev, nil)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("同规则重复事件未被拦截: %v", err)
	}
	if sink.calls != 2 {
		t.Fatalf("handler 调用次数不符: got %d, want 2", sink.calls)
	}
}

// TestDedupNoRoute 验证无路由信息时退化为事件 ID 去重。
func TestDedupNoRoute(t *testing.T) {
	set := dedup.New(64, time.Minute)
	sink := &countingHandler{}
	h := Apply(sink.handle, Dedup(set))
	ctx := context.Background()

	if err := h(ctx, newEvent(), nil); err != nil {
		t.Fatalf("首次事件被拦截: %v", err)
	}
	if err := h(ctx, newEvent(), nil); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("重复事件未被拦截: %v", err)
	}
}

// TestDedupFallbackKey 验证事件 ID 为空时按会话与消息 ID 组合去重。
func TestDedupFallbackKey(t *testing.T) {
	set := dedup.New(64, time.Minute)
	sink := &countingHandler{}
	h := Apply(sink.handle, Dedup(set))
	ctx := context.Background()

	first := newEvent()
	first.ID = ""
	if err := h(ctx, first, nil); err != nil {
		t.Fatalf("首次事件被拦截: %v", err)
	}
	// 同会话、同消息 ID 的重复投递：拦截。
	same := newEvent()
	same.ID = ""
	if err := h(ctx, same, nil); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("同消息重复未被拦截: %v", err)
	}
	// 同一会话中的另一条消息：放行。
	other := newEvent()
	other.ID = ""
	other.Message = &bot.Message{ID: "msg-2", Kind: bot.MessagePrivate}
	if err := h(ctx, other, nil); err != nil {
		t.Fatalf("新消息被误判重复: %v", err)
	}
	if sink.calls != 2 {
		t.Fatalf("handler 调用次数不符: got %d, want 2", sink.calls)
	}
}

// TestDedupDisabled 验证 set 为 nil 时透传。
func TestDedupDisabled(t *testing.T) {
	sink := &countingHandler{}
	h := Apply(sink.handle, Dedup(nil))
	ev := newEvent()
	for range 3 {
		if err := h(context.Background(), ev, nil); err != nil {
			t.Fatalf("禁用去重后不应拦截: %v", err)
		}
	}
	if sink.calls != 3 {
		t.Fatalf("handler 调用次数不符: got %d, want 3", sink.calls)
	}
}

// TestRateLimitExhausted 验证令牌耗尽后被限流。
func TestRateLimitExhausted(t *testing.T) {
	limiter := ratelimit.New(0.01, 1, time.Minute)
	h := Apply(okHandler(), RateLimit(limiter, ScopeUser))
	ctx := context.Background()

	if err := h(ctx, newEvent(), nil); err != nil {
		t.Fatalf("首个请求应放行: %v", err)
	}
	if err := h(ctx, newEvent(), nil); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("令牌耗尽后应限流: %v", err)
	}
}

// TestRateLimitScope 验证不同 scope 使用不同的限流键。
func TestRateLimitScope(t *testing.T) {
	other := func() *bot.Event {
		ev := newEvent()
		ev.Sender = &bot.User{ID: "user-2", Name: "另一个用户"}
		ev.Channel = &bot.Channel{ID: "chan-2", Kind: bot.MessageGroup}
		return ev
	}

	t.Run("channel", func(t *testing.T) {
		h := Apply(okHandler(), RateLimit(ratelimit.New(0.01, 1, time.Minute), ScopeChannel))
		// 同一会话、不同用户共享一个桶：第二个请求被拦。
		if err := h(context.Background(), newEvent(), nil); err != nil {
			t.Fatalf("首个请求应放行: %v", err)
		}
		sameChannel := newEvent()
		sameChannel.Sender = &bot.User{ID: "user-2"}
		if err := h(context.Background(), sameChannel, nil); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("同会话未共享配额: %v", err)
		}
		// 另一会话独立配额：放行。
		if err := h(context.Background(), other(), nil); err != nil {
			t.Fatalf("不同会话不应共享配额: %v", err)
		}
	})

	t.Run("plugin", func(t *testing.T) {
		h := Apply(okHandler(), RateLimit(ratelimit.New(0.01, 1, time.Minute), ScopePlugin))
		ctxA := bot.WithRoute(context.Background(), &bot.Route{Plugin: "p-a", RuleID: "r1"})
		ctxB := bot.WithRoute(context.Background(), &bot.Route{Plugin: "p-b", RuleID: "r2"})
		if err := h(ctxA, newEvent(), nil); err != nil {
			t.Fatalf("首个插件请求应放行: %v", err)
		}
		// 同一插件的另一条规则共享配额。
		if err := h(bot.WithRoute(context.Background(), &bot.Route{Plugin: "p-a", RuleID: "r2"}), newEvent(), nil); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("同插件未共享配额: %v", err)
		}
		if err := h(ctxB, newEvent(), nil); err != nil {
			t.Fatalf("不同插件不应共享配额: %v", err)
		}
	})

	t.Run("channel-fallback-session", func(t *testing.T) {
		h := Apply(okHandler(), RateLimit(ratelimit.New(0.01, 1, time.Minute), ScopeChannel))
		ev := newEvent()
		ev.Channel = nil // 私聊：回退到 SessionKey
		if err := h(context.Background(), ev, nil); err != nil {
			t.Fatalf("首个请求应放行: %v", err)
		}
		if err := h(context.Background(), ev, nil); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("无会话 ID 时未按 SessionKey 限流: %v", err)
		}
	})
}

// TestRateLimitDisabled 验证 limiter 为 nil 时透传。
func TestRateLimitDisabled(t *testing.T) {
	sink := &countingHandler{}
	h := Apply(sink.handle, RateLimit(nil, ScopeUser))
	for range 3 {
		if err := h(context.Background(), newEvent(), nil); err != nil {
			t.Fatalf("禁用限流后不应拦截: %v", err)
		}
	}
	if sink.calls != 3 {
		t.Fatalf("handler 调用次数不符: got %d, want 3", sink.calls)
	}
}

// TestAuth 验证管理员校验。
func TestAuth(t *testing.T) {
	adminEvent := newEvent()
	adminEvent.Sender = &bot.User{ID: "root"}
	isAdmin := func(e *bot.Event) bool { return e.Sender != nil && e.Sender.ID == "root" }

	adminCtx := bot.WithRoute(context.Background(), &bot.Route{Plugin: "admin", RuleID: "reboot", AdminOnly: true})
	plainCtx := bot.WithRoute(context.Background(), &bot.Route{Plugin: "echo", RuleID: "echo"})

	sink := &countingHandler{}
	h := Apply(sink.handle, Auth(isAdmin))

	// 管理员放行。
	if err := h(adminCtx, adminEvent, nil); err != nil {
		t.Fatalf("管理员被拦截: %v", err)
	}
	// 非管理员拦截。
	if err := h(adminCtx, newEvent(), nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("非管理员未被拦截: %v", err)
	}
	// 非管理员专属规则：普通用户放行。
	if err := h(plainCtx, newEvent(), nil); err != nil {
		t.Fatalf("普通规则被拦截: %v", err)
	}
	// 未注入路由：不校验，放行。
	if err := h(context.Background(), newEvent(), nil); err != nil {
		t.Fatalf("无路由信息时被拦截: %v", err)
	}
	// isAdmin 为 nil 时管理员专属规则一律拒绝。
	h = Apply(sink.handle, Auth(nil))
	if err := h(adminCtx, adminEvent, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("nil 断言函数未拒绝管理员规则: %v", err)
	}
	if sink.calls != 3 {
		t.Fatalf("handler 调用次数不符: got %d, want 3", sink.calls)
	}
}

// fakeRecorder 记录 Metrics 中间件的上报内容。
type fakeRecorder struct {
	calls        int
	matchedCalls int
	gotMatch     struct{ plugin, rule string }
	got          struct {
		plugin, rule string
		d            time.Duration
		err          error
	}
}

// EventHandled 实现 metrics.Recorder。
func (f *fakeRecorder) EventHandled(plugin, rule string, d time.Duration, err error) {
	f.calls++
	f.got.plugin, f.got.rule, f.got.d, f.got.err = plugin, rule, d, err
}

// EventPublished 实现 metrics.Recorder（本包不使用，留空）。
func (f *fakeRecorder) EventPublished(string) {}

// EventDropped 实现 metrics.Recorder（本包不使用，留空）。
func (f *fakeRecorder) EventDropped(string, string) {}

// MessageSent 实现 metrics.Recorder（本包不使用，留空）。
func (f *fakeRecorder) MessageSent(string, time.Duration, error) {}

// RuleMatched 实现 metrics.Recorder。
func (f *fakeRecorder) RuleMatched(plugin, rule string) {
	f.matchedCalls++
	f.gotMatch.plugin, f.gotMatch.rule = plugin, rule
}

// AdapterReconnected 实现 metrics.Recorder（本包不使用，留空）。
func (f *fakeRecorder) AdapterReconnected(string) {}

// AdapterReconnectFailed 实现 metrics.Recorder（本包不使用，留空）。
func (f *fakeRecorder) AdapterReconnectFailed(string) {}

// AdapterDisabled 实现 metrics.Recorder（本包不使用，留空）。
func (f *fakeRecorder) AdapterDisabled(string) {}

// TestMetricsMiddleware 验证指标中间件上报插件、规则、耗时与错误。
func TestMetricsMiddleware(t *testing.T) {
	rec := &fakeRecorder{}
	ctx := bot.WithRoute(context.Background(), &bot.Route{Plugin: "weather", RuleID: "today"})
	sentinel := errors.New("下游失败")

	// 成功路径。
	h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
		time.Sleep(2 * time.Millisecond)
		return nil
	}, Metrics(rec))
	if err := h(ctx, newEvent(), nil); err != nil {
		t.Fatalf("处理器返回错误: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("上报次数不符: got %d, want 1", rec.calls)
	}
	if rec.matchedCalls != 1 || rec.gotMatch.plugin != "weather" || rec.gotMatch.rule != "today" {
		t.Fatalf("规则命中上报不符: calls=%d %+v", rec.matchedCalls, rec.gotMatch)
	}
	if rec.got.plugin != "weather" || rec.got.rule != "today" {
		t.Fatalf("上报标签不符: %+v", rec.got)
	}
	if rec.got.err != nil {
		t.Fatalf("成功路径不应记录错误: %v", rec.got.err)
	}
	if rec.got.d <= 0 {
		t.Fatalf("耗时应为正数: %v", rec.got.d)
	}

	// 失败路径：错误透传且被记录。
	h = Apply(func(context.Context, *bot.Event, bot.Reply) error { return sentinel }, Metrics(rec))
	if err := h(ctx, newEvent(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("Metrics 不应改变错误: %v", err)
	}
	if !errors.Is(rec.got.err, sentinel) {
		t.Fatalf("未记录 handler 错误: %v", rec.got.err)
	}

	// 无路由信息：使用占位标签。
	h = Apply(okHandler(), Metrics(rec))
	if err := h(context.Background(), newEvent(), nil); err != nil {
		t.Fatalf("处理器返回错误: %v", err)
	}
	if rec.got.plugin != DefaultLabel || rec.got.rule != "" {
		t.Fatalf("缺省标签不符: %+v", rec.got)
	}
}

// TestMetricsDisabled 验证 rec 为 nil 时透传且不 panic。
func TestMetricsDisabled(t *testing.T) {
	sink := &countingHandler{}
	h := Apply(sink.handle, Metrics(nil))
	if err := h(context.Background(), newEvent(), nil); err != nil {
		t.Fatalf("禁用指标后不应报错: %v", err)
	}
	if sink.calls != 1 {
		t.Fatalf("handler 调用次数不符: got %d, want 1", sink.calls)
	}
}

// TestCombinedChain 验证典型链路的组合行为：限流 + 去重 + 指标 + recover。
func TestCombinedChain(t *testing.T) {
	rec := &fakeRecorder{}
	var seq []string
	ev := newEvent()
	ctx := bot.WithRoute(context.Background(), &bot.Route{Plugin: "p", RuleID: "r"})

	h := Apply(func(context.Context, *bot.Event, bot.Reply) error {
		seq = append(seq, "handler")
		return nil
	},
		Metrics(rec),
		RateLimit(ratelimit.New(0.01, 1, time.Minute), ScopeUser),
		Dedup(dedup.New(16, time.Minute)),
		Recover(slog.New(slog.DiscardHandler)),
	)

	if err := h(ctx, ev, nil); err != nil {
		t.Fatalf("首次调用应成功: %v", err)
	}
	// 第二次被限流拦住（限流在去重外层）。
	err := h(ctx, ev, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("第二次调用应被限流: %v", err)
	}
	if len(seq) != 1 {
		t.Fatalf("handler 只应执行一次: %v", seq)
	}
	if rec.calls != 2 {
		t.Fatalf("指标应记录两次调用: got %d", rec.calls)
	}
	// 两条路径都进入了链：限流在 Metrics 内层，因此第二次同样算作命中。
	if rec.matchedCalls != 2 {
		t.Fatalf("规则命中应记录两次: got %d", rec.matchedCalls)
	}
}

// countingHandler 统计被调用次数。
type countingHandler struct{ calls int }

// handle 实现 bot.Handler。
func (c *countingHandler) handle(context.Context, *bot.Event, bot.Reply) error {
	c.calls++
	return nil
}

// waitGoroutines 轮询等待 goroutine 数量回落到上限以内。
func waitGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		if got := runtime.NumGoroutine(); got <= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine 未回收: got %d, want <= %d", runtime.NumGoroutine(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
