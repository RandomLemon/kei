package sqlstore

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/RandomLemon/kei/pkg/bot"
)

// newTableStore 在临时 SQLite 文件上构造 Store 并登记 Close。
//
// 用文件 DSN 而不是 ":memory:"：内存库按连接分裂，连接池换一条连接就看不到
// 已建的表，测试会随机失败。
func newTableStore(t *testing.T) *Store {
	t.Helper()
	db := openGorm(t, filepath.Join(t.TempDir(), "table.db"))
	s, err := New(db, 0, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSQLStoreTable 断言 SQL 后端的表与行 CRUD 语义。
func TestSQLStoreTable(t *testing.T) {
	s := newTableStore(t)
	ctx := context.Background()

	// 空库返回非 nil 空切片。
	names, err := s.ListTables(ctx)
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if names == nil || len(names) != 0 {
		t.Fatalf("ListTables 空库 = %#v, want 非 nil 空切片", names)
	}

	if err := s.InsertRow(ctx, "users", "a", []byte("a1")); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("InsertRow 缺表 = %v, want ErrTableNotFound", err)
	}
	if err := s.CreateTable(ctx, "users"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if err := s.CreateTable(ctx, "users"); !errors.Is(err, bot.ErrTableExists) {
		t.Fatalf("重复 CreateTable = %v, want ErrTableExists", err)
	}

	value := []byte("a1")
	if err := s.InsertRow(ctx, "users", "a", value); err != nil {
		t.Fatalf("InsertRow: %v", err)
	}
	value[0] = 'X'
	if err := s.InsertRow(ctx, "users", "a", []byte("dup")); !errors.Is(err, bot.ErrRowExists) {
		t.Fatalf("重复 InsertRow = %v, want ErrRowExists", err)
	}

	got, err := s.GetRow(ctx, "users", "a")
	if err != nil || string(got) != "a1" {
		t.Fatalf("GetRow = %q, %v, want a1（必须拷贝入参/返回值）", got, err)
	}
	got[0] = 'Y'
	again, err := s.GetRow(ctx, "users", "a")
	if err != nil || string(again) != "a1" {
		t.Fatalf("二次 GetRow = %q, %v, want a1（必须拷贝返回值）", again, err)
	}

	// 新值与旧值相同时 MySQL 可能报告 0 行受影响；语义必须是成功。
	if err := s.UpdateRow(ctx, "users", "a", []byte("a1")); err != nil {
		t.Fatalf("UpdateRow 相同值 = %v, want nil", err)
	}
	if err := s.UpdateRow(ctx, "users", "missing", nil); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("UpdateRow 缺行 = %v, want ErrNotFound", err)
	}
	if err := s.UpdateRow(ctx, "users", "a", []byte("a2")); err != nil {
		t.Fatalf("UpdateRow: %v", err)
	}
	if got, err := s.GetRow(ctx, "users", "a"); err != nil || string(got) != "a2" {
		t.Fatalf("UpdateRow 后 GetRow = %q, %v, want a2", got, err)
	}

	// 行排序 + 空切片非 nil。
	if err := s.InsertRow(ctx, "users", "b", nil); err != nil {
		t.Fatalf("InsertRow nil 值: %v", err)
	}
	rows, err := s.ListRows(ctx, "users")
	if err != nil {
		t.Fatalf("ListRows: %v", err)
	}
	if len(rows) != 2 || rows[0].Key != "a" || rows[1].Key != "b" {
		t.Fatalf("ListRows = %+v, want 键 a,b 升序", rows)
	}
	if len(rows[1].Value) != 0 {
		t.Fatalf("nil 值行 = %q, want 空值", rows[1].Value)
	}
	rows[0].Value[0] = 'Z'
	if got, err := s.GetRow(ctx, "users", "a"); err != nil || string(got) != "a2" {
		t.Fatalf("修改 ListRows 返回值后 GetRow = %q, %v, want a2", got, err)
	}

	// 缺表与缺行的错误区分。
	if _, err := s.GetRow(ctx, "missing", "a"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("GetRow 缺表 = %v, want ErrTableNotFound", err)
	}
	if _, err := s.GetRow(ctx, "users", "missing"); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("GetRow 缺行 = %v, want ErrNotFound", err)
	}
	if _, err := s.ListRows(ctx, "missing"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("ListRows 缺表 = %v, want ErrTableNotFound", err)
	}
	if err := s.DeleteRow(ctx, "users", "missing"); err != nil {
		t.Fatalf("DeleteRow 缺行 = %v, want nil", err)
	}
	if err := s.DeleteRow(ctx, "missing", "a"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("DeleteRow 缺表 = %v, want ErrTableNotFound", err)
	}

	// 重命名：保留数据、目标冲突报 ErrTableExists、缺源报 ErrTableNotFound。
	if err := s.CreateTable(ctx, "taken"); err != nil {
		t.Fatalf("CreateTable taken: %v", err)
	}
	if err := s.RenameTable(ctx, "users", "taken"); !errors.Is(err, bot.ErrTableExists) {
		t.Fatalf("RenameTable 目标已存在 = %v, want ErrTableExists", err)
	}
	if err := s.RenameTable(ctx, "missing", "other"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("RenameTable 缺源 = %v, want ErrTableNotFound", err)
	}
	if err := s.RenameTable(ctx, "users", "users"); err != nil {
		t.Fatalf("RenameTable 同名 = %v, want nil", err)
	}
	if err := s.RenameTable(ctx, "users", "people"); err != nil {
		t.Fatalf("RenameTable: %v", err)
	}
	if _, err := s.GetRow(ctx, "users", "a"); !errors.Is(err, bot.ErrTableNotFound) {
		t.Fatalf("重命名后旧表 GetRow = %v, want ErrTableNotFound", err)
	}
	if got, err := s.GetRow(ctx, "people", "a"); err != nil || string(got) != "a2" {
		t.Fatalf("重命名后新表 GetRow = %q, %v, want a2", got, err)
	}

	names, err = s.ListTables(ctx)
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if len(names) != 2 || names[0] != "people" || names[1] != "taken" {
		t.Fatalf("ListTables = %v, want [people taken]", names)
	}

	// 删除表连带删行；缺表幂等；重建后是空表（不残留旧行）。
	if err := s.DropTable(ctx, "people"); err != nil {
		t.Fatalf("DropTable: %v", err)
	}
	if err := s.DropTable(ctx, "people"); err != nil {
		t.Fatalf("重复 DropTable = %v, want nil", err)
	}
	var orphan int64
	if err := s.db.Model(&RowEntry{}).Count(&orphan).Error; err != nil {
		t.Fatalf("统计残留行: %v", err)
	}
	if orphan != 0 {
		t.Fatalf("DropTable 后残留 %d 行, want 0", orphan)
	}
	if err := s.CreateTable(ctx, "people"); err != nil {
		t.Fatalf("重建同名表: %v", err)
	}
	if rows, err := s.ListRows(ctx, "people"); err != nil || len(rows) != 0 {
		t.Fatalf("重建后 ListRows = %v, %v, want 空", rows, err)
	}
}

// TestSQLStoreTableContext 断言取消的 ctx 不会写入数据。
func TestSQLStoreTableContext(t *testing.T) {
	s := newTableStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.CreateTable(ctx, "t"); err == nil {
		t.Fatal("CreateTable(已取消 ctx) = nil, want 错误")
	}
}
