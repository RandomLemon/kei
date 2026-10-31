package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/pkg/bot"
)

// retryAdapter 记录 Send 调用次数并始终失败。
type retryAdapter struct {
	calls atomic.Int32
}

func (a *retryAdapter) Name() string { return "retry" }
func (a *retryAdapter) Start(context.Context, bot.EventSink) error {
	return nil
}
func (a *retryAdapter) Stop(context.Context) error { return nil }
func (a *retryAdapter) Capabilities() bot.Capabilities {
	return bot.Capabilities{Text: true, Private: true}
}
func (a *retryAdapter) Send(context.Context, *bot.SendRequest) (*bot.SendResult, error) {
	a.calls.Add(1)
	return nil, errors.New("boom")
}

// TestSendRetriesRespectRateLimit 验证重试也要重新取得发送令牌：令牌耗尽时
// 第二次尝试应等待限流并在 ctx 到期后放弃，而不是直接再调一次适配器。
func TestSendRetriesRespectRateLimit(t *testing.T) {
	ad := &retryAdapter{}
	eng := newTestEngine(t, Options{
		Config: &config.Config{
			Limits: config.LimitsConfig{SendRate: 1, SendBurst: 1},
		},
		Adapters:    []AdapterBinding{{BotID: "retry", Adapter: ad}},
		SendRetries: 1,
		SendBackoff: time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	msg := &bot.Message{Kind: bot.MessagePrivate, Segments: []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi"}},
	}}
	if _, err := eng.Send(ctx, bot.Target{Platform: "retry", BotID: "retry"}, msg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send 错误 = %v, want context.DeadlineExceeded", err)
	}
	if got := ad.calls.Load(); got != 1 {
		t.Fatalf("令牌耗尽后不应再次调用适配器: calls=%d", got)
	}
}

// TestSendRetriesCallAdapterPerAttempt 验证 SendRetries 确实产生 N+1 次调用，
// 且退避封顶后仍能结束。
func TestSendRetriesCallAdapterPerAttempt(t *testing.T) {
	ad := &retryAdapter{}
	eng := newTestEngine(t, Options{
		Config:      &config.Config{},
		Adapters:    []AdapterBinding{{BotID: "retry", Adapter: ad}},
		SendRetries: 2,
		SendBackoff: time.Millisecond,
	})
	msg := &bot.Message{Kind: bot.MessagePrivate, Segments: []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi"}},
	}}
	if _, err := eng.Send(context.Background(), bot.Target{Platform: "retry", BotID: "retry"}, msg); err == nil {
		t.Fatal("适配器始终失败时 Send 应返回错误")
	}
	if got := ad.calls.Load(); got != 3 {
		t.Fatalf("Send 调用次数 = %d, want 3", got)
	}
}
