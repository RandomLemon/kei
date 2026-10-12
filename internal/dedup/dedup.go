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

	seq    uint64           // 单调递增的槽位序号
	items  map[string]entry // id -> 当前活条目
	order  []slot           // 插入顺序槽位，用于容量淘汰
	lastGC time.Time
}

// entry 是集合中的一个活条目。exp 为零值表示永不过期；seq 是写入该条目时
// 分配的槽位序号，用于在淘汰时区分同一 ID 的旧槽位。
type entry struct {
	exp time.Time
	seq uint64
}

// slot 是 order 中的一个插入顺序槽位。同一 ID 在过期后被重加会产生新的
// 槽位，旧槽位随之失效（其 seq 不再等于 items[id].seq）。
type slot struct {
	id  string
	seq uint64
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
		items:    make(map[string]entry, min(capacity, 1024)),
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

	if e, ok := s.items[id]; ok && (e.exp.IsZero() || e.exp.After(now)) {
		return false
	}
	s.seq++
	s.items[id] = entry{exp: s.expiry(now), seq: s.seq}
	s.order = append(s.order, slot{id: id, seq: s.seq})

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

	e, ok := s.items[id]
	return ok && (e.exp.IsZero() || e.exp.After(now))
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
	for id, e := range s.items {
		if !e.exp.IsZero() && !e.exp.After(now) {
			delete(s.items, id)
		}
	}
	s.compactOrderLocked()
}

// evictLocked 按插入顺序淘汰最旧的条目。
//
// 同一 ID 在过期后被重加时会留下旧槽位，只删除与当前活条目序号一致的槽位；
// 陈旧槽位直接跳过，不得连带删除重加后的新条目。
func (s *Set) evictLocked() {
	for len(s.order) > 0 {
		sl := s.order[0]
		s.order = s.order[1:]
		if e, ok := s.items[sl.id]; ok && e.seq == sl.seq {
			delete(s.items, sl.id)
			return
		}
	}
	// 不变量：每个活条目在 order 中都有当前槽位，因此 items 非空时不会走到这里。
	// 兜底删除任一残留条目，保证淘汰循环终止。
	for id := range s.items {
		delete(s.items, id)
		return
	}
}

// compactOrderLocked 丢弃 order 中已失效的槽位（条目已被删除，或该 ID 已被
// 重加而槽位序号不再是当前序号），防止切片无限增长。
func (s *Set) compactOrderLocked() {
	kept := s.order[:0]
	for _, sl := range s.order {
		if e, ok := s.items[sl.id]; ok && e.seq == sl.seq {
			kept = append(kept, sl)
		}
	}
	s.order = kept
}
