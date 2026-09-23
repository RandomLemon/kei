package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDisabledLimiterAllowsEverything(t *testing.T) {
	l := New(0, 0, time.Minute)
	for range 100 {
		if !l.Allow("k") {
			t.Fatal("rate=0 时应始终放行")
		}
	}
	var nilLimiter *Limiter
	if !nilLimiter.Allow("k") {
		t.Fatal("nil Limiter 应放行")
	}
	if err := nilLimiter.Wait(context.Background(), "k"); err != nil {
		t.Fatalf("nil Limiter Wait = %v", err)
	}
}

func TestBurstThenRefill(t *testing.T) {
	l := New(100, 2, time.Minute)
	// 突发容量为 2：前两次放行。
	for range 2 {
		if !l.Allow("u1") {
			t.Fatal("突发容量内的请求应放行")
		}
	}
	if l.Allow("u1") {
		t.Fatal("超出突发容量应拒绝")
	}
	if !l.Allow("u2") {
		t.Fatal("不同 key 有独立桶")
	}
	time.Sleep(30 * time.Millisecond)
	if !l.Allow("u1") {
		t.Fatal("等待后应补充令牌")
	}
}

func TestAllowN(t *testing.T) {
	l := New(10, 3, time.Minute)
	if !l.AllowN("u1", 3) {
		t.Fatal("AllowN(3) 应成功")
	}
	if l.AllowN("u1", 1) {
		t.Fatal("令牌耗尽后应拒绝")
	}
	if l.AllowN("u1", -1) {
		t.Fatal("非法 n 不应放行")
	}
}

func TestWaitBlocksUntilTokenAvailable(t *testing.T) {
	l := New(50, 1, time.Minute) // 20ms 一个令牌
	if !l.Allow("bot") {
		t.Fatal("首次应放行")
	}
	start := time.Now()
	if err := l.Wait(context.Background(), "bot"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Fatalf("Wait 应立即返回吗？耗时仅 %v", elapsed)
	}
}

func TestWaitRespectsContext(t *testing.T) {
	l := New(0.5, 1, time.Minute) // 2s 一个令牌
	if !l.Allow("bot") {
		t.Fatal("首次应放行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, "bot"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want DeadlineExceeded", err)
	}
}

func TestBucketReclaim(t *testing.T) {
	l := New(10, 1, 20*time.Millisecond)
	l.Allow("a")
	l.Allow("b")
	if got := l.Buckets(); got != 2 {
		t.Fatalf("Buckets = %d, want 2", got)
	}
	time.Sleep(40 * time.Millisecond)
	if got := l.GC(); got != 2 {
		t.Fatalf("GC = %d, want 2", got)
	}
	if got := l.Buckets(); got != 0 {
		t.Fatalf("GC 后 Buckets = %d, want 0", got)
	}
}

func TestConcurrentAllow(t *testing.T) {
	l := New(1000, 50, time.Minute)
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 100 {
				l.Allow("shared")
			}
		}()
	}
	for range 8 {
		<-done
	}
	if l.Buckets() != 1 {
		t.Fatalf("同一 key 应只有一个桶, got %d", l.Buckets())
	}
}
