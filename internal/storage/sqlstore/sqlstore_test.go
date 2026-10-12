package sqlstore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/RandomLemon/kei/internal/storage/sqlstore"
)

// openGorm 打开一个指向 dsn 的 GORM 连接，失败即终止测试。
func openGorm(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("gorm.Open(%s): %v", dsn, err)
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

	if _, err := sqlstore.New(db, 0); err == nil {
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

	if err := sqlstore.CloseDB(db); err != nil {
		t.Fatalf("CloseDB: %v", err)
	}
	if !poolClosed(t, db) {
		t.Fatal("CloseDB 后连接池仍可用")
	}
	if err := sqlstore.CloseDB(db); err != nil {
		t.Fatalf("重复 CloseDB 应幂等: %v", err)
	}
}
