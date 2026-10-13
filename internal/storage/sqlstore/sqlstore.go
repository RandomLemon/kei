// Package sqlstore 是 bot.Storage 的通用 SQL 实现，供 SQLite 与 MySQL 方言包共用。
//
// 键值存储在一张固定表 kei_storage 中：key 为主键，value 为字节串，
// expires_at 为空表示永不过期。Get/Set/Delete 语义与内存实现一致，键长校验
// 统一经 storage.CheckKey（见 internal/storage/key.go）。
package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

// Entry 是键值存储的表模型。
type Entry struct {
	// Key 是主键；size:191 为 MySQL utf8mb4 索引键长上限内的安全值，读写前由
	// storage.CheckKey 按同一上限（字节）校验，避免只有 MySQL 会拒绝超长键。
	Key string `gorm:"column:key;primaryKey;size:191"`
	// Value 是存储的字节串。
	Value []byte `gorm:"column:value;not null"`
	// ExpiresAt 是过期时间，nil 表示永不过期。
	ExpiresAt *time.Time `gorm:"column:expires_at;index"`
}

// TableName 固定表名，MySQL 与 SQLite 通用。
func (Entry) TableName() string { return "kei_storage" }

// Store 是 bot.Storage 的 SQL 实现，并发安全。
type Store struct {
	db      *gorm.DB
	now     func() time.Time
	cleanup time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	stopOnce sync.Once
	wg       sync.WaitGroup
}

// 确保 Store 满足 bot.Storage。
var _ bot.Storage = (*Store)(nil)

// New 在 db 上 AutoMigrate 并构造 Store；cleanup <= 0 时不启动后台清理。
//
// New 取得 db 的所有权：成功时由 Store.Close 关闭底层连接池，失败（AutoMigrate
// 报错）时由本函数关闭，调用方不需要（也不应）再关闭同一个 db。
func New(db *gorm.DB, cleanup time.Duration) (*Store, error) {
	return NewWithClock(db, cleanup, time.Now)
}

// NewWithClock 同 New，但注入时钟（测试用，避免 sleep）。
func NewWithClock(db *gorm.DB, cleanup time.Duration, now func() time.Time) (*Store, error) {
	if db == nil {
		return nil, errors.New("sqlstore: db 为 nil")
	}
	if now == nil {
		now = time.Now
	}
	if err := db.AutoMigrate(&Entry{}); err != nil {
		// 失败路径必须释放连接池（SQLite 还会占住文件句柄）：调用方拿到的是
		// error 而非 Store，没有任何 closer，不关闭就是永久泄漏。
		return nil, errors.Join(fmt.Errorf("sqlstore: 建表: %w", err), CloseDB(db))
	}
	s := &Store{db: db, now: now, cleanup: cleanup}
	if cleanup > 0 {
		s.ctx, s.cancel = context.WithCancel(context.Background())
		s.wg.Add(1)
		go s.janitor()
	}
	return s, nil
}

// whereKey 生成按主键 key 过滤的条件。key 是 MySQL 8 保留字：字符串条件
// （`Where("key = ?", key)`）由 GORM 原样下发，MySQL 会报 Error 1064 语法错误
// （SQLite 宽容，问题只在 MySQL 暴露）。用 clause.Eq 把列名交给 dialector 引用，
// 与 Set 的 OnConflict 保持同一约定。
func whereKey(key string) clause.Eq {
	return clause.Eq{Column: clause.Column{Name: "key"}, Value: key}
}

// Get 读取键值；键不存在或已过期时返回 bot.ErrNotFound。
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	if err := storage.CheckKey(key); err != nil {
		return nil, err
	}
	var e Entry
	err := s.db.WithContext(ctx).Where(whereKey(key)).First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, bot.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if e.ExpiresAt != nil && !e.ExpiresAt.After(s.now()) {
		// 惰性清理：过期行 best-effort 删除，忽略错误。
		_ = s.db.WithContext(ctx).Where(whereKey(key)).Delete(&Entry{}).Error
		return nil, bot.ErrNotFound
	}
	return append([]byte(nil), e.Value...), nil
}

// Set 写入键值，ttl <= 0 表示永不过期；已存在的键被覆盖。
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := storage.CheckKey(key); err != nil {
		return err
	}
	e := Entry{Key: key, Value: append([]byte(nil), value...)}
	if ttl > 0 {
		t := s.now().Add(ttl)
		e.ExpiresAt = &t
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "expires_at"}),
	}).Create(&e).Error
}

// Delete 删除键值，键不存在时不返回错误。
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := storage.CheckKey(key); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Where(whereKey(key)).Delete(&Entry{}).Error
}

// Close 停止后台清理并关闭底层数据库，可重复调用；db 的所有权语义见 New。
func (s *Store) Close() error {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
	})
	return CloseDB(s.db)
}

// CloseDB 关闭 db 的底层连接池，释放连接与文件句柄；db 为 nil 时返回 nil，
// 对已关闭的连接池幂等。
//
// 供 New 的失败路径与方言包（如 mysql 应用连接池参数失败时）统一释放资源。
func CloseDB(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (s *Store) janitor() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cleanup)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			_ = s.db.WithContext(s.ctx).
				Where("expires_at IS NOT NULL AND expires_at <= ?", s.now()).
				Delete(&Entry{}).Error
		}
	}
}

// ApplyPool 按配置里的 max_open_conns / max_idle_conns / conn_max_lifetime 调整连接池；
//
// 三个键都缺失时不改动默认值。
func ApplyPool(db *gorm.DB, cfg *bot.Config) error {
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("sqlstore: 获取连接池: %w", err)
	}
	if _, ok := cfg.Get("max_open_conns"); ok {
		sqlDB.SetMaxOpenConns(cfg.Int("max_open_conns", 0))
	}
	if _, ok := cfg.Get("max_idle_conns"); ok {
		sqlDB.SetMaxIdleConns(cfg.Int("max_idle_conns", 0))
	}
	if _, ok := cfg.Get("conn_max_lifetime"); ok {
		sqlDB.SetConnMaxLifetime(cfg.Duration("conn_max_lifetime", 0))
	}
	return nil
}
