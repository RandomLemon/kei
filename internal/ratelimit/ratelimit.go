// Package ratelimit 提供按 key 的令牌桶限流器。
//
// 中间件用它按用户/群/插件限流，发送链路用它限制对平台的调用速率。
// 所有 key 共享同一组参数，空闲桶会被回收。
package ratelimit

import (
	"sync"
	"time"
)

// Limiter 是并发安全的令牌桶集合。
type Limiter struct {
	mu      sync.Mutex
	rate    float64       // 每秒补充的令牌数
	burst   float64       // 桶容量
	idle    time.Duration // 空闲多久后回收桶
	buckets map[string]*bucket
	lastGC  time.Time
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New 构造限流器。rate 为每秒允许的请求数，burst 为突发容量。
//
// rate <= 0 时表示不限流，Allow 恒为 true。burst <= 0 时取 max(1, rate)。
// idle <= 0 时使用 10 分钟作为空闲回收阈值。
func New(rate float64, burst int, idle time.Duration) *Limiter {
	if idle <= 0 {
		idle = 10 * time.Minute
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
		buckets: make(map[string]*bucket),
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
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
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
		}
	}
}
