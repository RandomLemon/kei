// Package eventbus 提供分片保序、去重且不阻塞投递方的事件总线。
//
// 总线按会话键（bot.Event.SessionKey）的哈希把事件路由到固定数量的 worker
// 分片：同一会话的事件落在同一分片、由同一个 goroutine 串行处理，因此严格
// 保序；不同会话可以并行处理。总线实现了 bot.EventSink，适配器的回调线程
// 永远不会被业务处理阻塞：队列已满时事件被丢弃并返回错误。
package eventbus

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomLemon/kei/internal/dedup"
	"github.com/RandomLemon/kei/pkg/bot"
)

// 默认配置，用于 Options 中未指定或非法的字段。
const (
	defaultWorkers       = 4
	defaultQueueSize     = 256
	defaultDedupTTL      = 5 * time.Minute
	defaultDedupCapacity = 4096
)

// ErrClosed 表示总线已关闭，不再接收新事件。
var ErrClosed = errors.New("eventbus: closed")

// ErrQueueFull 表示目标分片队列已满，事件被丢弃。
var ErrQueueFull = errors.New("eventbus: queue full")

// errNilEvent 表示调用方传入了空事件。
var errNilEvent = errors.New("eventbus: nil event")

// errNilHandler 表示调用方传入了空处理器。
var errNilHandler = errors.New("eventbus: nil handler")

// handlerFunc 是订阅的事件处理器签名。
type handlerFunc func(context.Context, *bot.Event)

// Options 是总线配置。零值合法，所有字段都会回落到默认值。
type Options struct {
	Workers       int           // 分片 worker 数；<=0 → 4
	QueueSize     int           // 每分片队列容量；<=0 → 256
	DedupTTL      time.Duration // 去重 TTL；<=0 → 5m
	DedupCapacity int           // 去重集合容量；<=0 → 4096
	Logger        *slog.Logger  // nil → slog.Default()
}

// Stats 是总线计数器快照，各项均为自总线创建以来的累计值。
type Stats struct {
	// Published 是成功入队的事件数。
	Published uint64
	// Duplicated 是因重复 ID 被忽略的事件数。
	Duplicated uint64
	// Dropped 是因队列已满被丢弃的事件数。
	Dropped uint64
	// Handled 是被处理器正常处理完毕的事件数。
	Handled uint64
	// Failed 是处理过程中至少有一次处理器 panic 的事件数。
	Failed uint64
}

// counters 是 Stats 的原子实现，避免读快照时加锁。
type counters struct {
	published  atomic.Uint64
	duplicated atomic.Uint64
	dropped    atomic.Uint64
	handled    atomic.Uint64
	failed     atomic.Uint64
}

// shard 是一个分片：一个带缓冲队列与处理它的 worker goroutine。
//
// mu 同时保护 closed 标志与队列写入：Publish 在锁内检查 closed 并入队，
// Close 在锁内置位 closed 并关闭队列，因此不存在向已关闭 channel 发送的窗口。
type shard struct {
	mu     sync.Mutex
	q      chan *bot.Event
	closed bool
}

// Bus 是分片保序的事件总线，实现 bot.EventSink。
//
// 零值不可用，必须通过 New 构造。
type Bus struct {
	shards []*shard
	dedup  *dedup.Set
	logger *slog.Logger
	// baseCtx 是传递给处理器的上下文：它不随 Close 取消，保证 Close 时
	// 已入队事件仍能被处理完。
	baseCtx context.Context

	// handlers 以写时复制方式更新，读取路径无锁、无分配。
	handlers atomic.Pointer[[]handlerFunc]
	subMu    sync.Mutex

	closed    atomic.Bool
	closeOnce sync.Once
	wg        sync.WaitGroup
	done      chan struct{}

	stats counters
}

// 编译期断言：Bus 实现 bot.EventSink。
var _ bot.EventSink = (*Bus)(nil)

