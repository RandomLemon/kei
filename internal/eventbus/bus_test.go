package eventbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// newEvent 构造一个属于 session 会话的消息事件。
func newEvent(session, id string) *bot.Event {
	return &bot.Event{
		ID:       id,
		Type:     bot.EventMessage,
		Platform: "test",
		BotID:    "bot",
		Time:     time.Now().UTC(),
		Message: &bot.Message{
			ID:   id,
			Kind: bot.MessageGroup,
			Segments: []bot.Segment{
				{Type: bot.SegText, Data: map[string]any{bot.KeyText: id}},
			},
		},
		Sender:  &bot.User{ID: session, Name: session},
		Channel: &bot.Channel{ID: session, Name: session, Kind: bot.MessageGroup},
	}
}

// sessionKey 返回 newEvent(session, "") 的会话键，用于挑选分片。
func sessionKey(session string) string {
	return "test:bot:" + session + ":" + session
}

// pickSessions 返回 n 个落在互不相同分片上的会话名。
//
// 用于构造"不同会话并行处理"的输入：只有落在不同分片上的会话才可能并行。
func pickSessions(t *testing.T, n, shards int) []string {
	t.Helper()
	seen := make(map[int]struct{}, n)
	out := make([]string, 0, n)
	for i := range 10000 {
		if len(out) == n {
			return out
		}
		name := fmt.Sprintf("session-%d", i)
		idx := shardIndex(sessionKey(name), shards)
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		out = append(out, name)
	}
	t.Fatalf("找不到 %d 个落在不同分片的会话（分片数 %d）", n, shards)
	return nil
}

// testLogger 返回丢弃输出的 logger，避免测试输出噪音。
func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// gate 是一个可重复释放、只关闭一次的通道，用于在测试中阻塞/放行处理器。
type gate struct {
	once sync.Once
	ch   chan struct{}
}

// newGate 构造一个未释放的 gate。
func newGate() *gate { return &gate{ch: make(chan struct{})} }

// wait 阻塞直到 gate 被释放。
func (g *gate) wait() { <-g.ch }

// release 释放 gate；重复调用安全。
func (g *gate) release() { g.once.Do(func() { close(g.ch) }) }

// recorder 并发安全地记录处理器收到的事件。
type recorder struct {
	mu   sync.Mutex
	ids  []string
	keys []string
}

// handler 实现事件处理器签名。
func (r *recorder) handler(_ context.Context, ev *bot.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, ev.ID)
	r.keys = append(r.keys, ev.SessionKey())
}

// snapshot 返回已记录 ID 的副本。
func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// mustSubscribe 注册处理器，失败即终止测试。
func mustSubscribe(t *testing.T, b *Bus, h func(context.Context, *bot.Event)) {
	t.Helper()
	if err := b.Subscribe(h); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
}

// mustPublish 投递事件并断言无错误。
func mustPublish(t *testing.T, b *Bus, ev *bot.Event) {
	t.Helper()
	if err := b.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish(%q): %v", ev.ID, err)
	}
}

// closeBus 在超时保护下关闭总线，失败即终止测试。
func closeBus(t *testing.T, b *Bus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// assertIDs 断言实际记录与期望完全一致。
func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("处理事件数 = %d, 期望 %d；实际 %v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个事件 = %q, 期望 %q；实际顺序 %v", i, got[i], want[i], got)
		}
	}
}

// ---------------------------------------------------------------------------
// 保序与并行
// ---------------------------------------------------------------------------

// TestPublishPreservesOrderPerSession 验证同一会话的事件严格保序。
func TestPublishPreservesOrderPerSession(t *testing.T) {
	b := New(Options{Workers: 4, QueueSize: 512, Logger: testLogger()})
	defer closeBus(t, b)

	rec := &recorder{}
	mustSubscribe(t, b, rec.handler)

	const n = 200
	want := make([]string, 0, n)
	for i := range n {
		id := fmt.Sprintf("e%03d", i)
		mustPublish(t, b, newEvent("ordered", id))
		want = append(want, id)
	}

	closeBus(t, b)
	assertIDs(t, rec.snapshot(), want)

	if got := b.Stats().Handled; got != n {
		t.Fatalf("Handled = %d, 期望 %d", got, n)
	}
}

