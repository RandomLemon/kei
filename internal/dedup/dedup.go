// Package dedup 提供带 TTL 与容量上限的事件去重集合。
//
// 事件总线与 Dedup 中间件共用该实现：同一个 ID 在 TTL 内第二次出现即被视为
// 重复。集合同时受容量约束，避免恶意 ID 造成内存膨胀。
package dedup

import (
	"sync"
	"time"
)

// Set 是并发安全的去重集合。零值不可用，必须通过 New 构造。
type Set struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	now      func() time.Time

	items  map[string]time.Time // id -> 过期时间，零值表示永不过期
	order  []string             // 插入顺序，用于容量淘汰
	lastGC time.Time
}

// New 构造去重集合。capacity <= 0 时使用默认容量 4096；
// ttl <= 0 时条目永不过期（此时容量淘汰是唯一的内存约束）。
func New(capacity int, ttl time.Duration) *Set {
	if capacity <= 0 {
		capacity = 4096
	}
	return &Set{
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
		items:    make(map[string]time.Time, min(capacity, 1024)),
		lastGC:   time.Now(),
	}
}

// Add 标记 ID 已出现，返回 true 表示这是首次出现，false 表示重复。
//
// 空 ID 无法去重，一律视为首次出现。
func (s *Set) Add(id string) bool {
	if s == nil || id == "" {
		return true
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if exp, ok := s.items[id]; ok && (exp.IsZero() || exp.After(now)) {
		return false
	}
	s.items[id] = s.expiry(now)
	s.order = append(s.order, id)

	if s.ttl > 0 && now.Sub(s.lastGC) >= s.ttl {
		s.lastGC = now
		s.gcLocked(now)
	}
	for len(s.items) > s.capacity {
		s.evictLocked()
	}
	return true
}

// Seen 判断 ID 是否已在有效期内出现过，不改变集合状态。
func (s *Set) Seen(id string) bool {
	if s == nil || id == "" {
		return false
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	exp, ok := s.items[id]
	return ok && (exp.IsZero() || exp.After(now))
}

// Len 返回当前有效条目数。
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// GC 清理过期条目，返回删除数量。
func (s *Set) GC() int {
	if s == nil || s.ttl <= 0 {
		return 0
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	before := len(s.items)
	s.lastGC = now
	s.gcLocked(now)
	return before - len(s.items)
}

func (s *Set) expiry(now time.Time) time.Time {
	if s.ttl <= 0 {
		return time.Time{}
	}
	return now.Add(s.ttl)
}

func (s *Set) gcLocked(now time.Time) {
	if s.ttl <= 0 {
		return
	}
	for id, exp := range s.items {
		if !exp.IsZero() && !exp.After(now) {
			delete(s.items, id)
		}
	}
	s.compactOrderLocked()
}

// evictLocked 按插入顺序淘汰最旧的条目。
func (s *Set) evictLocked() {
	for len(s.order) > 0 {
		id := s.order[0]
		s.order = s.order[1:]
		if _, ok := s.items[id]; ok {
			delete(s.items, id)
			return
		}
	}
	// order 已空但 items 仍有残留（理论上不会发生），退化为清空最旧键。
	for id := range s.items {
		delete(s.items, id)
		return
	}
}

// compactOrderLocked 丢弃 order 中已被删除的 ID，防止切片无限增长。
func (s *Set) compactOrderLocked() {
	kept := s.order[:0]
	for _, id := range s.order {
		if _, ok := s.items[id]; ok {
			kept = append(kept, id)
		}
	}
	s.order = kept
}
