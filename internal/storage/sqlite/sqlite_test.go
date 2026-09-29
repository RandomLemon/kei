package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/internal/storage/sqlstore"
	"github.com/RandomLemon/kei/pkg/bot"

	_ "github.com/RandomLemon/kei/internal/storage/sqlite"
)

// openDB 打开一个指向临时文件的 GORM 连接，供需要注入时钟的用例使用。
func openDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	return db
}

func TestSQLiteRoundTrip(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "k.db")
	st, err := storage.Open("sqlite", bot.NewConfig(map[string]any{"dsn": dsn}))
	if err != nil {
		t.Fatalf("Open(sqlite): %v", err)
	}
	defer func() { _ = st.(interface{ Close() error }).Close() }()
	ctx := context.Background()

	if _, err := st.Get(ctx, "missing"); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("Get 缺失键 = %v, want ErrNotFound", err)
	}

	value := []byte("v1")
	if err := st.Set(ctx, "k", value, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value[0] = 'X' // 写入后修改入参不得影响已存值
	got, err := st.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("Get = %q, want v1（必须拷贝入参）", got)
	}
	got[0] = 'Y' // 修改返回值不得影响已存值
	again, err := st.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(again) != "v1" {
		t.Fatalf("二次 Get = %q, want v1（必须拷贝返回值）", again)
	}

	if err := st.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(ctx, "k"); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("删除后 Get = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, "k"); err != nil {
		t.Fatalf("重复 Delete 不应报错: %v", err)
	}
}

func TestSQLiteCloseIsIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "k.db")
	st, err := storage.Open("sqlite", bot.NewConfig(map[string]any{"dsn": dsn}))
	if err != nil {
		t.Fatalf("Open(sqlite): %v", err)
	}
	c := st.(interface{ Close() error })
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
}

func TestSQLitePersistence(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "k.db")
	ctx := context.Background()

	first, err := storage.Open("sqlite", bot.NewConfig(map[string]any{"dsn": dsn}))
	if err != nil {
		t.Fatalf("Open(sqlite): %v", err)
	}
	if err := first.Set(ctx, "keep", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := first.(interface{ Close() error }).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := storage.Open("sqlite", bot.NewConfig(map[string]any{"dsn": dsn}))
	if err != nil {
		t.Fatalf("重新 Open(sqlite): %v", err)
	}
	defer func() { _ = second.(interface{ Close() error }).Close() }()
	got, err := second.Get(ctx, "keep")
	if err != nil {
		t.Fatalf("重新打开后 Get: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("重新打开后 Get = %q, want v", got)
	}
}

func TestSQLiteTTLLazyExpiry(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "k.db")
	db := openDB(t, dsn)
	now := time.Now()
	st, err := sqlstore.NewWithClock(db, 0, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewWithClock: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	if err := st.Set(ctx, "short", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := st.Get(ctx, "short"); err != nil {
		t.Fatalf("未过期时应可读: %v", err)
	}
	now = now.Add(61 * time.Second)
	if _, err := st.Get(ctx, "short"); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("过期后 Get = %v, want ErrNotFound", err)
	}
	var count int64
	if err := db.Model(&sqlstore.Entry{}).Where("key = ?", "short").Count(&count).Error; err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Fatalf("惰性清理后表内仍有 %d 行，want 0", count)
	}
}

func TestSQLiteBackgroundCleanup(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "k.db")
	db := openDB(t, dsn)
	st, err := sqlstore.NewWithClock(db, 20*time.Millisecond, time.Now)
	if err != nil {
		t.Fatalf("NewWithClock: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	if err := st.Set(ctx, "short", []byte("v"), 20*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int64
		if err := db.Model(&sqlstore.Entry{}).Where("key = ?", "short").Count(&count).Error; err != nil {
			t.Fatalf("Count: %v", err)
		}
		if count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("后台清理未在 2s 内删除过期行")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
