package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound 表示 Storage 中不存在所请求的键。
//
// 实现必须保证「键不存在」返回可被 errors.Is(err, ErrNotFound) 识别的错误。
var ErrNotFound = errors.New("bot: storage: key not found")

// Storage 是插件可用的键值存储。
//
// MVP 使用内存实现，后续可替换为 Redis/SQLite；实现必须并发安全。
type Storage interface {
	// Get 读取键值，键不存在时返回 ErrNotFound。
	Get(ctx context.Context, key string) ([]byte, error)
	// Set 写入键值；ttl <= 0 表示永不过期。
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete 删除键值，键不存在时不返回错误。
	Delete(ctx context.Context, key string) error
}

// BotAPI 是插件与平台交互的唯一入口。
//
// 插件不得直接依赖任何平台 SDK；主动发送与回复都必须经过该接口，
// 以便引擎统一执行权限校验、能力降级与限流。
type BotAPI interface {
	// Send 向目标会话发送消息。
	Send(ctx context.Context, target Target, msg *Message) (*SendResult, error)
	// Reply 回复某个事件所在的会话。
	Reply(ctx context.Context, ev *Event, msg *Message) (*SendResult, error)
	// Logger 返回带插件字段的日志器。
	Logger() *slog.Logger
	// Storage 返回插件可用的键值存储。
	Storage() Storage
}

// PluginCatalog 提供已加载插件的元信息，供管理类插件使用。
type PluginCatalog interface {
	// Plugins 返回当前已加载插件的元信息快照。
	Plugins() []Metadata
}

// PluginContext 是插件运行期依赖的聚合，由引擎在 Setup/Start 阶段注入。
//
// 通过 PluginContextFrom(ctx) 在插件内部获取。
type PluginContext struct {
	// Name 是插件自身名称，等于 Metadata().Name。
	Name string
	// Config 是该插件的独立配置，未配置时为空配置（方法仍然安全）。
	Config *Config
	// Logger 是已绑定 plugin 字段的日志器。
	Logger *slog.Logger
	// Storage 是插件可用的键值存储，插件需声明 PermStorage 才能使用。
	Storage Storage
	// HTTPClient 是带超时的 HTTP 客户端，插件需声明 PermNetwork 才能使用。
	HTTPClient *http.Client
	// Bot 是插件访问平台的唯一入口。
	Bot BotAPI
	// Catalog 提供插件列表，可能为 nil。
	Catalog PluginCatalog
	// Adapters 提供已加载适配器列表，可能为 nil。
	Adapters AdapterCatalog
}

// Config 是插件的独立配置，来自 configs 中该插件名下的键值。
//
// 支持 "a.b.c" 形式的多级键；所有取值方法对 nil 接收者与缺失键都返回默认值。
type Config struct {
	raw map[string]any
}

// NewConfig 从原始键值构建插件配置，raw 可为 nil。
func NewConfig(raw map[string]any) *Config {
	return &Config{raw: raw}
}

// Raw 返回原始配置映射，调用方不得修改返回值。
func (c *Config) Raw() map[string]any {
	if c == nil {
		return nil
	}
	return c.raw
}

// Get 按路径读取原始值，路径以 "." 分隔。
func (c *Config) Get(key string) (any, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	var cur any = c.raw
	for part := range strings.SplitSeq(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// String 读取字符串配置，键缺失或类型无法转换时返回 def。
func (c *Config) String(key, def string) string {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

// Bool 读取布尔配置，键缺失或无法解析时返回 def。
func (c *Config) Bool(key string, def bool) bool {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		if b, err := strconv.ParseBool(t); err == nil {
			return b
		}
	}
	return def
}

// Int 读取整数配置，键缺失或无法解析时返回 def。
func (c *Config) Int(key string, def int) int {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return def
}

// Duration 读取时长配置。字符串按 time.ParseDuration 解析，数字按秒处理。
func (c *Config) Duration(key string, def time.Duration) time.Duration {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case time.Duration:
		return t
	case int:
		return time.Duration(t) * time.Second
	case int64:
		return time.Duration(t) * time.Second
	case float64:
		return time.Duration(t * float64(time.Second))
	case string:
		if d, err := time.ParseDuration(strings.TrimSpace(t)); err == nil {
			return d
		}
	}
	return def
}

// Strings 读取字符串列表配置，键缺失时返回 nil。
func (c *Config) Strings(key string) []string {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// Unmarshal 把配置解码到 v，v 通常是指向结构体的指针。
//
// 解码依赖 encoding/json 的标签与类型转换规则。
func (c *Config) Unmarshal(v any) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(c.raw)
	if err != nil {
		return fmt.Errorf("bot: encode plugin config: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("bot: decode plugin config: %w", err)
	}
	return nil
}
