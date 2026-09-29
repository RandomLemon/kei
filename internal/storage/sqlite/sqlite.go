// Package sqlite 把基于 GORM 的 SQLite 实现注册为存储类型 "sqlite"。
//
// dsn 是 SQLite 文件路径或 "file:...?params" 形式，语义原样透传给驱动。
// sqlite 驱动依赖 cgo，构建需 CGO_ENABLED=1。
package sqlite

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/internal/storage/sqlstore"
	"github.com/RandomLemon/kei/pkg/bot"
)

func init() {
	storage.Register("sqlite", open)
}

// open 依据配置构造 SQLite 存储。
func open(cfg *bot.Config) (bot.Storage, error) {
	dsn := cfg.String("dsn", "")
	if dsn == "" {
		return nil, errors.New("storage: sqlite: 缺少 dsn")
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("storage: sqlite: 打开数据库: %w", err)
	}
	return sqlstore.New(db, cfg.Duration("cleanup_interval", time.Minute))
}