// TestSameSessionLandsOnSingleShard 验证多会话并发投递时各会话仍独立保序。
func TestSameSessionLandsOnSingleShard(t *testing.T) {
	b := New(Options{Workers: 8, QueueSize: 512, Logger: testLogger()})
	defer closeBus(t, b)

	var mu sync.Mutex
	got := make(map[string][]string, 4)
	mustSubscribe(t, b, func(_ context.Context, ev *bot.Event) {
		mu.Lock()
		key := ev.SessionKey()
		got[key] = append(got[key], ev.ID)
		mu.Unlock()
	})

	const sessions, perSession = 4, 50
	errs := make(chan error, sessions)
	var wg sync.WaitGroup
	for s := range sessions {
		session := fmt.Sprintf("sess-%d", s)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perSession {
				ev := newEvent(session, fmt.Sprintf("s%d-e%03d", s, i))
				if err := b.Publish(context.Background(), ev); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Publish: %v", err)
	}
	closeBus(t, b)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != sessions {
		t.Fatalf("观察到 %d 个会话, 期望 %d", len(got), sessions)
	}
	for s := range sessions {
		key := sessionKey(fmt.Sprintf("sess-%d", s))
		ids := got[key]
		if len(ids) != perSession {
			t.Fatalf("会话 %s 处理 %d 条, 期望 %d", key, len(ids), perSession)
		}
		for i, id := range ids {
			want := fmt.Sprintf("s%d-e%03d", s, i)
			if id != want {
				t.Fatalf("会话 %s 第 %d 条 = %q, 期望 %q", key, i, id, want)
			}
		}
	}
}

// TestDifferentSessionsRunConcurrently 验证不同会话由不同 goroutine 并行处理。
func TestDifferentSessionsRunConcurrently(t *testing.T) {
	const workers = 4
	b := New(Options{Workers: workers, QueueSize: 8, Logger: testLogger()})
	defer closeBus(t, b)

	sessions := pickSessions(t, 2, workers)
	entered := make(chan string, 2)
	release := newGate()
	defer release.release()

	mustSubscribe(t, b, func(_ context.Context, ev *bot.Event) {
		entered <- ev.ID
		release.wait()
	})

	for i, s := range sessions {
		mustPublish(t, b, newEvent(s, fmt.Sprintf("par-%d", i)))
	}

	// 若两个会话被串行处理，第二个事件永远无法"进入"处理器。
	seen := make(map[string]bool, 2)
	timeout := time.After(5 * time.Second)
	for range 2 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-timeout:
			t.Fatalf("不同会话未并行处理：仅观察到 %v 进入处理器", seen)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("进入处理器的事件 = %v, 期望两个不同事件", seen)
	}
}

// ---------------------------------------------------------------------------
// 去重
// ---------------------------------------------------------------------------

// TestDuplicateIDHandledOnce 验证同一 ID 只被处理一次且不返回错误。
func TestDuplicateIDHandledOnce(t *testing.T) {
	b := New(Options{Workers: 2, QueueSize: 64, Logger: testLogger()})
	defer closeBus(t, b)

	rec := &recorder{}
	mustSubscribe(t, b, rec.handler)

	mustPublish(t, b, newEvent("s", "dup"))
	mustPublish(t, b, newEvent("s", "dup"))
	mustPublish(t, b, newEvent("s", "dup"))
	mustPublish(t, b, newEvent("s", "other"))

	closeBus(t, b)

	// 顺序断言依赖同一会话单分片 FIFO。
	assertIDs(t, rec.snapshot(), []string{"dup", "other"})
	stats := b.Stats()
	if stats.Published != 2 || stats.Duplicated != 2 || stats.Handled != 2 {
		t.Fatalf("Stats = %+v, 期望 Published=2 Duplicated=2 Handled=2", stats)
	}
}