// New 构造并启动事件总线，按 Options.Workers 启动对应数量的处理 goroutine。
//
// 返回的总线必须调用 Close 释放 worker goroutine。
func New(opts Options) *Bus {
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if opts.DedupTTL <= 0 {
		opts.DedupTTL = defaultDedupTTL
	}
	if opts.DedupCapacity <= 0 {
		opts.DedupCapacity = defaultDedupCapacity
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	b := &Bus{
		shards:  make([]*shard, opts.Workers),
		dedup:   dedup.New(opts.DedupCapacity, opts.DedupTTL),
		logger:  logger,
		baseCtx: context.Background(),
		done:    make(chan struct{}),
	}
	empty := make([]handlerFunc, 0, 2)
	b.handlers.Store(&empty)

	for i := range opts.Workers {
		sh := &shard{q: make(chan *bot.Event, opts.QueueSize)}
		b.shards[i] = sh
		b.wg.Add(1)
		go b.run(sh)
	}
	return b
}

// Publish 把事件投递到会话所属分片。
//
// 返回 nil 表示事件已入队，或事件因 ID 重复被忽略；它不表示事件已被处理。
// Publish 永不阻塞：队列已满时丢弃事件并返回包装了 ErrQueueFull 的错误。
// ctx 仅为满足 bot.EventSink 契约，投递本身是非阻塞的，不响应取消。
// Publish 不会修改 ev，调用方可以在投递后复用事件对象。
func (b *Bus) Publish(_ context.Context, ev *bot.Event) error {
	if ev == nil {
		return errNilEvent
	}
	key := ev.SessionKey()
	sh := b.shards[shardIndex(key, len(b.shards))]

	// 所有入队操作都在 sh.mu 内完成，因此下面的容量检查与发送之间不存在
	// 并发写入，容量足够时发送一定不会阻塞。
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if sh.closed {
		return ErrClosed
	}
	if b.dedup.Seen(ev.ID) {
		b.stats.duplicated.Add(1)
		return nil
	}
	if len(sh.q) >= cap(sh.q) {
		// 丢弃的事件不写入去重集合，重试仍可成功投递。
		b.stats.dropped.Add(1)
		b.logger.Warn("eventbus: 分片队列已满，事件被丢弃",
			"event_id", ev.ID, "session_key", key, "queue_size", cap(sh.q))
		return fmt.Errorf("eventbus: publish %q: %w", ev.ID, ErrQueueFull)
	}
	if !b.dedup.Add(ev.ID) {
		// 并发投递下同一 ID 的兜底去重。
		b.stats.duplicated.Add(1)
		return nil
	}
	sh.q <- ev
	b.stats.published.Add(1)
	return nil
}

// Emit 实现 bot.EventSink，语义与 Publish 完全一致。
func (b *Bus) Emit(ctx context.Context, ev *bot.Event) error {
	return b.Publish(ctx, ev)
}

// Subscribe 注册事件处理器。
//
// 可以多次调用，同一事件会按注册顺序串行调用全部处理器；某个处理器 panic
// 不会影响其它处理器，也不会终止 worker。建议在首次 Publish 之前完成注册，
// 关闭后再注册返回 ErrClosed。
func (b *Bus) Subscribe(handler func(context.Context, *bot.Event)) error {
	if handler == nil {
		return errNilHandler
	}
	if b.closed.Load() {
		return ErrClosed
	}

	b.subMu.Lock()
	defer b.subMu.Unlock()

	old := b.handlers.Load()
	var next []handlerFunc
	if old != nil {
		next = make([]handlerFunc, 0, len(*old)+1)
		next = append(next, *old...)
	} else {
		next = make([]handlerFunc, 0, 1)
	}
	next = append(next, handler)
	b.handlers.Store(&next)
	return nil
}

// Close 停止接收新事件，处理完所有已入队事件，并等待全部 worker 退出。
//
// Close 幂等，可以多次调用。ctx 超时返回 ctx.Err()，此时后台仍会继续处理
// 已入队的事件；后续 Close 可以继续等待它们完成。
func (b *Bus) Close(ctx context.Context) error {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		for _, sh := range b.shards {
			sh.mu.Lock()
			if !sh.closed {
				sh.closed = true
				close(sh.q)
			}
			sh.mu.Unlock()
		}
		go func() {
			b.wg.Wait()
			close(b.done)
		}()
	})

	// 已完成 drain 时优先返回 nil：即使调用方传入的 ctx 已过期，
	// 关闭也已经是既成事实，不应报告失败。
	select {
	case <-b.done:
		return nil
	default:
	}

	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats 返回当前计数器快照。
func (b *Bus) Stats() Stats {
	return Stats{
		Published:  b.stats.published.Load(),
		Duplicated: b.stats.duplicated.Load(),
		Dropped:    b.stats.dropped.Load(),
		Handled:    b.stats.handled.Load(),
		Failed:     b.stats.failed.Load(),
	}
}

// run 是分片的 worker：按 FIFO 消费队列，队列关闭并取空后退出。
func (b *Bus) run(sh *shard) {
	defer b.wg.Done()
	for ev := range sh.q {
		b.dispatch(ev)
	}
}

// dispatch 按注册顺序调用全部处理器，并更新计数器。
//
// 事件只要有一个处理器 panic 就计入 Failed，否则计入 Handled。
func (b *Bus) dispatch(ev *bot.Event) {
	var list []handlerFunc
	if hs := b.handlers.Load(); hs != nil {
		list = *hs
	}

	panicked := false
	for _, h := range list {
		if b.callHandler(h, ev) {
			panicked = true
		}
	}
	if panicked {
		b.stats.failed.Add(1)
		return
	}
	b.stats.handled.Add(1)
}

// callHandler 调用单个处理器并捕获 panic，返回值表示是否发生 panic。
func (b *Bus) callHandler(h handlerFunc, ev *bot.Event) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			b.logger.Error("eventbus: 处理器 panic",
				"event_id", ev.ID, "session_key", ev.SessionKey(), "panic", r)
		}
	}()
	h(b.baseCtx, ev)
	return false
}

// hasher 复用 FNV-1a 哈希器与字符串缓冲区，避免每次 Publish 分配。
type hasher struct {
	h   hash.Hash32
	buf []byte
}

// maxHasherBuf 限制放回池中的缓冲区大小，避免超长会话键导致内存长期滞留。
const maxHasherBuf = 512

var hasherPool = sync.Pool{New: func() any { return &hasher{h: fnv.New32a()} }}

// shardIndex 按 FNV-1a 哈希把 key 映射到 [0, n) 的分片下标。
func shardIndex(key string, n int) int {
	hs := hasherPool.Get().(*hasher)
	hs.h.Reset()
	hs.buf = append(hs.buf[:0], key...)
	_, _ = hs.h.Write(hs.buf)
	idx := int(hs.h.Sum32() % uint32(n))
	if cap(hs.buf) > maxHasherBuf {
		hs.buf = nil
	}
	hasherPool.Put(hs)
	return idx
}
