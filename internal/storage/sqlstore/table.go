package sqlstore

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

// TableEntry 是表元数据模型：一行一张表，ID 由自增主键生成。
//
// 单独一张元数据表而不是用 SQL 的真实表：真实表需要按名字拼 DDL，既无法在
// 方言间统一，也会让插件提供的表名进入 DDL。这里所有表共用 kei_storage_rows，
// 由 table_id 区分，表名只作为数据出现。
type TableEntry struct {
	// ID 是内部标识；行表的 table_id 引用它。
	ID uint `gorm:"column:id;primaryKey;autoIncrement"`
	// Name 是逻辑表名；uniqueIndex 保证 CreateTable 的冲突能被后端发现。
	Name string `gorm:"column:name;uniqueIndex;size:191;not null"`
}

// TableName 固定表名。
func (TableEntry) TableName() string { return "kei_storage_tables" }

// RowEntry 是表内的一行：主键为 (table_id, row_key)。
//
// 复合主键让「同一表内行键唯一」由数据库保证，也让按 table_id 前缀的查询
// （ListRows、DropTable 的级联删除）走同一个索引。
type RowEntry struct {
	// TableID 是所属表；autoIncrement:false 表示它来自 TableEntry.ID。
	TableID uint `gorm:"column:table_id;primaryKey;autoIncrement:false"`
	// Key 是行键；列名避开 SQL 保留字 key。
	Key string `gorm:"column:row_key;primaryKey;size:191;not null"`
	// Value 是行值，nil 归一为空 BLOB（列 NOT NULL）。
	Value []byte `gorm:"column:value;not null"`
}

// TableName 固定表名。
func (RowEntry) TableName() string { return "kei_storage_rows" }

// 确保 Store 同时满足 bot.TableStorage。
var _ bot.TableStorage = (*Store)(nil)

// CreateTable 创建表；同名表已存在时返回 bot.ErrTableExists。
func (s *Store) CreateTable(ctx context.Context, table string) error {
	if err := storage.CheckTableName(table); err != nil {
		return err
	}
	err := s.db.WithContext(ctx).Create(&TableEntry{Name: table}).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return fmt.Errorf("%w: %q", bot.ErrTableExists, table)
	}
	return err
}

// ListTables 按名称升序返回全部表名；无表时返回空切片。
func (s *Store) ListTables(ctx context.Context) ([]string, error) {
	var names []string
	if err := s.db.WithContext(ctx).Model(&TableEntry{}).Order("name").Pluck("name", &names).Error; err != nil {
		return nil, err
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}

// RenameTable 重命名表并保留行数据。
func (s *Store) RenameTable(ctx context.Context, oldName, newName string) error {
	if err := storage.CheckTableName(oldName); err != nil {
		return err
	}
	if err := storage.CheckTableName(newName); err != nil {
		return err
	}
	id, err := s.tableID(ctx, oldName)
	if err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	res := s.db.WithContext(ctx).Model(&TableEntry{}).Where("id = ?", id).Update("name", newName)
	if errors.Is(res.Error, gorm.ErrDuplicatedKey) {
		return fmt.Errorf("%w: %q", bot.ErrTableExists, newName)
	}
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 查到之后被并发删除：报告源表不存在，与先查后改的语义一致。
		return fmt.Errorf("%w: %q", bot.ErrTableNotFound, oldName)
	}
	return nil
}

// DropTable 删除表及其全部行；表不存在时返回 nil。
//
// 元数据与行必须一起删除：先删行再删元数据，任何一步失败都回滚，避免留下
// 无法经表名访问的孤儿行。
func (s *Store) DropTable(ctx context.Context, table string) error {
	if err := storage.CheckTableName(table); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var t TableEntry
		err := tx.Where("name = ?", table).First(&t).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.Where("table_id = ?", t.ID).Delete(&RowEntry{}).Error; err != nil {
			return err
		}
		return tx.Delete(&TableEntry{}, t.ID).Error
	})
}

