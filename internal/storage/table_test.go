package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// newMemoryStore 构造不做后台清理的内存实现，并登记 Close。
func newMemoryStore(t *testing.T) *Memory {
	t.Helper()
	m := NewMemoryWithCleanup(0)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// tableCRUD 对任意 bot.TableStorage 实现跑一遍公开语义。
//
// 供内存、SQL 与方言包复用：同一组断言覆盖三个内置后端，避免出现「内存能过、
// SQL 悄悄不同」的偏差。
func tableCRUD(t *testing.T, ts bot.TableStorage) {
	t.Helper()
	ctx := context.Background()

	// 空库：无表返回空切片，且不是 nil（调用方 do len/range 时不必再判空）。
	names, err := ts.ListTables(ctx)
	if err != nil {
		t.Fatalf("ListTables 空库: %v", err)
	}
	if names == nil || len(names) != 0 {
		t.Fatalf("ListTables 空库 = %v, want 非 nil 空切片", names)
	}

	// 表必须显式创建：行操作不会自动建表。
	if err := ts.InsertRow(ctx, "users", "a", []byte("a1")); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("InsertRow 缺表 = %v, want ErrTableNotFound", err)
	}

	if err := ts.CreateTable(ctx, "users"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if err := ts.CreateTable(ctx, "users"); !errors.Is(err, bot.ErrTableExists) {
		t.Fatalf("重复 CreateTable = %v, want ErrTableExists", err)
	}

	// 插入、列表、排序与副本隔离。
	if err := ts.InsertRow(ctx, "users", "b", []byte("b1")); err != nil {
		t.Fatalf("InsertRow b: %v", err)
	}
	value := []byte("a1")
	if err := ts.InsertRow(ctx, "users", "a", value); err != nil {
		t.Fatalf("InsertRow a: %v", err)
	}
	value[0] = 'X' // 插入后修改入参不得影响已存值
	rows, err := ts.ListRows(ctx, "users")
	if err != nil {
		t.Fatalf("ListRows: %v", err)
	}
	if len(rows) != 2 || rows[0].Key != "a" || rows[1].Key != "b" {
		t.Fatalf("ListRows = %+v, want 键 a,b 升序", rows)
	}
	if string(rows[0].Value) != "a1" {
		t.Fatalf("ListRows a = %q, want a1（必须拷贝入参）", rows[0].Value)
	}
	rows[0].Value[0] = 'Y' // 修改返回值不得影响已存值
	got, err := ts.GetRow(ctx, "users", "a")
	if err != nil {
		t.Fatalf("GetRow: %v", err)
	}
	if string(got) != "a1" {
		t.Fatalf("GetRow = %q, want a1（必须拷贝返回值）", got)
	}
	got[0] = 'Z'
	again, err := ts.GetRow(ctx, "users", "a")
	if err != nil || string(again) != "a1" {
		t.Fatalf("二次 GetRow = %q, %v, want a1", again, err)
	}

	// 行级错误语义。
	if err := ts.InsertRow(ctx, "users", "a", []byte("dup")); !errors.Is(err, bot.ErrRowExists) {
		t.Fatalf("重复 InsertRow = %v, want ErrRowExists", err)
	}
	if _, err := ts.GetRow(ctx, "users", "missing"); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("GetRow 缺行 = %v, want ErrNotFound", err)
	}
	if err := ts.UpdateRow(ctx, "users", "missing", []byte("v")); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("UpdateRow 缺行 = %v, want ErrNotFound", err)
	}
	// 新值等于旧值也必须成功（SQL 后端可能报告 0 行受影响）。
	if err := ts.UpdateRow(ctx, "users", "a", []byte("a1")); err != nil {
		t.Fatalf("UpdateRow 相同值: %v", err)
	}
	if err := ts.UpdateRow(ctx, "users", "a", []byte("a2")); err != nil {
		t.Fatalf("UpdateRow: %v", err)
	}
	if got, err := ts.GetRow(ctx, "users", "a"); err != nil || string(got) != "a2" {
		t.Fatalf("UpdateRow 后 GetRow = %q, %v, want a2", got, err)
	}
	if err := ts.DeleteRow(ctx, "users", "missing"); err != nil {
		t.Fatalf("DeleteRow 缺行 = %v, want nil", err)
	}

	// 空值（nil 或零长度）按空值处理，两个后端都不得因此失败，且读回 nil。
	if err := ts.InsertRow(ctx, "users", "n", nil); err != nil {
		t.Fatalf("InsertRow nil 值: %v", err)
	}
	if err := ts.InsertRow(ctx, "users", "e", []byte{}); err != nil {
		t.Fatalf("InsertRow 零长度值: %v", err)
	}
	for _, key := range []string{"n", "e"} {
		got, err := ts.GetRow(ctx, "users", key)
		if err != nil || len(got) != 0 || got != nil {
			t.Fatalf("GetRow(%s) = %q, %v, want nil 空值", key, got, err)
		}
	}

	// 缺表的其余方法统一返回 ErrTableNotFound。
	if _, err := ts.GetRow(ctx, "missing", "a"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("GetRow 缺表 = %v, want ErrTableNotFound", err)
	}
	if err := ts.UpdateRow(ctx, "missing", "a", nil); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("UpdateRow 缺表 = %v, want ErrTableNotFound", err)
	}
	if err := ts.DeleteRow(ctx, "missing", "a"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("DeleteRow 缺表 = %v, want ErrTableNotFound", err)
	}
	if _, err := ts.ListRows(ctx, "missing"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("ListRows 缺表 = %v, want ErrTableNotFound", err)
	}

	// 重命名保留行数据；同名重命名是幂等成功。
	if err := ts.RenameTable(ctx, "users", "users"); err != nil {
		t.Fatalf("RenameTable 同名: %v", err)
	}
	if err := ts.RenameTable(ctx, "missing", "other"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("RenameTable 缺源 = %v, want ErrTableNotFound", err)
	}
	if err := ts.CreateTable(ctx, "taken"); err != nil {
		t.Fatalf("CreateTable taken: %v", err)
	}
	if err := ts.RenameTable(ctx, "users", "taken"); !errors.Is(err, bot.ErrTableExists) {
		t.Fatalf("RenameTable 目标已存在 = %v, want ErrTableExists", err)
	}
	if err := ts.RenameTable(ctx, "users", "people"); err != nil {
		t.Fatalf("RenameTable: %v", err)
	}
	if _, err := ts.GetRow(ctx, "users", "a"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("重命名后旧表 GetRow = %v, want ErrTableNotFound", err)
	}
	if got, err := ts.GetRow(ctx, "people", "a"); err != nil || string(got) != "a2" {
		t.Fatalf("重命名后新表 GetRow = %q, %v, want a2", got, err)
	}

	// 表名升序。
	names, err = ts.ListTables(ctx)
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if len(names) != 2 || names[0] != "people" || names[1] != "taken" {
		t.Fatalf("ListTables = %v, want [people taken]", names)
	}

	// 删除表连带删行，且缺表幂等。
	if err := ts.DropTable(ctx, "people"); err != nil {
		t.Fatalf("DropTable: %v", err)
	}
	if err := ts.DropTable(ctx, "people"); err != nil {
		t.Fatalf("重复 DropTable = %v, want nil", err)
	}
	if _, err := ts.ListRows(ctx, "people"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("DropTable 后 ListRows = %v, want ErrTableNotFound", err)
	}
	if err := ts.CreateTable(ctx, "people"); err != nil {
		t.Fatalf("重建同名表: %v", err)
	}
	if rows, err := ts.ListRows(ctx, "people"); err != nil || len(rows) != 0 {
		t.Fatalf("重建后 ListRows = %v, %v, want 空", rows, err)
	}

	// 行键只在表内唯一：不同表可以有相同行键，互不覆盖。
	if err := ts.InsertRow(ctx, "people", "a", []byte("people-a")); err != nil {
		t.Fatalf("people.InsertRow: %v", err)
	}
	if err := ts.InsertRow(ctx, "taken", "a", []byte("taken-a")); err != nil {
		t.Fatalf("taken.InsertRow: %v", err)
	}
	if got, err := ts.GetRow(ctx, "people", "a"); err != nil || string(got) != "people-a" {
		t.Fatalf("people.GetRow = %q, %v, want people-a", got, err)
	}
	if got, err := ts.GetRow(ctx, "taken", "a"); err != nil || string(got) != "taken-a" {
		t.Fatalf("taken.GetRow = %q, %v, want taken-a（不同表的同名行键不得互相覆盖）", got, err)
	}
}

