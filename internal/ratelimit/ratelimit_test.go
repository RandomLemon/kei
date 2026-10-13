package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDisabledLimiterAllowsEverything(t *testing.T) {
	l := New(0, 0, time.Minute, 0)
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
	l := New(100, 2, time.Minute, 0)
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
	l := New(10, 3, time.Minute, 0)
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
	l := New(50, 1, time.Minute, 0) // 20ms 一个令牌
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
	l := New(0.5, 1, time.Minute, 0) // 2s 一个令牌
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
	l := New(10, 1, 20*time.Millisecond, 0)
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
	l := New(1000, 50, time.Minute, 0)
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

// TestBucketTableBounded 验证桶表容量有上限：大量不同 key 只保留最近使用的一批，
// maxKeys <= 0 时使用默认上限。
func TestBucketTableBounded(t *testing.T) {
	cases := []struct {
		name    string
		maxKeys int // New 的入参，0 表示用默认上限
		want    int
	}{
		{name: "显式上限", maxKeys: 4, want: 4},
		{name: "默认上限", maxKeys: 0, want: defaultMaxKeys},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New(100, 1, time.Minute, tc.maxKeys)
			for i := range tc.want + 1 {
				l.Allow(fmt.Sprintf("k-%d", i))
			}
			if got := l.Buckets(); got != tc.want {
				t.Fatalf("Buckets = %d, 期望 %d（上限）", got, tc.want)
			}
		})
	}
}

// TestEvictionKeepsMostRecentlyUsed 验证淘汰选最久未使用的桶：最近被访问过的 key
// 保留原有令牌状态，被淘汰的 key 重建为满桶。
func TestEvictionKeepsMostRecentlyUsed(t *testing.T) {
	l := New(0.01, 1, time.Minute, 2) // 100s 一个令牌：测试期间不会补充
	if !l.Allow("k1") || !l.Allow("k2") {
		t.Fatal("突发容量内应放行")
	}
	if l.Allow("k1") {
		t.Fatal("k1 令牌已用完，应拒绝")
	}
	// k1 刚被访问过，插入新 key 时应淘汰更久未使用的 k2。
	if !l.Allow("k3") {
		t.Fatal("新 key 应重建为满桶并放行")
	}
	if l.Allow("k1") {
		t.Fatal("最近被访问过的 k1 不应被淘汰（淘汰后会重建为满桶而放行）")
	}
	if !l.Allow("k2") {
		t.Fatal("被淘汰的 k2 应重建为满桶")
	}
	if got := l.Buckets(); got != 2 {
		t.Fatalf("Buckets = %d, 期望 2", got)
	}
}

// TestRetryAfterUsesFreshTokens 验证 retryAfter 按当前时间补充令牌后再估算：速率
// 1/s 的桶耗尽 500ms 后应只剩约 500ms，而不是按陈旧快照算成整整 1s。
func TestRetryAfterUsesFreshTokens(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(1, 1, time.Minute, 0)
	l.now = func() time.Time { return now }

	if !l.Allow("k") {
		t.Fatal("初始令牌应放行")
	}
	now = now.Add(500 * time.Millisecond)
	if d := l.retryAfter("k"); d <= 0 || d > 600*time.Millisecond {
		t.Fatalf("retryAfter = %v, 期望约 500ms（陈旧快照会算成 1s）", d)
	}
	if d := l.retryAfter("missing"); d != 0 {
		t.Fatalf("未知 key 的 retryAfter = %v, 期望 0", d)
	}
	// 补充后令牌已够：等待归零。
	now = now.Add(time.Second)
	if d := l.retryAfter("k"); d != 0 {
		t.Fatalf("令牌补满后的 retryAfter = %v, 期望 0", d)
	}
}

// TestWaitDoesNotOversleep 验证 Wait 的实际等待不显著超过速率推导值：rate=20/s
// （50ms 一个令牌）、burst=1 时连续 3 次 Wait 约需 150ms，而不是每轮多睡一份
// （那会到 300ms）。
func TestWaitDoesNotOversleep(t *testing.T) {
	const rate = 20.0
	const n = 3
	l := New(rate, 1, time.Minute, 0)
	if !l.Allow("k") {
		t.Fatal("初始令牌应放行")
	}

	want := time.Duration(float64(n) / rate * float64(time.Second))
	start := time.Now()
	for range n {
		if err := l.Wait(context.Background(), "k"); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > want*3/2 {
		t.Fatalf("Wait 总耗时 %v 显著超过速率推导值 %v", elapsed, want)
	}
}
