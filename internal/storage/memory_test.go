package storage

import (
	"context"
	"errors"
	"strings"
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

	if _, err := st.Get(ctx, "k"); !errors.Is(err, bot.ErrPermissionDenied) {
		t.Fatalf("Get = %v, want bot.ErrPermissionDenied", err)
	}
	if err := st.Set(ctx, "k", []byte("v"), 0); !errors.Is(err, bot.ErrPermissionDenied) {
		t.Fatalf("Set = %v, want bot.ErrPermissionDenied", err)
	}
	if err := st.Delete(ctx, "k"); !errors.Is(err, bot.ErrPermissionDenied) {
		t.Fatalf("Delete = %v, want bot.ErrPermissionDenied", err)
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

// TestMemoryCleanupInterval 验证内存实现尊重 cleanup_interval：自定义间隔生效、
// 0 关闭后台清理，且经注册表（storage.Open）读取同一配置键。
func TestMemoryCleanupInterval(t *testing.T) {
	ctx := context.Background()

	waitLen := func(t *testing.T, m *Memory, want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for m.Len() != want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := m.Len(); got != want {
			t.Fatalf("Len = %d, want %d", got, want)
		}
	}

	t.Run("自定义间隔清理过期条目", func(t *testing.T) {
		m := NewMemoryWithCleanup(20 * time.Millisecond)
		defer func() { _ = m.Close() }()
		if err := m.Set(ctx, "k", []byte("v"), 10*time.Millisecond); err != nil {
			t.Fatalf("Set: %v", err)
		}
		waitLen(t, m, 0)
	})

	t.Run("间隔为 0 时不启动后台清理", func(t *testing.T) {
		m := NewMemoryWithCleanup(0)
		defer func() { _ = m.Close() }()
		if err := m.Set(ctx, "k", []byte("v"), 10*time.Millisecond); err != nil {
			t.Fatalf("Set: %v", err)
		}
		time.Sleep(60 * time.Millisecond)
		if _, err := m.Get(ctx, "k"); !errors.Is(err, bot.ErrNotFound) {
			t.Fatalf("过期键应返回 ErrNotFound: %v", err)
		}
		if got := m.Len(); got != 1 {
			t.Fatalf("无后台清理时过期条目应留在内存里, Len = %d", got)
		}
	})

	t.Run("注册表读取 cleanup_interval", func(t *testing.T) {
		st, err := Open("memory", bot.NewConfig(map[string]any{"cleanup_interval": "20ms"}), nil)
		if err != nil {
			t.Fatalf("Open(memory): %v", err)
		}
		m, ok := st.(*Memory)
		if !ok {
			t.Fatalf("Open(memory) 返回 %T", st)
		}
		defer func() { _ = m.Close() }()
		if err := m.Set(ctx, "k", []byte("v"), 10*time.Millisecond); err != nil {
			t.Fatalf("Set: %v", err)
		}
		waitLen(t, m, 0)

		// cleanup_interval: 0（或缺失时）关闭后台清理。
		st, err = Open("memory", bot.NewConfig(map[string]any{"cleanup_interval": 0}), nil)
		if err != nil {
			t.Fatalf("Open(memory): %v", err)
		}
		m = st.(*Memory)
		defer func() { _ = m.Close() }()
		if err := m.Set(ctx, "k", []byte("v"), 10*time.Millisecond); err != nil {
			t.Fatalf("Set: %v", err)
		}
		time.Sleep(60 * time.Millisecond)
		if got := m.Len(); got != 1 {
			t.Fatalf("cleanup_interval=0 时不应清理, Len = %d", got)
		}
	})
}

// TestKeyLengthLimit 验证内存实现按 MaxKeyBytes 拒绝超长键：上限内的键可读写，
// 超长键的三个操作都返回可被 errors.Is 识别的 ErrKeyTooLong（SQL 后端同款校验，
// 见 sqlite/sqlstore 的用例）。
func TestKeyLengthLimit(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	max := strings.Repeat("k", MaxKeyBytes)
	if err := m.Set(ctx, max, []byte("v"), 0); err != nil {
		t.Fatalf("上限内的键应可写: %v", err)
	}
	if got, err := m.Get(ctx, max); err != nil || string(got) != "v" {
		t.Fatalf("上限内的键应可读: %q, %v", got, err)
	}

	long := strings.Repeat("k", MaxKeyBytes+1)
	if err := m.Set(ctx, long, []byte("v"), 0); !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("Set 超长键 = %v, want ErrKeyTooLong", err)
	}
	if _, err := m.Get(ctx, long); !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("Get 超长键 = %v, want ErrKeyTooLong", err)
	}
	if err := m.Delete(ctx, long); !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("Delete 超长键 = %v, want ErrKeyTooLong", err)
	}
	if m.Len() != 1 {
		t.Fatalf("超长键不应写入，Len = %d", m.Len())
	}
}

// TestScopedStorageIsolatesKeys 验证命名空间隔离：两个插件/两个适配器实例的同名
// 键互不覆盖，底层键带各自前缀，删除只影响自己的命名空间。
func TestScopedStorageIsolatesKeys(t *testing.T) {
	inner := NewMemory()
	defer func() { _ = inner.Close() }()
	ctx := context.Background()

	cases := []struct {
		name     string
		first    bot.Storage
		second   bot.Storage
		wantKeys []string // 底层存储中两个命名空间的键
	}{
		{
			name:     "插件",
			first:    PluginScoped("p-a", inner),
			second:   PluginScoped("p-b", inner),
			wantKeys: []string{"plugin:p-a:k", "plugin:p-b:k"},
		},
		{
			name:     "适配器实例",
			first:    AdapterScoped("bot-1", inner),
			second:   AdapterScoped("bot-2", inner),
			wantKeys: []string{"adapter:bot-1:k", "adapter:bot-2:k"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.first.Set(ctx, "k", []byte("first"), 0); err != nil {
				t.Fatalf("first.Set: %v", err)
			}
			if err := tc.second.Set(ctx, "k", []byte("second"), 0); err != nil {
				t.Fatalf("second.Set: %v", err)
			}
			for i, st := range []bot.Storage{tc.first, tc.second} {
				want := []string{"first", "second"}[i]
				got, err := st.Get(ctx, "k")
				if err != nil || string(got) != want {
					t.Fatalf("第 %d 个视图 Get = %q, %v, want %q", i, got, err, want)
				}
			}
			for _, key := range tc.wantKeys {
				if _, err := inner.Get(ctx, key); err != nil {
					t.Fatalf("底层键 %q 应存在: %v", key, err)
				}
			}

			// 删除只影响自己的命名空间。
			if err := tc.first.Delete(ctx, "k"); err != nil {
				t.Fatalf("first.Delete: %v", err)
			}
			if _, err := tc.first.Get(ctx, "k"); !errors.Is(err, bot.ErrNotFound) {
				t.Fatalf("删除后 first.Get = %v, want ErrNotFound", err)
			}
			if got, err := tc.second.Get(ctx, "k"); err != nil || string(got) != "second" {
				t.Fatalf("second 的键不应被删除: %q, %v", got, err)
			}
			if _, err := inner.Get(ctx, tc.wantKeys[0]); !errors.Is(err, bot.ErrNotFound) {
				t.Fatalf("底层键 %q 应已删除: %v", tc.wantKeys[0], err)
			}
		})
	}
}
