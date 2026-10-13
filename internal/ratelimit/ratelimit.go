// Package ratelimit 提供按 key 的令牌桶限流器。
//
// 中间件用它按用户/群/插件限流，发送链路用它限制对平台的调用速率。
// 所有 key 共享同一组参数；空闲桶会被回收，桶表本身也有容量上限（超过后按
// 最久未使用淘汰），因此 key 空间不可枚举（例如伪造的用户 ID）时内存仍有界。
package ratelimit

import (
	"container/list"
	"sync"
	"time"
)

// defaultMaxKeys 是桶表的默认容量上限。
const defaultMaxKeys = 10000

// Limiter 是并发安全的令牌桶集合。
type Limiter struct {
	mu      sync.Mutex
	rate    float64       // 每秒补充的令牌数
	burst   float64       // 桶容量
	idle    time.Duration // 空闲多久后回收桶
	maxKeys int           // 桶表上限，达到后淘汰最久未使用的桶
	buckets map[string]*bucket
	order   *list.List // 按最近使用排序，Front 最久未使用
	lastGC  time.Time
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
	elem   *list.Element
}

// New 构造限流器。rate 为每秒允许的请求数，burst 为突发容量。
//
// rate <= 0 时表示不限流，Allow 恒为 true。burst <= 0 时取 max(1, rate)。
// idle <= 0 时使用 10 分钟作为空闲回收阈值。maxKeys <= 0 时使用 defaultMaxKeys；
// 活跃桶数达到上限后，每插入一个新 key 淘汰一个最久未使用的桶（该 key 下次出现
// 时重建为满桶）。
func New(rate float64, burst int, idle time.Duration, maxKeys int) *Limiter {
	if idle <= 0 {
		idle = 10 * time.Minute
	}
	if maxKeys <= 0 {
		maxKeys = defaultMaxKeys
	}
	b := float64(burst)
	if b <= 0 {
		b = rate
	}
	if b < 1 {
		b = 1
	}
	return &Limiter{
		rate:    rate,
		burst:   b,
		idle:    idle,
		maxKeys: maxKeys,
		buckets: make(map[string]*bucket),
		order:   list.New(),
		lastGC:  time.Now(),
		now:     time.Now,
	}
}

// Allow 判断 key 是否可放行一个请求，等价于 AllowN(key, 1)。
func (l *Limiter) Allow(key string) bool {
	return l.AllowN(key, 1)
}

// AllowN 判断 key 是否可放行 n 个请求；放行时才扣除令牌。
func (l *Limiter) AllowN(key string, n int) bool {
	if l == nil || l.rate <= 0 {
		return true
	}
	if n <= 0 {
		n = 1
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.gcLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		l.evictLocked()
		b = &bucket{tokens: l.burst, last: now}
		b.elem = l.order.PushBack(key)
		l.buckets[key] = b
	} else {
		// 任何访问都算使用：被拒绝的调用同样刷新桶的最近使用位置。
		l.order.MoveToBack(b.elem)
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
			b.last = now
		}
	}
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

// Buckets 返回当前活跃桶数量。
func (l *Limiter) Buckets() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// GC 回收空闲桶，返回回收数量。
func (l *Limiter) GC() int {
	if l == nil {
		return 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	before := len(l.buckets)
	l.gcLocked(now)
	return before - len(l.buckets)
}

func (l *Limiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < l.idle {
		return
	}
	l.lastGC = now
	for k, b := range l.buckets {
		if now.Sub(b.last) >= l.idle {
			delete(l.buckets, k)
			l.order.Remove(b.elem)
		}
	}
}

// evictLocked 在桶表达到上限时为即将插入的新 key 腾出一个位置，淘汰最久未使用
// 的桶（order 的 Front）。O(1)，不随桶数增长。
func (l *Limiter) evictLocked() {
	if len(l.buckets) < l.maxKeys {
		return
	}
	front := l.order.Front()
	if front == nil {
		return
	}
	delete(l.buckets, front.Value.(string))
	l.order.Remove(front)
}
