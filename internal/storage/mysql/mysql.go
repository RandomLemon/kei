// Package mysql 把基于 GORM 的 MySQL 实现注册为存储类型 "mysql"。
//
// dsn 形如 "user:pass@tcp(host:3306)/db?parseTime=true"；parseTime=true 是
// expires_at 能扫描为 time.Time 的前提（核心不校验，缺失时由驱动报错）。
package mysql

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/internal/storage/sqlstore"
	"github.com/RandomLemon/kei/pkg/bot"
)

func init() {
	storage.Register("mysql", open)
}

// open 依据配置构造 MySQL 存储。
func open(cfg *bot.Config) (bot.Storage, error) {
	dsn := cfg.String("dsn", "")
	if dsn == "" {
		return nil, errors.New("storage: mysql: 缺少 dsn")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("storage: mysql: 打开数据库: %w", err)
	}
	if err := sqlstore.ApplyPool(db, cfg); err != nil {
		return nil, fmt.Errorf("storage: mysql: %w", err)
	}
	return sqlstore.New(db, cfg.Duration("cleanup_interval", time.Minute))
}
