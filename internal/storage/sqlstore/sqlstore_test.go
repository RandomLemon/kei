package sqlstore

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/RandomLemon/kei/pkg/bot"
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

	if _, err := New(db, 0, nil); err == nil {
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

// logCapture 是并发安全（后台 janitor 与测试 goroutine 并发写）的 slog.Handler，
// 把日志记录收集到切片供断言。
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

// Enabled 实现 slog.Handler（收集所有级别，含 Debug）。
func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

// Handle 实现 slog.Handler。
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.records = append(c.records, r.Clone())
	c.mu.Unlock()
	return nil
}

// WithAttrs 实现 slog.Handler。
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }

// WithGroup 实现 slog.Handler。
func (c *logCapture) WithGroup(string) slog.Handler { return c }

// find 返回第一条级别匹配且消息含 substr 的记录（副本），未找到返回 nil。
func (c *logCapture) find(level slog.Level, substr string) *slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.records {
		if c.records[i].Level == level && strings.Contains(c.records[i].Message, substr) {
			r := c.records[i]
			return &r
		}
	}
	return nil
}

// recordAttr 从记录中按键取属性值。
func recordAttr(r slog.Record, key string) (slog.Value, bool) {
	var (
		value slog.Value
		found bool
	)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			value, found = a.Value, true
			return false
		}
		return true
	})
	return value, found
}

// precreateStore 在 path 上建表（可写连接），随后释放连接池，供只读用例复用。
func precreateStore(t *testing.T, path string) {
	t.Helper()
	db := openGorm(t, path)
	st, err := NewWithClock(db, 0, time.Now, nil)
	if err != nil {
		t.Fatalf("预建表 NewWithClock: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("预建表 Close: %v", err)
	}
}

// TestGetLazyDeleteFailureLogged 断言只读库上惰性删除过期行失败时不再静默：
// Get 仍返回 bot.ErrNotFound（行为不变），同时记一条含键与错误的 Debug 日志。
func TestGetLazyDeleteFailureLogged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")
	ctx := context.Background()
	now := time.Now()

	// 可写连接写入一行短期键（过期时刻 = now+1min）。
	wdb := openGorm(t, path)
	wst, err := NewWithClock(wdb, 0, func() time.Time { return now }, nil)
	if err != nil {
		t.Fatalf("NewWithClock: %v", err)
	}
	if err := wst.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := wst.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 只读打开同一文件，把时钟拨到过期之后：Get 读到过期行，删除必然失败。
	rdb := openGorm(t, "file:"+path+"?mode=ro")
	capture := &logCapture{}
	st, err := NewWithClock(rdb, 0, func() time.Time { return now.Add(2 * time.Minute) },
		slog.New(capture))
	if err != nil {
		t.Fatalf("只读 NewWithClock: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.Get(ctx, "k"); !errors.Is(err, bot.ErrNotFound) {
		t.Fatalf("只读库上过期键 Get = %v, want bot.ErrNotFound", err)
	}
	rec := capture.find(slog.LevelDebug, "惰性清理")
	if rec == nil {
		t.Fatal("未捕获到惰性删除失败的 Debug 日志")
	}
	if v, ok := recordAttr(*rec, "key"); !ok || v.String() != "k" {
		t.Fatalf("Debug 日志的 key = %v (ok=%v), want k", v, ok)
	}
	if v, ok := recordAttr(*rec, "error"); !ok || v.String() == "" {
		t.Fatalf("Debug 日志缺少非空 error: %v (ok=%v)", v, ok)
	}
}

// TestJanitorFailureLogged 断言只读库上后台清理失败记 Warn：表在无界增长必须有信号。
func TestJanitorFailureLogged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")
	precreateStore(t, path)

	rdb := openGorm(t, "file:"+path+"?mode=ro")
	capture := &logCapture{}
	st, err := NewWithClock(rdb, 10*time.Millisecond, time.Now, slog.New(capture))
	if err != nil {
		t.Fatalf("只读 NewWithClock: %v", err)
	}
	defer func() { _ = st.Close() }()

	deadline := time.Now().Add(2 * time.Second)
	for capture.find(slog.LevelWarn, "后台清理") == nil {
		if time.Now().After(deadline) {
			t.Fatal("2s 内未捕获到后台清理失败的 Warn 日志")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec := capture.find(slog.LevelWarn, "后台清理")
	if v, ok := recordAttr(*rec, "error"); !ok || v.String() == "" {
		t.Fatalf("Warn 日志缺少非空 error: %v (ok=%v)", v, ok)
	}
}
