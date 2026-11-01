package storage

import (
	"context"
	"fmt"
	"sort"

	"github.com/RandomLemon/kei/pkg/bot"
)

// 确保 Memory 同时满足 bot.TableStorage。
var _ bot.TableStorage = (*Memory)(nil)

// CreateTable 创建空表；同名表已存在时返回 bot.ErrTableExists。
func (m *Memory) CreateTable(ctx context.Context, table string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckTableName(table); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tables[table]; ok {
		return fmt.Errorf("%w: %q", bot.ErrTableExists, table)
	}
	m.tables[table] = make(map[string][]byte)
	return nil
}

// ListTables 按名称升序返回全部表名；无表时返回空切片。
func (m *Memory) ListTables(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	names := make([]string, 0, len(m.tables))
	for name := range m.tables {
		names = append(names, name)
	}
	m.mu.RUnlock()
	sort.Strings(names)
	return names, nil
}

// RenameTable 重命名表并保留行数据。
func (m *Memory) RenameTable(ctx context.Context, oldName, newName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckTableName(oldName); err != nil {
		return err
	}
	if err := CheckTableName(newName); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, ok := m.tables[oldName]
	if !ok {
		return fmt.Errorf("%w: %q", bot.ErrTableNotFound, oldName)
	}
	if oldName == newName {
		return nil
	}
	if _, exists := m.tables[newName]; exists {
		return fmt.Errorf("%w: %q", bot.ErrTableExists, newName)
	}
	delete(m.tables, oldName)
	m.tables[newName] = rows
	return nil
}

// DropTable 删除表及其全部行；表不存在时返回 nil。
func (m *Memory) DropTable(ctx context.Context, table string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckTableName(table); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.tables, table)
	m.mu.Unlock()
	return nil
}

// InsertRow 插入一行；表不存在返回 bot.ErrTableNotFound，行键已存在返回
// bot.ErrRowExists。入参被拷贝。
func (m *Memory) InsertRow(ctx context.Context, table, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckTableName(table); err != nil {
		return err
	}
	if err := CheckRowKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, ok := m.tables[table]
	if !ok {
		return errTableMissing(table)
	}
	if _, exists := rows[key]; exists {
		return fmt.Errorf("%w: %q", bot.ErrRowExists, key)
	}
	rows[key] = copyBytes(value)
	return nil
}

// GetRow 读取一行；表不存在返回 bot.ErrTableNotFound，行不存在返回
// bot.ErrNotFound。返回值是副本。
func (m *Memory) GetRow(ctx context.Context, table, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckTableName(table); err != nil {
		return nil, err
	}
	if err := CheckRowKey(key); err != nil {
		return nil, err
	}
	m.mu.RLock()
	rows, ok := m.tables[table]
	if !ok {
		m.mu.RUnlock()
		return nil, errTableMissing(table)
	}
	value, exists := rows[key]
	var out []byte
	if exists {
		out = copyBytes(value)
	}
	m.mu.RUnlock()
	if !exists {
		return nil, bot.ErrNotFound
	}
	return out, nil
}

// UpdateRow 覆盖一行；表不存在返回 bot.ErrTableNotFound，行不存在返回
// bot.ErrNotFound。入参被拷贝。
func (m *Memory) UpdateRow(ctx context.Context, table, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckTableName(table); err != nil {
		return err
	}
	if err := CheckRowKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, ok := m.tables[table]
	if !ok {
		return errTableMissing(table)
	}
	if _, exists := rows[key]; !exists {
		return bot.ErrNotFound
	}
	rows[key] = copyBytes(value)
	return nil
}

// DeleteRow 删除一行；表不存在返回 bot.ErrTableNotFound，行不存在返回 nil。
func (m *Memory) DeleteRow(ctx context.Context, table, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckTableName(table); err != nil {
		return err
	}
	if err := CheckRowKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, ok := m.tables[table]
	if !ok {
		return errTableMissing(table)
	}
	delete(rows, key)
	return nil
}

// ListRows 按行键升序返回表内全部行；表不存在返回 bot.ErrTableNotFound，
// 空表返回空切片。返回值是副本。
func (m *Memory) ListRows(ctx context.Context, table string) ([]bot.TableRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckTableName(table); err != nil {
		return nil, err
	}
	m.mu.RLock()
	rows, ok := m.tables[table]
	if !ok {
		m.mu.RUnlock()
		return nil, errTableMissing(table)
	}
	out := make([]bot.TableRow, 0, len(rows))
	for key, value := range rows {
		out = append(out, bot.TableRow{Key: key, Value: copyBytes(value)})
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// copyBytes 拷贝行值；空值（nil 或零长度）统一为 nil。
//
// KV 层与 SQL 后端的表读回对空值都给出 nil（SQL 用 append(nil, empty...)），
// 内存表实现若保留非 nil 空切片，同一份数据在 JSON 里会出现 "" 与 null 的
// 后端差异。写入侧因此也把空值存成 nil。
func copyBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	out := make([]byte, len(value))
	copy(out, value)
	return out
}

// errTableMissing 统一包装「表不存在」，供各实现复用同一文案。
func errTableMissing(table string) error {
	return fmt.Errorf("%w: %q", bot.ErrTableNotFound, table)
}