// TestEmptyIDNotDeduped 验证空 ID 每次都视为首次出现。
func TestEmptyIDNotDeduped(t *testing.T) {
	b := New(Options{Workers: 2, QueueSize: 64, Logger: testLogger()})
	defer closeBus(t, b)

	rec := &recorder{}
	mustSubscribe(t, b, rec.handler)

	for range 5 {
		mustPublish(t, b, newEvent("s", ""))
	}
	closeBus(t, b)

	if got := len(rec.snapshot()); got != 5 {
		t.Fatalf("空 ID 事件被处理 %d 次, 期望 5 次", got)
	}
	if stats := b.Stats(); stats.Duplicated != 0 || stats.Handled != 5 {
		t.Fatalf("Stats = %+v, 期望 Duplicated=0 Handled=5", stats)
	}
}

// TestDedupTTLExpiry 验证 TTL 过后同一 ID 可以再次被处理。
func TestDedupTTLExpiry(t *testing.T) {
	b := New(Options{Workers: 1, QueueSize: 16, DedupTTL: 50 * time.Millisecond, Logger: testLogger()})
	defer closeBus(t, b)

	rec := &recorder{}
	mustSubscribe(t, b, rec.handler)

	// TTL 内的第二次投递必须被去重，绝不能进入队列。
	mustPublish(t, b, newEvent("s", "expiring"))
	mustPublish(t, b, newEvent("s", "expiring"))

	// 等待远超 TTL，让首次记录过期。
	time.Sleep(300 * time.Millisecond)
	mustPublish(t, b, newEvent("s", "expiring"))

	closeBus(t, b)

	assertIDs(t, rec.snapshot(), []string{"expiring", "expiring"})
	stats := b.Stats()
	if stats.Published != 2 || stats.Duplicated != 1 || stats.Handled != 2 {
		t.Fatalf("Stats = %+v, 期望 Published=2 Duplicated=1 Handled=2", stats)
	}
}

// ---------------------------------------------------------------------------
// 队列满
// ---------------------------------------------------------------------------

// TestQueueFullDropsWithoutBlocking 验证队列满时丢弃事件、返回 ErrQueueFull 且不阻塞。
func TestQueueFullDropsWithoutBlocking(t *testing.T) {
	b := New(Options{Workers: 1, QueueSize: 1, Logger: testLogger()})
	defer closeBus(t, b)

	entered := make(chan struct{}, 1)
	release := newGate()
	defer release.release()
	mustSubscribe(t, b, func(_ context.Context, ev *bot.Event) {
		select {
		case entered <- struct{}{}:
		default:
		}
		release.wait()
	})

	// 让唯一的 worker 忙起来，此时队列为空。
	mustPublish(t, b, newEvent("s", "q1"))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker 未开始处理首个事件")
	}

	// 队列容量为 1，这一条占满队列。
	mustPublish(t, b, newEvent("s", "q2"))

	// 队列已满：必须立刻返回 ErrQueueFull，绝不阻塞调用方。
	done := make(chan error, 1)
	go func() { done <- b.Publish(context.Background(), newEvent("s", "q3")) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrQueueFull) {
			t.Fatalf("队列满时 Publish 返回 %v, 期望 ErrQueueFull", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("队列满时 Publish 阻塞了")
	}

	release.release()
	closeBus(t, b)

	stats := b.Stats()
	if stats.Published != 2 || stats.Dropped != 1 || stats.Handled != 2 {
		t.Fatalf("Stats = %+v, 期望 Published=2 Dropped=1 Handled=2", stats)
	}
}

// ---------------------------------------------------------------------------
// 关闭语义
// ---------------------------------------------------------------------------

