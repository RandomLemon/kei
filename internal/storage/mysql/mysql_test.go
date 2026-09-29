package mysql_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"

	_ "github.com/RandomLemon/kei/internal/storage/mysql"
)

// dsn 返回测试用 MySQL DSN；未设置 KEI_TEST_MYSQL_DSN 时跳过。
//
// 需要形如 user:pass@tcp(host:3306)/db?parseTime=true 的 DSN。
func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("KEI_TEST_MYSQL_DSN")
	if v == "" {
		t.Skip("未设置 KEI_TEST_MYSQL_DSN，跳过 MySQL 用例")
	}
	return v
}

func TestMySQLRoundTrip(t *testing.T) {
	st, err := storage.Open("mysql", bot.NewConfig(map[string]any{"dsn": dsn(t)}))
	if err != nil {
		t.Fatalf("Open(mysql): %v", err)
	}
	defer func() { _ = st.(interface{ Close() error }).Close() }()
	ctx := context.Background()

	key := "test-roundtrip"
	_ = st.Delete(ctx, key)

	if _, err := st.Get(ctx, key); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("Get 缺失键 = %v, want ErrNotFound", err)
	}
	value := []byte("v1")
	if err := st.Set(ctx, key, value, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value[0] = 'X'
	got, err := st.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("Get = %q, want v1", got)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestMySQLTTL(t *testing.T) {
	st, err := storage.Open("mysql", bot.NewConfig(map[string]any{"dsn": dsn(t)}))
	if err != nil {
		t.Fatalf("Open(mysql): %v", err)
	}
	defer func() { _ = st.(interface{ Close() error }).Close() }()
	ctx := context.Background()

	key := "test-ttl"
	_ = st.Delete(ctx, key)
	if err := st.Set(ctx, key, []byte("v"), 30*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := st.Get(ctx, key); err != nil {
		t.Fatalf("未过期时应可读: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := st.Get(ctx, key); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("过期后 Get = %v, want ErrNotFound", err)
	}
	_ = st.Delete(ctx, key)
}
