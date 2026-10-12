package dedup

import (
	"sync"
	"testing"
	"time"
)

func TestAddDetectsDuplicate(t *testing.T) {
	s := New(16, time.Minute)

	if !s.Add("e1") {
		t.Fatal("首次 Add 应返回 true")
	}
	if s.Add("e1") {
		t.Fatal("重复 Add 应返回 false")
	}
	if !s.Seen("e1") {
		t.Fatal("Seen 应为 true")
	}
	if s.Seen("e2") {
		t.Fatal("未出现过的 ID Seen 应为 false")
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

func TestAddEmptyIDIsNeverDuplicate(t *testing.T) {
	s := New(16, time.Minute)
	// 两次调用都必须返回 true（空 ID 无法去重）。
	for range 2 {
		if !s.Add("") {
			t.Fatal("空 ID 无法去重，应始终返回 true")
		}
	}
	if s.Len() != 0 {
		t.Fatalf("空 ID 不应写入集合, Len = %d", s.Len())
	}
}

func TestExpiryAllowsReuse(t *testing.T) {
	s := New(16, 20*time.Millisecond)
	if !s.Add("e1") {
		t.Fatal("首次 Add 应为 true")
	}
	time.Sleep(35 * time.Millisecond)
	if s.Seen("e1") {
		t.Fatal("过期后 Seen 应为 false")
	}
	if !s.Add("e1") {
		t.Fatal("过期后 Add 应重新返回 true")
	}
}

func TestGCRemovesExpired(t *testing.T) {
	s := New(16, 15*time.Millisecond)
	s.Add("e1")
	s.Add("e2")
	time.Sleep(30 * time.Millisecond)

	if got := s.GC(); got != 2 {
		t.Fatalf("GC 删除数 = %d, want 2", got)
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("GC 后 Len = %d, want 0", got)
	}

	permanent := New(16, 0)
	permanent.Add("keep")
	if got := permanent.GC(); got != 0 {
		t.Fatalf("无 TTL 时 GC 不应删除, got %d", got)
	}
}

func TestCapacityEvictsOldest(t *testing.T) {
	s := New(2, time.Hour)
	s.Add("a")
	s.Add("b")
	s.Add("c")

	if got := s.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	if s.Seen("a") {
		t.Fatal("最旧的条目应被淘汰")
	}
	if !s.Seen("c") {
		t.Fatal("最新条目应保留")
	}
	s.Add("d")
	if s.Seen("b") {
		t.Fatal("超出容量后应继续淘汰最旧条目")
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
}

func TestNilSetIsSafe(t *testing.T) {
	var s *Set
	if !s.Add("x") {
		t.Fatal("nil Set 的 Add 应返回 true（无法去重）")
	}
	if s.Seen("x") {
		t.Fatal("nil Set 的 Seen 应为 false")
	}
	if s.Len() != 0 || s.GC() != 0 {
		t.Fatal("nil Set 的 Len/GC 应为 0")
	}
}

func TestConcurrentAddIsRaceFree(t *testing.T) {
	s := New(1024, time.Minute)
	const workers = 16

	var (
		mu       sync.Mutex
		firstAdd int
	)
	done := make(chan struct{})
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			if s.Add("shared") {
				mu.Lock()
				firstAdd++
				mu.Unlock()
			}
		}()
	}
	for range workers {
		<-done
	}
	if firstAdd != 1 {
		t.Fatalf("并发 Add 同一 ID 应恰好有一次返回 true, got %d", firstAdd)
	}
}

// orderLen 在锁内读取内部插入顺序切片长度（仅本包测试使用）。
func (s *Set) orderLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.order)
}

// TestStaleOrderSlotDoesNotEvictReAddedID 复现「过期后重加的 ID 被自己的陈旧槽位
// 提前淘汰」：X 首次插入后过期，A 在其间插入（更旧），随后 X 被重加；容量淘汰
// 必须删掉最旧的 A，而不是只剩陈旧槽位的 X。
func TestStaleOrderSlotDoesNotEvictReAddedID(t *testing.T) {
	s := New(2, 100*time.Millisecond)
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }

	if !s.Add("X") {
		t.Fatal("首次 Add(X) 应为 true")
	}
	// 显式 GC 刷新基准时刻：此后重加 X 不会触发 GC，X 的陈旧槽位会留在 order 中。
	now = now.Add(50 * time.Millisecond)
	if n := s.GC(); n != 0 {
		t.Fatalf("GC 不应删除未过期条目, got %d", n)
	}
	if !s.Add("A") {
		t.Fatal("Add(A) 应为 true")
	}

	now = now.Add(70 * time.Millisecond) // X 已过期（100ms），A 仍未过期
	if !s.Add("X") {
		t.Fatal("过期后重加 X 应返回 true")
	}
	if !s.Add("B") { // 容量 2，len(items)==3 触发淘汰
		t.Fatal("Add(B) 应为 true")
	}

	if !s.Seen("X") {
		t.Fatal("重加的 X 在 TTL 内应仍被视为已出现")
	}
	if !s.Seen("B") {
		t.Fatal("最新写入的 B 应保留")
	}
	if s.Seen("A") {
		t.Fatal("淘汰的应是最旧的活条目 A")
	}
	if got := s.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

// TestOrderStaysBoundedUnderReAdd 断言跨 TTL 反复重加同一批 ID 时插入顺序切片有界。
func TestOrderStaysBoundedUnderReAdd(t *testing.T) {
	s := New(4, time.Second)
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }
	s.GC() // 把 lastGC 同步到假时钟基准

	for range 20000 {
		now = now.Add(2 * time.Second)
		s.Add("same")
	}
	if got := s.orderLen(); got > 4 {
		t.Fatalf("重置后 order 长度 = %d, want <= 4", got)
	}
	if !s.Seen("same") {
		t.Fatal("最后一次重加应仍在有效期内")
	}
}
