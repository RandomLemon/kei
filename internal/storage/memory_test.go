package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func TestMemoryRoundTrip(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	if _, err := m.Get(ctx, "missing"); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("Get 缺失键 = %v, want ErrNotFound", err)
	}

	value := []byte("v1")
	if err := m.Set(ctx, "k", value, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value[0] = 'X' // 写入后修改入参不得影响已存值
	got, err := m.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("Get = %q, want v1（必须拷贝入参）", got)
	}
	got[0] = 'Y' // 修改返回值不得影响已存值
	again, err := m.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(again) != "v1" {
		t.Fatalf("二次 Get = %q, want v1（必须拷贝返回值）", again)
	}

	if err := m.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := m.Get(ctx, "k"); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("删除后 Get = %v, want ErrNotFound", err)
	}
	if err := m.Delete(ctx, "k"); err != nil {
		t.Fatalf("重复 Delete 不应报错: %v", err)
	}
}

func TestMemoryTTL(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	if err := m.Set(ctx, "short", []byte("v"), 20*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := m.Get(ctx, "short"); err != nil {
		t.Fatalf("未过期时应可读: %v", err)
	}
	time.Sleep(35 * time.Millisecond)
	if _, err := m.Get(ctx, "short"); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("过期后 Get = %v, want ErrNotFound", err)
	}
	if got := m.Len(); got == 0 {
		t.Fatal("过期条目在 janitor 触发前仍占用映射（Len 仅用于观察）")
	}
}

func TestMemoryContextCanceled(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get = %v, want context.Canceled", err)
	}
	if err := m.Set(ctx, "k", nil, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Set = %v, want context.Canceled", err)
	}
	if err := m.Delete(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete = %v, want context.Canceled", err)
	}
}

func TestMemoryCloseIsIdempotent(t *testing.T) {
	m := NewMemory()
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
}

func TestDeniedStorage(t *testing.T) {
	ctx := context.Background()
	st := Denied()

	if _, err := st.Get(ctx, "k"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Get = %v, want ErrPermissionDenied", err)
	}
	if err := st.Set(ctx, "k", []byte("v"), 0); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Set = %v, want ErrPermissionDenied", err)
	}
	if err := st.Delete(ctx, "k"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Delete = %v, want ErrPermissionDenied", err)
	}
}

func TestMemoryConcurrentAccess(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	done := make(chan struct{})
	for w := range 8 {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			key := string(rune('a' + w))
			for range 100 {
				_ = m.Set(ctx, key, []byte("v"), time.Minute)
				_, _ = m.Get(ctx, key)
				_ = m.Delete(ctx, key)
			}
		}(w)
	}
	for range 8 {
		<-done
	}
}