// TestCloseDrainsQueuedEvents 验证 Close 会等待已入队事件全部处理完毕。
func TestCloseDrainsQueuedEvents(t *testing.T) {
	b := New(Options{Workers: 1, QueueSize: 64, Logger: testLogger()})
	defer closeBus(t, b)

	entered := make(chan struct{}, 1)
	release := newGate()
	defer release.release()
	rec := &recorder{}
	mustSubscribe(t, b, func(ctx context.Context, ev *bot.Event) {
		select {
		case entered <- struct{}{}:
		default:
		}
		release.wait()
		rec.handler(ctx, ev)
	})

	mustPublish(t, b, newEvent("s", "e0"))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker 未开始处理首个事件")
	}

	const queued = 5
	for i := 1; i <= queued; i++ {
		mustPublish(t, b, newEvent("s", fmt.Sprintf("e%d", i)))
	}

	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closed <- b.Close(ctx)
	}()

	// 处理器被阻塞，Close 必须仍在 drain 中而非提前返回。
	select {
	case err := <-closed:
		t.Fatalf("Close 在队列未处理完时返回: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release.release()
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := make([]string, 0, queued+1)
	for i := range queued + 1 {
		want = append(want, fmt.Sprintf("e%d", i))
	}
	assertIDs(t, rec.snapshot(), want)
	if got := b.Stats().Handled; got != queued+1 {
		t.Fatalf("Handled = %d, 期望 %d", got, queued+1)
	}
}

// TestCloseIsIdempotentAndRejectsPublish 验证 Close 幂等且关闭后拒绝新事件。
func TestCloseIsIdempotentAndRejectsPublish(t *testing.T) {
	b := New(Options{Workers: 2, QueueSize: 16, Logger: testLogger()})

	rec := &recorder{}
	mustSubscribe(t, b, rec.handler)
	mustPublish(t, b, newEvent("s", "before"))

	closeBus(t, b)
	closeBus(t, b)

	err := b.Publish(context.Background(), newEvent("s", "after"))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后 Publish 返回 %v, 期望 ErrClosed", err)
	}
	if err := b.Emit(context.Background(), newEvent("s", "after")); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后 Emit 返回 %v, 期望 ErrClosed", err)
	}
	if err := b.Subscribe(rec.handler); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后 Subscribe 返回 %v, 期望 ErrClosed", err)
	}
	assertIDs(t, rec.snapshot(), []string{"before"})
	if stats := b.Stats(); stats.Published != 1 || stats.Handled != 1 {
		t.Fatalf("Stats = %+v, 期望 Published=1 Handled=1", stats)
	}

	// 已完成 drain 的总线即使传入已过期的 ctx 也应返回 nil，体现幂等性。
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Close(expired); err != nil {
		t.Fatalf("已关闭总线上 Close(过期 ctx) 返回 %v, 期望 nil", err)
	}
}

// TestCloseRespectsContext 验证 Close 遵守 ctx 超时且后台仍会处理完已入队事件。
func TestCloseRespectsContext(t *testing.T) {
	b := New(Options{Workers: 1, QueueSize: 16, Logger: testLogger()})
	defer closeBus(t, b)

	entered := make(chan struct{}, 1)
	release := newGate()
	defer release.release()
	rec := &recorder{}
	mustSubscribe(t, b, func(ctx context.Context, ev *bot.Event) {
		select {
		case entered <- struct{}{}:
		default:
		}
		release.wait()
		rec.handler(ctx, ev)
	})

	mustPublish(t, b, newEvent("s", "slow"))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker 未开始处理事件")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close 返回 %v, 期望 context.DeadlineExceeded", err)
	}

	// 释放处理器后，后续 Close 应等待其完成。
	release.release()
	closeBus(t, b)

	assertIDs(t, rec.snapshot(), []string{"slow"})
}

// ---------------------------------------------------------------------------
// 处理器语义
// ---------------------------------------------------------------------------

// TestSubscribeCallsHandlersInOrder 验证多个处理器按注册顺序串行调用。
func TestSubscribeCallsHandlersInOrder(t *testing.T) {
	b := New(Options{Workers: 2, QueueSize: 16, Logger: testLogger()})
	defer closeBus(t, b)

	var mu sync.Mutex
	var calls []string
	add := func(name string) func(context.Context, *bot.Event) {
		return func(context.Context, *bot.Event) {
			mu.Lock()
			calls = append(calls, name)
			mu.Unlock()
		}
	}
	mustSubscribe(t, b, add("h1"))
	mustSubscribe(t, b, add("h2"))
	mustSubscribe(t, b, add("h3"))

	mustPublish(t, b, newEvent("s", "one"))
	mustPublish(t, b, newEvent("s", "two"))
	closeBus(t, b)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"h1", "h2", "h3", "h1", "h2", "h3"}
	if len(calls) != len(want) {
		t.Fatalf("调用次数 = %d, 期望 %d；实际 %v", len(calls), len(want), calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("第 %d 次调用 = %q, 期望 %q；实际 %v", i, calls[i], want[i], calls)
		}
	}
}