// TestMemoryTableCRUD 断言内存后端的表与行 CRUD 语义。
func TestMemoryTableCRUD(t *testing.T) {
	m := newMemoryStore(t)
	tableCRUD(t, m)

	// 表数据与 KV 是两套空间：行不计入 Len，也不受影响于 KV 键。
	if err := m.Set(context.Background(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := m.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1（表行不计入 KV 条目数）", got)
	}
}

// TestTableNameValidation 断言表名/行键的空值与长度上限。
func TestTableNameValidation(t *testing.T) {
	m := newMemoryStore(t)
	ctx := context.Background()
	long := strings.Repeat("x", MaxKeyBytes+1)

	if err := m.CreateTable(ctx, ""); err == nil || !strings.Contains(err.Error(), "表名不能为空") {
		t.Fatalf("CreateTable 空名 = %v, want 表名不能为空", err)
	}
	if err := m.CreateTable(ctx, long); !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("CreateTable 超长名 = %v, want ErrKeyTooLong", err)
	}
	// 上限本身必须可用（按字节 ≤ 191）。
	exact := strings.Repeat("x", MaxKeyBytes)
	if err := m.CreateTable(ctx, exact); err != nil {
		t.Fatalf("CreateTable 191 字节名: %v", err)
	}
	if err := m.InsertRow(ctx, exact, strings.Repeat("k", MaxKeyBytes), nil); err != nil {
		t.Fatalf("InsertRow 191 字节键: %v", err)
	}
	if err := m.CreateTable(ctx, "t"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if err := m.InsertRow(ctx, "t", "", nil); err == nil || !strings.Contains(err.Error(), "行键不能为空") {
		t.Fatalf("InsertRow 空键 = %v, want 行键不能为空", err)
	}
	if err := m.InsertRow(ctx, "t", long, nil); !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("InsertRow 超长键 = %v, want ErrKeyTooLong", err)
	}
	if _, err := m.GetRow(ctx, "t", ""); err == nil {
		t.Fatal("GetRow 空键 = nil, want 错误")
	}
}