// InsertRow 插入一行；表不存在返回 bot.ErrTableNotFound，行键已存在返回
// bot.ErrRowExists。
func (s *Store) InsertRow(ctx context.Context, table, key string, value []byte) error {
	if err := storage.CheckTableName(table); err != nil {
		return err
	}
	if err := storage.CheckRowKey(key); err != nil {
		return err
	}
	// 一条语句完成「表存在性检查 + 插入」：先查表再插入会在两步之间留下竞态
	// （并发 DropTable 后插进一张没有元数据的表，变成孤儿行）。表不存在时
	// SELECT 不产生任何行，RowsAffected 为 0。
	res := s.db.WithContext(ctx).Exec(
		"INSERT INTO `kei_storage_rows` (`table_id`, `row_key`, `value`) "+
			"SELECT `id`, ?, ? FROM `kei_storage_tables` WHERE `name` = ?",
		key, normalizeValue(value), table)
	if errors.Is(res.Error, gorm.ErrDuplicatedKey) {
		return fmt.Errorf("%w: %q", bot.ErrRowExists, key)
	}
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %q", bot.ErrTableNotFound, table)
	}
	return nil
}

// GetRow 读取一行；表不存在返回 bot.ErrTableNotFound，行不存在返回
// bot.ErrNotFound。返回值是副本。
func (s *Store) GetRow(ctx context.Context, table, key string) ([]byte, error) {
	if err := storage.CheckTableName(table); err != nil {
		return nil, err
	}
	if err := storage.CheckRowKey(key); err != nil {
		return nil, err
	}
	id, err := s.tableID(ctx, table)
	if err != nil {
		return nil, err
	}
	var r RowEntry
	err = s.db.WithContext(ctx).Where("table_id = ? AND row_key = ?", id, key).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, bot.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), r.Value...), nil
}

// UpdateRow 覆盖一行；表不存在返回 bot.ErrTableNotFound，行不存在返回
// bot.ErrNotFound。
func (s *Store) UpdateRow(ctx context.Context, table, key string, value []byte) error {
	if err := storage.CheckTableName(table); err != nil {
		return err
	}
	if err := storage.CheckRowKey(key); err != nil {
		return err
	}
	id, err := s.tableID(ctx, table)
	if err != nil {
		return err
	}
	where := "table_id = ? AND row_key = ?"
	res := s.db.WithContext(ctx).Model(&RowEntry{}).Where(where, id, key).
		Update("value", normalizeValue(value))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	// RowsAffected == 0 有两种原因：行不存在，或新值与旧值相同（MySQL 默认
	// 只统计「真正改变」的行）。不能据此判缺失，必须再查一次行的存在性。
	var count int64
	if err := s.db.WithContext(ctx).Model(&RowEntry{}).Where(where, id, key).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return bot.ErrNotFound
	}
	return nil
}

// DeleteRow 删除一行；表不存在返回 bot.ErrTableNotFound，行不存在返回 nil。
func (s *Store) DeleteRow(ctx context.Context, table, key string) error {
	if err := storage.CheckTableName(table); err != nil {
		return err
	}
	if err := storage.CheckRowKey(key); err != nil {
		return err
	}
	id, err := s.tableID(ctx, table)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).
		Where("table_id = ? AND row_key = ?", id, key).
		Delete(&RowEntry{}).Error
}

// ListRows 按行键升序返回表内全部行；表不存在返回 bot.ErrTableNotFound，
// 空表返回空切片。返回值是副本。
func (s *Store) ListRows(ctx context.Context, table string) ([]bot.TableRow, error) {
	if err := storage.CheckTableName(table); err != nil {
		return nil, err
	}
	id, err := s.tableID(ctx, table)
	if err != nil {
		return nil, err
	}
	var entries []RowEntry
	if err := s.db.WithContext(ctx).Where("table_id = ?", id).
		Order("row_key").Find(&entries).Error; err != nil {
		return nil, err
	}
	out := make([]bot.TableRow, 0, len(entries))
	for _, e := range entries {
		out = append(out, bot.TableRow{Key: e.Key, Value: append([]byte(nil), e.Value...)})
	}
	return out, nil
}

// tableID 按表名取内部 ID；表不存在时返回包装 bot.ErrTableNotFound 的错误。
func (s *Store) tableID(ctx context.Context, name string) (uint, error) {
	var t TableEntry
	err := s.db.WithContext(ctx).Where("name = ?", name).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, fmt.Errorf("%w: %q", bot.ErrTableNotFound, name)
	}
	if err != nil {
		return 0, err
	}
	return t.ID, nil
}

// normalizeValue 把 nil 归一为空 BLOB：value 列是 NOT NULL，nil []byte 会被
// 驱动绑定为 SQL NULL 而失败。用 make 而不是 append(nil, value...)，因为后者对
// 空切片仍返回 nil。
func normalizeValue(value []byte) []byte {
	out := make([]byte, len(value))
	copy(out, value)
	return out
}
