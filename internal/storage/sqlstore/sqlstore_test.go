package sqlstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openGorm 打开一个指向 dsn 的 SQLite GORM 连接，失败即终止测试。
func openGorm(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("gorm.Open(%s): %v", dsn, err)
	}
	return db
}

// openMySQLDialect 构造 MySQL 方言的 dry-run 连接：只生成 SQL，不连数据库。
func openMySQLDialect(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		mysql.New(mysql.Config{DSN: "user:pass@tcp(127.0.0.1:3306)/db", SkipInitializeWithVersion: true}),
		&gorm.Config{DryRun: true, DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatalf("gorm.Open(mysql): %v", err)
	}
	return db
}

// poolClosed 判断 db 的底层连接池是否已关闭。
func poolClosed(t *testing.T, db *gorm.DB) bool {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	return sqlDB.PingContext(context.Background()) != nil && sqlDB.Stats().OpenConnections == 0
}

// TestNewClosesPoolOnMigrateFailure 断言 New 在 AutoMigrate 失败时关闭连接池：
// 只读库（无写权限）上每次构造都会新建 *sql.DB（SQLite 还占住文件句柄），
// 失败路径不关闭就永久泄漏，调用方拿不到任何 closer。
func TestNewClosesPoolOnMigrateFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readonly.db")
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	db := openGorm(t, "file:"+path+"?mode=ro")

	if _, err := New(db, 0); err == nil {
		t.Fatal("只读库上的 AutoMigrate 应报错")
	} else {
		t.Logf("AutoMigrate 失败（预期）: %v", err)
	}

	if !poolClosed(t, db) {
		t.Fatal("构造失败后连接池未关闭（Ping 成功 / 仍有打开连接）")
	}
}

// TestCloseDBClosesPool 断言 CloseDB 真正关闭连接池且可重复调用，
// 该 helper 是方言包（mysql 应用连接池参数失败时）释放资源共用的路径。
func TestCloseDBClosesPool(t *testing.T) {
	db := openGorm(t, filepath.Join(t.TempDir(), "k.db"))

	if err := CloseDB(db); err != nil {
		t.Fatalf("CloseDB: %v", err)
	}
	if !poolClosed(t, db) {
		t.Fatal("CloseDB 后连接池仍可用")
	}
	if err := CloseDB(db); err != nil {
		t.Fatalf("重复 CloseDB 应幂等: %v", err)
	}
}

// TestKeyColumnQuotedForMySQL 断言按主键 key 过滤的语句由 dialector 引用列名
// （“ `key` “）。key 是 MySQL 8 保留字：裸字符串条件（Where("key = ?", …)）
// 在 MySQL 上直接报 Error 1064 语法错误，SQLite 宽容导致缺陷只在 MySQL 后端
// 暴露；这里用 MySQL 方言的 dry-run 生成 SQL，无需真实服务器。
func TestKeyColumnQuotedForMySQL(t *testing.T) {
	db := openMySQLDialect(t)

	get := db.ToSQL(func(tx *gorm.DB) *gorm.DB { return tx.Where(whereKey("k1")).First(&Entry{}) })
	del := db.ToSQL(func(tx *gorm.DB) *gorm.DB { return tx.Where(whereKey("k1")).Delete(&Entry{}) })
	t.Logf("SELECT: %s", get)
	t.Logf("DELETE: %s", del)

	for _, sql := range []string{get, del} {
		if !strings.Contains(sql, "WHERE `key` = 'k1'") {
			t.Fatalf("key 列未被 dialector 引用: %s", sql)
		}
	}
}