// TestScopedTablesIsolateTables 断言插件/适配器表视图只看到自己的表，
// 且底层表名带命名空间前缀。
func TestScopedTablesIsolateTables(t *testing.T) {
	root := newMemoryStore(t)
	ctx := context.Background()

	a := PluginTables("a", root)
	b := PluginTables("b", root)
	if a == nil || b == nil {
		t.Fatal("PluginTables 在内存后端上返回 nil")
	}
	for _, view := range []bot.TableStorage{a, b} {
		if err := view.CreateTable(ctx, "users"); err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
	}
	if err := a.InsertRow(ctx, "users", "k", []byte("from-a")); err != nil {
		t.Fatalf("a.InsertRow: %v", err)
	}
	if err := b.InsertRow(ctx, "users", "k", []byte("from-b")); err != nil {
		t.Fatalf("b.InsertRow: %v", err)
	}

	// 视图只看到逻辑名，且与另一个插件隔离。
	for name, view := range map[string]bot.TableStorage{"a": a, "b": b} {
		names, err := view.ListTables(ctx)
		if err != nil {
			t.Fatalf("%s.ListTables: %v", name, err)
		}
		if len(names) != 1 || names[0] != "users" {
			t.Fatalf("%s.ListTables = %v, want [users]", name, names)
		}
	}
	// 根实例看到的是带前缀的物理表名。
	names, err := root.ListTables(ctx)
	if err != nil {
		t.Fatalf("root.ListTables: %v", err)
	}
	if len(names) != 2 || names[0] != "plugin:a:users" || names[1] != "plugin:b:users" {
		t.Fatalf("root.ListTables = %v, want [plugin:a:users plugin:b:users]", names)
	}
	// 行键原样传递：底层按同一行键存放，值由表名（前缀）区分。
	for table, want := range map[string]string{"plugin:a:users": "from-a", "plugin:b:users": "from-b"} {
		if got, err := root.GetRow(ctx, table, "k"); err != nil || string(got) != want {
			t.Fatalf("root.GetRow(%s) = %q, %v, want %q", table, got, err, want)
		}
	}

	if got, err := a.GetRow(ctx, "users", "k"); err != nil || string(got) != "from-a" {
		t.Fatalf("a.GetRow = %q, %v, want from-a", got, err)
	}
	if err := a.DropTable(ctx, "users"); err != nil {
		t.Fatalf("a.DropTable: %v", err)
	}
	if _, err := a.GetRow(ctx, "users", "k"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("删除后 a.GetRow = %v, want ErrTableNotFound", err)
	}
	if got, err := b.GetRow(ctx, "users", "k"); err != nil || string(got) != "from-b" {
		t.Fatalf("另一视图 b.GetRow = %q, %v, want from-b（不得被连带删除）", got, err)
	}

	// 前缀计入长度上限：逻辑名超过 191-len(prefix) 字节时在视图侧就被拒绝。
	if err := a.CreateTable(ctx, strings.Repeat("x", MaxKeyBytes)); !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("视图超长表名 = %v, want ErrKeyTooLong", err)
	}
}