// TestNilHandlerRejected 验证空处理器被拒绝。
func TestNilHandlerRejected(t *testing.T) {
	b := New(Options{Workers: 1, QueueSize: 4, Logger: testLogger()})
	defer closeBus(t, b)
	if err := b.Subscribe(nil); err == nil {
		t.Fatal("Subscribe(nil) 未返回错误")
	}
}

// TestNilEventRejected 验证空事件被拒绝且不影响后续投递。
func TestNilEventRejected(t *testing.T) {
	b := New(Options{Workers: 1, QueueSize: 4, Logger: testLogger()})
	defer closeBus(t, b)

	rec := &recorder{}
	mustSubscribe(t, b, rec.handler)

	if err := b.Publish(context.Background(), nil); err == nil {
		t.Fatal("Publish(nil) 未返回错误")
	}
	mustPublish(t, b, newEvent("s", "ok"))
	closeBus(t, b)
	assertIDs(t, rec.snapshot(), []string{"ok"})
}

// TestHandlerPanicDoesNotKillWorker 验证处理器 panic 被恢复且后续事件仍被处理。
func TestHandlerPanicDoesNotKillWorker(t *testing.T) {
	b := New(Options{Workers: 2, QueueSize: 32, Logger: testLogger()})
	defer closeBus(t, b)

	rec := &recorder{}
	mustSubscribe(t, b, func(ctx context.Context, ev *bot.Event) {
		if ev.ID == "boom" {
			panic("boom")
		}
		rec.handler(ctx, ev)
	})

	mustPublish(t, b, newEvent("s", "before"))
	mustPublish(t, b, newEvent("s", "boom"))
	mustPublish(t, b, newEvent("s", "after-1"))
	mustPublish(t, b, newEvent("s", "after-2"))
	closeBus(t, b)

	assertIDs(t, rec.snapshot(), []string{"before", "after-1", "after-2"})
	stats := b.Stats()
	if stats.Handled != 3 || stats.Failed != 1 || stats.Published != 4 {
		t.Fatalf("Stats = %+v, 期望 Published=4 Handled=3 Failed=1", stats)
	}
}

// TestPanicInOneHandlerDoesNotBlockOthers 验证单个处理器 panic 不影响其它处理器。
func TestPanicInOneHandlerDoesNotBlockOthers(t *testing.T) {
	b := New(Options{Workers: 1, QueueSize: 8, Logger: testLogger()})
	defer closeBus(t, b)

	rec := &recorder{}
	mustSubscribe(t, b, func(context.Context, *bot.Event) { panic("first handler") })
	mustSubscribe(t, b, rec.handler)

	mustPublish(t, b, newEvent("s", "e1"))
	mustPublish(t, b, newEvent("s", "e2"))
	closeBus(t, b)

	assertIDs(t, rec.snapshot(), []string{"e1", "e2"})
	if stats := b.Stats(); stats.Failed != 2 || stats.Handled != 0 {
		t.Fatalf("Stats = %+v, 期望 Failed=2 Handled=0", stats)
	}
}

