package storage_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"

	_ "github.com/RandomLemon/kei/internal/storage/mysql"
	_ "github.com/RandomLemon/kei/internal/storage/sqlite"
)

func TestKinds(t *testing.T) {
	got := storage.Kinds()
	want := []string{"memory", "mysql", "sqlite"}
	if !slices.Equal(got, want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
}

func TestOpenMemoryRoundTrip(t *testing.T) {
	st, err := storage.Open("memory", bot.NewConfig(nil))
	if err != nil {
		t.Fatalf("Open(memory): %v", err)
	}
	if c, ok := st.(interface{ Close() error }); ok {
		defer func() { _ = c.Close() }()
	}
	ctx := context.Background()
	if err := st.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := st.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("Get = %q, want v", got)
	}
}

func TestOpenUnknownType(t *testing.T) {
	_, err := storage.Open("nosuch", bot.NewConfig(nil))
	if err == nil {
		t.Fatal("Open(nosuch) 应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, `未知存储类型 "nosuch"`) {
		t.Fatalf("错误文本缺少未知类型描述: %q", msg)
	}
	if !strings.Contains(msg, "memory, mysql, sqlite") {
		t.Fatalf("错误文本缺少已注册类型列表: %q", msg)
	}
}