// TestAdapterTablesScoped 断言适配器视图使用 adapter:<botID>: 前缀。
func TestAdapterTablesScoped(t *testing.T) {
	root := newMemoryStore(t)
	ctx := context.Background()

	view := AdapterTables("bot-1", root)
	if view == nil {
		t.Fatal("AdapterTables 在内存后端上返回 nil")
	}
	if err := view.CreateTable(ctx, "t"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	names, err := root.ListTables(ctx)
	if err != nil {
		t.Fatalf("root.ListTables: %v", err)
	}
	if len(names) != 1 || names[0] != "adapter:bot-1:t" {
		t.Fatalf("root.ListTables = %v, want [adapter:bot-1:t]", names)
	}
}

// kvOnly 只实现 bot.Storage，不实现 bot.TableStorage：模拟注入的自定义后端。
type kvOnly struct{}

func (kvOnly) Get(context.Context, string) ([]byte, error) { return nil, bot.ErrNotFound }

func (kvOnly) Set(context.Context, string, []byte, time.Duration) error { return nil }

func (kvOnly) Delete(context.Context, string) error { return nil }

// TestTablesUnsupported 断言未实现 bot.TableStorage 的 Storage 得到 nil 视图。
func TestTablesUnsupported(t *testing.T) {
	var inner bot.Storage = kvOnly{}
	if got := PluginTables("p", inner); got != nil {
		t.Fatalf("PluginTables(不支持表的后端) = %v, want nil", got)
	}
	if got := AdapterTables("bot", inner); got != nil {
		t.Fatalf("AdapterTables(不支持表的后端) = %v, want nil", got)
	}
}

// TestDeniedTables 断言未授权视图的所有表方法都返回 bot.ErrPermissionDenied。
func TestDeniedTables(t *testing.T) {
	ctx := context.Background()
	ts := DeniedTables()

	checks := map[string]error{
		"CreateTable": ts.CreateTable(ctx, "t"),
		"RenameTable": ts.RenameTable(ctx, "t", "u"),
		"DropTable":   ts.DropTable(ctx, "t"),
		"InsertRow":   ts.InsertRow(ctx, "t", "k", nil),
		"UpdateRow":   ts.UpdateRow(ctx, "t", "k", nil),
		"DeleteRow":   ts.DeleteRow(ctx, "t", "k"),
	}
	for name, err := range checks {
		if !errors.Is(err, bot.ErrPermissionDenied) {
			t.Fatalf("%s = %v, want bot.ErrPermissionDenied", name, err)
		}
	}
	if _, err := ts.ListTables(ctx); !errors.Is(err, bot.ErrPermissionDenied) {
		t.Fatalf("ListTables = %v, want bot.ErrPermissionDenied", err)
	}
	if _, err := ts.GetRow(ctx, "t", "k"); !errors.Is(err, bot.ErrPermissionDenied) {
		t.Fatalf("GetRow = %v, want bot.ErrPermissionDenied", err)
	}
	if _, err := ts.ListRows(ctx, "t"); !errors.Is(err, bot.ErrPermissionDenied) {
		t.Fatalf("ListRows = %v, want bot.ErrPermissionDenied", err)
	}
}

// unorderedStore 让 ListTables 逆序返回，用来断言表视图自己排序，
// 而不是依赖底层 bot.TableStorage 实现恰好有序。
type unorderedStore struct{ *Memory }

func (unorderedStore) ListTables(context.Context) ([]string, error) {
	return []string{"plugin:p:b", "plugin:p:a"}, nil
}

// TestScopedTablesListSorts 断言视图剥离前缀后按逻辑名升序返回。
func TestScopedTablesListSorts(t *testing.T) {
	m := newMemoryStore(t)
	view := PluginTables("p", unorderedStore{m})
	if view == nil {
		t.Fatal("PluginTables 返回 nil")
	}
	names, err := view.ListTables(context.Background())
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("ListTables = %v, want [a b]", names)
	}
}

// TestMemoryTableOrdering 断言排序按字节序，而不是按语言/区域规则。
func TestMemoryTableOrdering(t *testing.T) {
	m := newMemoryStore(t)
	ctx := context.Background()

	for _, name := range []string{"alpha", "Zeta"} {
		if err := m.CreateTable(ctx, name); err != nil {
			t.Fatalf("CreateTable(%s): %v", name, err)
		}
	}
	names, err := m.ListTables(ctx)
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if len(names) != 2 || names[0] != "Zeta" || names[1] != "alpha" {
		t.Fatalf("ListTables = %v, want [Zeta alpha]（字节序：'Z' < 'a'）", names)
	}

	if err := m.CreateTable(ctx, "order"); err != nil {
		t.Fatalf("CreateTable order: %v", err)
	}
	for _, key := range []string{"a", "B"} {
		if err := m.InsertRow(ctx, "order", key, nil); err != nil {
			t.Fatalf("InsertRow(%s): %v", key, err)
		}
	}
	rows, err := m.ListRows(ctx, "order")
	if err != nil {
		t.Fatalf("ListRows: %v", err)
	}
	if len(rows) != 2 || rows[0].Key != "B" || rows[1].Key != "a" {
		t.Fatalf("ListRows = %+v, want 键 [B a]（字节序：'B' < 'a'）", rows)
	}
}