// TestEmitBehavesLikePublish 验证 Emit 与 Publish 等价。
func TestEmitBehavesLikePublish(t *testing.T) {
	b := New(Options{Workers: 2, QueueSize: 16, Logger: testLogger()})
	defer closeBus(t, b)

	var sink bot.EventSink = b
	rec := &recorder{}
	mustSubscribe(t, b, rec.handler)

	if err := sink.Emit(context.Background(), newEvent("s", "emitted")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	closeBus(t, b)

	assertIDs(t, rec.snapshot(), []string{"emitted"})
	if stats := b.Stats(); stats.Published != 1 || stats.Handled != 1 {
		t.Fatalf("Stats = %+v, 期望 Published=1 Handled=1", stats)
	}
}

// TestStatsCounts 验证计数器各项在混合负载下精确计数。
func TestStatsCounts(t *testing.T) {
	b := New(Options{Workers: 2, QueueSize: 64, Logger: testLogger()})
	defer closeBus(t, b)

	rec := &recorder{}
	mustSubscribe(t, b, func(ctx context.Context, ev *bot.Event) {
		if ev.ID == "boom" {
			panic("boom")
		}
		rec.handler(ctx, ev)
	})

	for i := range 5 {
		mustPublish(t, b, newEvent("s", fmt.Sprintf("unique-%d", i)))
	}
	for range 2 {
		mustPublish(t, b, newEvent("s", "unique-0")) // 重复
	}
	for range 2 {
		mustPublish(t, b, newEvent("s", "")) // 空 ID 不去重
	}
	mustPublish(t, b, newEvent("s", "boom"))

	closeBus(t, b)

	want := []string{"unique-0", "unique-1", "unique-2", "unique-3", "unique-4", "", "", "boom"}
	got := rec.snapshot()
	if len(got) != len(want)-1 {
		t.Fatalf("处理器记录 %d 条, 期望 %d 条: %v", len(got), len(want)-1, got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条 = %q, 期望 %q; 实际 %v", i, got[i], want[i], got)
		}
	}

	stats := b.Stats()
	if stats.Published != 8 || stats.Duplicated != 2 || stats.Dropped != 0 || stats.Handled != 7 || stats.Failed != 1 {
		t.Fatalf("Stats = %+v, 期望 Published=8 Duplicated=2 Dropped=0 Handled=7 Failed=1", stats)
	}
}

// TestStatsIsSnapshot 验证 Stats 返回独立快照，不随总线状态变化。
func TestStatsIsSnapshot(t *testing.T) {
	b := New(Options{Workers: 1, QueueSize: 16, Logger: testLogger()})
	defer closeBus(t, b)

	mustSubscribe(t, b, func(context.Context, *bot.Event) {})
	mustPublish(t, b, newEvent("s", "e1"))

	snap := b.Stats()
	mustPublish(t, b, newEvent("s", "e2"))
	closeBus(t, b)

	if snap.Published != 1 {
		t.Fatalf("快照 Published = %d, 期望 1", snap.Published)
	}
	if live := b.Stats(); live.Published != 2 || live.Handled != 2 {
		t.Fatalf("实时 Stats = %+v, 期望 Published=2 Handled=2", live)
	}
}

// TestConcurrentPublishSameIDHandledOnce 验证并发投递同一 ID 时只被处理一次。
func TestConcurrentPublishSameIDHandledOnce(t *testing.T) {
	b := New(Options{Workers: 4, QueueSize: 128, Logger: testLogger()})
	defer closeBus(t, b)

	rec := &recorder{}
	mustSubscribe(t, b, rec.handler)

	const goroutines = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := b.Publish(context.Background(), newEvent("race", "same-id")); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Publish: %v", err)
	}
	closeBus(t, b)

	assertIDs(t, rec.snapshot(), []string{"same-id"})
	stats := b.Stats()
	if stats.Handled != 1 || stats.Published != 1 || stats.Duplicated != goroutines-1 {
		t.Fatalf("Stats = %+v, 期望 Published=1 Duplicated=%d Handled=1", stats, goroutines-1)
	}
}

