package storage

import (
	"context"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// 键空间前缀：插件与适配器共用同一个 Store，若不隔离，同名键会静默互相覆盖。
// 前缀由框架在分发存储时注入，插件/适配器只看到自己的逻辑键。
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
