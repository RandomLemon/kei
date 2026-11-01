package storage

import (
	"context"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// 键空间前缀：插件与适配器共用同一个 Store，若不隔离，同名键会静默互相覆盖。
// 前缀由框架在分发存储时注入，插件/适配器只看到自己的逻辑键。
//
// 前缀计入 storage.MaxKeyBytes：底层键（前缀 + 逻辑键）总长不能超过 191 字节，
// 因此插件/适配器可用的逻辑键上限是 191 - len(prefix)。
const (
	pluginKeyPrefix  = "plugin:"
	adapterKeyPrefix = "adapter:"
)

// PluginScoped 返回以插件名隔离的 Storage 视图：底层键统一带 "plugin:<name>:" 前缀。
func PluginScoped(name string, inner bot.Storage) bot.Storage {
	return scoped{prefix: pluginKeyPrefix + name + ":", inner: inner}
}

// AdapterScoped 返回以适配器实例名隔离的 Storage 视图：底层键统一带
// "adapter:<botID>:" 前缀，同平台多实例因此互不可见。
func AdapterScoped(botID string, inner bot.Storage) bot.Storage {
	return scoped{prefix: adapterKeyPrefix + botID + ":", inner: inner}
}

// scoped 给底层 Storage 的所有键加固定前缀，读/写/删使用同一前缀。
type scoped struct {
	prefix string
	inner  bot.Storage
}

// 确保 scoped 满足 bot.Storage。
var _ bot.Storage = scoped{}

// PluginTables 返回以插件名隔离的表视图：底层表名统一带 "plugin:<name>:" 前缀。
//
// inner 未实现 bot.TableStorage 时返回 nil：表能力是可选的，未实现的后端不提供
// 表 API，而不是给一个用不了的实现。
func PluginTables(name string, inner bot.Storage) bot.TableStorage {
	ts, ok := inner.(bot.TableStorage)
	if !ok {
		return nil
	}
	return tableScoped{prefix: pluginKeyPrefix + name + ":", inner: ts}
}

// AdapterTables 返回以适配器实例名隔离的表视图：底层表名统一带
// "adapter:<botID>:" 前缀，同平台多实例因此互不可见。
func AdapterTables(botID string, inner bot.Storage) bot.TableStorage {
	ts, ok := inner.(bot.TableStorage)
	if !ok {
		return nil
	}
	return tableScoped{prefix: adapterKeyPrefix + botID + ":", inner: ts}
}

// tableScoped 只给表名加固定前缀；行键原样传递（行已被表名限定在命名空间内）。
//
// 逻辑表名在加前缀前先经 CheckTableName，行键经 CheckRowKey：前缀本身计入
// 191 字节上限，因此超长逻辑名在插件侧就报错，而不是等到底层后端才失败。
type tableScoped struct {
	prefix string
	inner  bot.TableStorage
}

// 确保 tableScoped 满足 bot.TableStorage。
var _ bot.TableStorage = tableScoped{}

// prefixed 校验逻辑表名并返回带命名空间的底层表名。
func (s tableScoped) prefixed(table string) (string, error) {
	if err := CheckTableName(table); err != nil {
		return "", err
	}
	return s.prefix + table, nil
}

// CreateTable 在本命名空间内创建表。
func (s tableScoped) CreateTable(ctx context.Context, table string) error {
	name, err := s.prefixed(table)
	if err != nil {
		return err
	}
	return s.inner.CreateTable(ctx, name)
}

// ListTables 只返回本命名空间内的表，并剥离前缀。
func (s tableScoped) ListTables(ctx context.Context) ([]string, error) {
	all, err := s.inner.ListTables(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(all))
	for _, name := range all {
		stripped := strings.TrimPrefix(name, s.prefix)
		if stripped == name || stripped == "" {
			// 不属于本命名空间（或只有前缀）：跳过，避免把别的插件/适配器的
			// 表泄露或伪装成本空间的空名。
			continue
		}
		names = append(names, stripped)
	}
	return names, nil
}

// RenameTable 在本命名空间内重命名表。
func (s tableScoped) RenameTable(ctx context.Context, oldName, newName string) error {
	oldPrefixed, err := s.prefixed(oldName)
	if err != nil {
		return err
	}
	newPrefixed, err := s.prefixed(newName)
	if err != nil {
		return err
	}
	return s.inner.RenameTable(ctx, oldPrefixed, newPrefixed)
}

// DropTable 删除本命名空间内的表。
func (s tableScoped) DropTable(ctx context.Context, table string) error {
	name, err := s.prefixed(table)
	if err != nil {
		return err
	}
	return s.inner.DropTable(ctx, name)
}

// InsertRow 在本命名空间内的表中插入行。
func (s tableScoped) InsertRow(ctx context.Context, table, key string, value []byte) error {
	name, err := s.prefixed(table)
	if err != nil {
		return err
	}
	if err := CheckRowKey(key); err != nil {
		return err
	}
	return s.inner.InsertRow(ctx, name, key, value)
}

// GetRow 读取本命名空间内的表的行。
func (s tableScoped) GetRow(ctx context.Context, table, key string) ([]byte, error) {
	name, err := s.prefixed(table)
	if err != nil {
		return nil, err
	}
	if err := CheckRowKey(key); err != nil {
		return nil, err
	}
	return s.inner.GetRow(ctx, name, key)
}

// UpdateRow 覆盖本命名空间内的表的行。
func (s tableScoped) UpdateRow(ctx context.Context, table, key string, value []byte) error {
	name, err := s.prefixed(table)
	if err != nil {
		return err
	}
	if err := CheckRowKey(key); err != nil {
		return err
	}
	return s.inner.UpdateRow(ctx, name, key, value)
}

// DeleteRow 删除本命名空间内的表的行。
func (s tableScoped) DeleteRow(ctx context.Context, table, key string) error {
	name, err := s.prefixed(table)
	if err != nil {
		return err
	}
	if err := CheckRowKey(key); err != nil {
		return err
	}
	return s.inner.DeleteRow(ctx, name, key)
}

// ListRows 返回本命名空间内的表的全部行。
func (s tableScoped) ListRows(ctx context.Context, table string) ([]bot.TableRow, error) {
	name, err := s.prefixed(table)
	if err != nil {
		return nil, err
	}
	return s.inner.ListRows(ctx, name)
}

// Get 读取本命名空间内的键值。
func (s scoped) Get(ctx context.Context, key string) ([]byte, error) {
	return s.inner.Get(ctx, s.prefix+key)
}

// Set 写入本命名空间内的键值。
func (s scoped) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return s.inner.Set(ctx, s.prefix+key, value, ttl)
}

// Delete 删除本命名空间内的键值。
func (s scoped) Delete(ctx context.Context, key string) error {
	return s.inner.Delete(ctx, s.prefix+key)
}