// TestCloseWaitsForWorkerExit 验证 Close 返回时所有 worker goroutine 均已退出。
//
// done 通道由 wg.Wait() 关闭，因此闭合即证明每个 worker 的 run 循环都已返回。
func TestCloseWaitsForWorkerExit(t *testing.T) {
	b := New(Options{Workers: 4, QueueSize: 16, Logger: testLogger()})
	mustSubscribe(t, b, func(context.Context, *bot.Event) {})
	for i := range 20 {
		mustPublish(t, b, newEvent(fmt.Sprintf("s%d", i), fmt.Sprintf("e%d", i)))
	}
	closeBus(t, b)

	select {
	case <-b.done:
	default:
		t.Fatal("Close 返回后 done 未关闭，worker goroutine 仍在运行")
	}

	// worker 退出后计数器不应再变化。
	before := b.Stats()
	time.Sleep(20 * time.Millisecond)
	if after := b.Stats(); after != before {
		t.Fatalf("worker 退出后 Stats 仍在变化: %+v → %+v", before, after)
	}
	if before.Handled != 20 {
		t.Fatalf("Handled = %d, 期望 20", before.Handled)
	}
}

// TestShardRoutingIsDeterministicAndInRange 验证会话键稳定映射到 [0,workers)。
func TestShardRoutingIsDeterministicAndInRange(t *testing.T) {
	const workers = 8
	for i := range 200 {
		key := sessionKey(fmt.Sprintf("sess-%d", i))
		idx := shardIndex(key, workers)
		if idx < 0 || idx >= workers {
			t.Fatalf("shardIndex(%q) = %d, 越界 [0,%d)", key, idx, workers)
		}
		if again := shardIndex(key, workers); again != idx {
			t.Fatalf("shardIndex(%q) 不稳定: %d → %d", key, idx, again)
		}
	}

	// 同一会话键必然落同一分片；不同 worker 数下映射可以不同，但必须合法。
	keys := []string{sessionKey("a"), sessionKey("b"), sessionKey("c")}
	for _, n := range []int{1, 2, 3, 4, 8} {
		for _, k := range keys {
			idx := shardIndex(k, n)
			if idx < 0 || idx >= n {
				t.Fatalf("shardIndex(%q, %d) = %d, 越界", k, n, idx)
			}
		}
	}
}

// TestPublishDoesNotMutateEvent 验证 Publish 不写调用方的事件对象。
func TestPublishDoesNotMutateEvent(t *testing.T) {
	b := New(Options{Workers: 2, QueueSize: 16, Logger: testLogger()})
	defer closeBus(t, b)

	var mu sync.Mutex
	var seen *bot.Event
	mustSubscribe(t, b, func(_ context.Context, ev *bot.Event) {
		mu.Lock()
		seen = ev
		mu.Unlock()
	})

	ev := newEvent("s", "immutable")
	before := *ev
	beforeMsg := *ev.Message
	beforeSender := *ev.Sender
	beforeChannel := *ev.Channel

	mustPublish(t, b, ev)

	// publish 返回后事件的所有字段都必须保持原样，调用方才可以安全复用。
	if !reflect.DeepEqual(*ev, before) {
		t.Fatalf("Publish 修改了事件: %+v → %+v", before, *ev)
	}
	if !reflect.DeepEqual(*ev.Message, beforeMsg) ||
		!reflect.DeepEqual(*ev.Sender, beforeSender) ||
		!reflect.DeepEqual(*ev.Channel, beforeChannel) {
		t.Fatal("Publish 修改了事件的子结构")
	}

	closeBus(t, b)

	mu.Lock()
	defer mu.Unlock()
	if seen == nil {
		t.Fatal("处理器未收到事件")
	}
	if !reflect.DeepEqual(*seen, before) {
		t.Fatalf("处理器收到的事件 = %+v, 期望 %+v", *seen, before)
	}
}

// TestDefaultsApplied 验证零值 Options 使用默认配置仍可正常工作。
func TestDefaultsApplied(t *testing.T) {
	b := New(Options{})
	defer closeBus(t, b)

	if len(b.shards) != defaultWorkers {
		t.Fatalf("worker 分片数 = %d, 期望 %d", len(b.shards), defaultWorkers)
	}
	if got := cap(b.shards[0].q); got != defaultQueueSize {
		t.Fatalf("队列容量 = %d, 期望 %d", got, defaultQueueSize)
	}

	rec := &recorder{}
	mustSubscribe(t, b, rec.handler)
	mustPublish(t, b, newEvent("s", "default"))
	closeBus(t, b)
	assertIDs(t, rec.snapshot(), []string{"default"})
}
