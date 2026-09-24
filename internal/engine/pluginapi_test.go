package engine

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/RandomLemon/kei/adapters/mock"
	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

func newTestEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	if opts.Config == nil {
		opts.Config = &config.Config{
			Bots:    []config.BotConfig{{Name: "mock-main", Adapter: "mock"}},
			Plugins: map[string]config.PluginConfig{},
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Adapters == nil {
		opts.Adapters = []AdapterBinding{{BotID: "mock-main", Adapter: mock.New(mock.Options{Name: "mock-main"})}}
	}
	eng, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

func TestPluginAPIPermissionGates(t *testing.T) {
	eng := newTestEngine(t, Options{})
	ctx := context.Background()
	msg := &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi"}},
	}}
	target := bot.Target{Platform: "mock", ChannelID: "c1"}

	t.Run("未声明 send_message 时主动发送被拒绝", func(t *testing.T) {
		api := eng.pluginAPI("plain", bot.Metadata{Name: "plain"})
		if _, err := api.Send(ctx, target, msg); err == nil {
			t.Fatal("应返回权限错误")
		} else if !contains2(err.Error(), string(bot.PermSendMessage)) {
			t.Fatalf("错误信息应包含权限名: %v", err)
		}
		// 回复当前会话不需要额外权限。
		if _, err := api.Reply(ctx, &bot.Event{Platform: "mock", BotID: "mock-main"}, msg); err != nil {
			t.Fatalf("Reply 不应被权限拒绝: %v", err)
		}
	})

	t.Run("声明 send_message 后允许发送", func(t *testing.T) {
		api := eng.pluginAPI("sender", bot.Metadata{
			Name:        "sender",
			Permissions: []bot.Permission{bot.PermSendMessage},
		})
		if _, err := api.Send(ctx, target, msg); err != nil {
			t.Fatalf("Send: %v", err)
		}
	})

	t.Run("存储权限按声明裁剪", func(t *testing.T) {
		denied := eng.pluginAPI("nostore", bot.Metadata{Name: "nostore"}).Storage()
		if err := denied.Set(ctx, "k", []byte("v"), time.Minute); !errors.Is(err, storage.ErrPermissionDenied) {
			t.Fatalf("未声明 storage 权限时 Set = %v", err)
		}

		allowed := eng.pluginAPI("store", bot.Metadata{
			Name:        "store",
			Permissions: []bot.Permission{bot.PermStorage},
		}).Storage()
		if err := allowed.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
			t.Fatalf("声明 storage 权限后 Set = %v", err)
		}
		got, err := allowed.Get(ctx, "k")
		if err != nil || string(got) != "v" {
			t.Fatalf("Get = %q, %v", got, err)
		}
	})

	t.Run("日志器绑定插件字段", func(t *testing.T) {
		api := eng.pluginAPI("logger", bot.Metadata{Name: "logger"})
		if api.Logger() == nil {
			t.Fatal("Logger 不应为 nil")
		}
	})
}

func TestGlobalMiddlewareChainOrder(t *testing.T) {
	eng := newTestEngine(t, Options{})
	mws := eng.globalMiddlewares()
	if len(mws) != 8 {
		t.Fatalf("全局中间件数量 = %d, want 8（含 Timeout 内外两层 Recover）", len(mws))
	}

	var order []string
	h := func(context.Context, *bot.Event, bot.Reply) error {
		order = append(order, "handler")
		return nil
	}
	rule := &bot.Rule{ID: "r1", Plugin: "p", Handler: h}
	wrapped := applyMiddlewares(rule, mws)

	ev := &bot.Event{ID: "e1", Type: bot.EventMessage, Platform: "mock", BotID: "mock-main", Sender: &bot.User{ID: "u1"}}
	if err := wrapped(context.Background(), ev, bot.NewNoopReply()); err != nil {
		t.Fatalf("执行链返回错误: %v", err)
	}
	if len(order) == 0 || order[len(order)-1] != "handler" {
		t.Fatalf("执行顺序 = %v", order)
	}
}

// applyMiddlewares 复刻路由器的折叠语义，用于验证全局链可执行且 Handler 在最后。
func applyMiddlewares(rule *bot.Rule, mws []bot.Middleware) bot.Handler {
	h := rule.Handler
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func TestIsAdmin(t *testing.T) {
	eng := newTestEngine(t, Options{
		Config: &config.Config{
			Bots: []config.BotConfig{{Name: "mock-main", Adapter: "mock"}},
			Auth: config.AuthConfig{AdminUsers: []string{"admin-1"}},
		},
	})

	if eng.isAdmin(nil) {
		t.Fatal("nil 事件不是管理员")
	}
	if eng.isAdmin(&bot.Event{Sender: &bot.User{ID: "user-1"}}) {
		t.Fatal("普通用户不是管理员")
	}
	if !eng.isAdmin(&bot.Event{Sender: &bot.User{ID: "admin-1"}}) {
		t.Fatal("名单内用户应为管理员")
	}
}

func TestSendLimiterIsApplied(t *testing.T) {
	eng := newTestEngine(t, Options{
		Config: &config.Config{
			Bots:   []config.BotConfig{{Name: "mock-main", Adapter: "mock"}},
			Limits: config.LimitsConfig{SendRate: 1, SendBurst: 1},
		},
	})
	if _, ok := eng.sendLimits["mock-main"]; !ok {
		t.Fatal("配置了 send_rate 后应创建限流器")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	target := bot.Target{Platform: "mock", BotID: "mock-main", ChannelID: "c1"}
	msg := &bot.Message{Kind: bot.MessagePrivate, Segments: []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi"}},
	}}

	if _, err := eng.Send(ctx, target, msg); err != nil {
		t.Fatalf("首次发送应成功: %v", err)
	}
	// 令牌耗尽：等待期间 ctx 超时，发送应失败而不是无限阻塞。
	if _, err := eng.Send(ctx, target, msg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("限流等待应受 ctx 约束, got %v", err)
	}
}

func TestEventSinkMarksMetricsAndRejectsNil(t *testing.T) {
	eng := newTestEngine(t, Options{})
	sink := &eventSink{eng: eng}

	if err := sink.Emit(context.Background(), nil); err == nil {
		t.Fatal("nil 事件应返回错误")
	}
	if err := sink.Emit(context.Background(), &bot.Event{ID: "e1", Platform: "mock"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := eng.bus.Stats().Published; got != 1 {
		t.Fatalf("Published = %d, want 1", got)
	}
}

func TestTemplateDefaults(t *testing.T) {
	if got := orDuration(0, time.Second); got != time.Second {
		t.Fatalf("orDuration 默认值 = %v", got)
	}
	if got := orInt(0, 4); got != 4 {
		t.Fatalf("orInt 默认值 = %d", got)
	}
	if !contains([]string{"a", "b"}, "b") || contains([]string{"a"}, "c") {
		t.Fatal("contains 判定错误")
	}
}

func contains2(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
