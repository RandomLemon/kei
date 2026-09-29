// Package sqlstore 是 bot.Storage 的通用 SQL 实现，供 SQLite 与 MySQL 方言包共用。
//
// 键值存储在一张固定表 kei_storage 中：key 为主键，value 为字节串，
// expires_at 为空表示永不过期。Get/Set/Delete 语义与内存实现一致。
package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Entry 是键值存储的表模型。
type Entry struct {
	// Key 是主键；size:191 为 MySQL utf8mb4 索引键长上限内的安全值。
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
		return nil, fmt.Errorf("sqlstore: 建表: %w", err)
	}
	s := &Store{db: db, now: now, cleanup: cleanup}
	if cleanup > 0 {
		s.ctx, s.cancel = context.WithCancel(context.Background())
		s.wg.Add(1)
		go s.janitor()
	}
	return s, nil
}

// Get 读取键值；键不存在或已过期时返回 bot.ErrNotFound。
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	var e Entry
	err := s.db.WithContext(ctx).Where("key = ?", key).First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, bot.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if e.ExpiresAt != nil && !e.ExpiresAt.After(s.now()) {
		// 惰性清理：过期行 best-effort 删除，忽略错误。
		_ = s.db.WithContext(ctx).Where("key = ?", key).Delete(&Entry{}).Error
		return nil, bot.ErrNotFound
	}
	return append([]byte(nil), e.Value...), nil
}

// Set 写入键值，ttl <= 0 表示永不过期；已存在的键被覆盖。
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
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
	return s.db.WithContext(ctx).Where("key = ?", key).Delete(&Entry{}).Error
}

// Close 停止后台清理并关闭底层数据库，可重复调用。
func (s *Store) Close() error {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
	})
	if s.db == nil {
		return nil
	}
	sqlDB, err := s.db.DB()
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
