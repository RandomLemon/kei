// Package config 负责加载、合并与校验 kei 的 YAML 配置。
//
// 配置有两层来源：YAML 文件与环境变量。环境变量统一以 KEI_ 为前缀，
// 路径段之间用 "_" 连接；匹配时不区分大小写，且 "-"、"." 与 "_" 等价。
// 例如 KEI_LOG_LEVEL 覆盖 Log.Level，KEI_BOTS_TELEGRAM_MAIN_APP_ID 覆盖
// 名为 "telegram-main" 的 bot 的 Settings["app_id"]。未匹配任何已知路径的
// KEI_* 变量会被忽略（不报错）。
//
// 顶层允许的键为 log、metrics、bots、plugins、adapters、limits、auth、storage、
// engine，其余顶层键一律报错。
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 日志默认值。
const (
	defaultLogLevel  = "info"
	defaultLogFormat = "text"
)

// namePattern 限制名字类配置的字符集：适配器名、插件名与存储类型名共用。
var namePattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// validLogLevels 与 validLogFormats 是 log 段允许的取值（比较前统一转小写）。
var (
	validLogLevels  = map[string]struct{}{"debug": {}, "info": {}, "warn": {}, "error": {}}
	validLogFormats = map[string]struct{}{"text": {}, "json": {}}
)

// Config 是 kei 的完整配置。
type Config struct {
	// Log 是日志配置。
	Log LogConfig `yaml:"log"`
	// Metrics 是指标暴露配置。
	Metrics MetricsConfig `yaml:"metrics"`
	// Bots 是机器人实例列表，可为空（只运行插件时）。
	Bots []BotConfig `yaml:"bots"`
	// Plugins 是插件名到插件配置的映射。
	Plugins map[string]PluginConfig `yaml:"plugins"`
	// Adapters 是适配器名到启用开关的映射；进程内适配器无需在此声明。
	Adapters map[string]AdapterConfig `yaml:"adapters"`
	// Limits 是限流配置。
	Limits LimitsConfig `yaml:"limits"`
	// Auth 是权限配置。
	Auth AuthConfig `yaml:"auth"`
	// Storage 是存储后端配置。
	Storage StorageConfig `yaml:"storage"`
	// Engine 是引擎可调参数配置；全部字段零值表示「由引擎填默认值」。
	Engine EngineConfig `yaml:"engine"`
	// EnvOverrides 记录本次加载中实际生效的环境变量覆盖，按应用顺序排列；
	// 供启动时以 DEBUG 日志输出，排查某个键来自 YAML 还是环境变量。
	// 它不是 YAML 键，解码不会读写该字段。
	EnvOverrides []EnvOverride `yaml:"-"`
}

// EnvOverride 记录一条实际生效的环境变量覆盖。
type EnvOverride struct {
	// Key 是原始环境变量名，例如 KEI_LOG_LEVEL。
	Key string
	// Path 是被覆盖的配置项路径，例如 log.level。
	Path string
	// Value 是环境变量的原始取值。
	Value string
}

// LogConfig 是日志配置。
type LogConfig struct {
	// Level 是日志级别，默认 info。
	Level string `yaml:"level"`
	// Format 是输出格式（text 或 json），默认 text。
	Format string `yaml:"format"`
}

// MetricsConfig 是指标配置。
type MetricsConfig struct {
	// Addr 是指标监听地址，为空表示不暴露。
	Addr string `yaml:"addr"`
}

// LimitsConfig 是限流配置。速率为 0 表示不限流。
type LimitsConfig struct {
	// HandlerRate 是每用户每秒允许处理的规则次数，0 表示不限流。
	HandlerRate float64 `yaml:"handler_rate"`
	// HandlerBurst 是处理侧突发容量，<=0 时由调用方取 HandlerRate。
	HandlerBurst int `yaml:"handler_burst"`
	// SendRate 是每个 bot 每秒允许发送的条数，0 表示不限流。
	SendRate float64 `yaml:"send_rate"`
	// SendBurst 是发送侧突发容量，<=0 时由调用方取 SendRate。
	SendBurst int `yaml:"send_burst"`
}

// AuthConfig 是权限配置。
type AuthConfig struct {
	// AdminUsers 是管理员用户 ID 列表，供 Auth 中间件判定。
	AdminUsers []string `yaml:"admin_users"`
}

// StorageConfig 是存储后端配置。
type StorageConfig struct {
	// Type 是存储类型名，如 memory/sqlite/mysql，默认 memory。
	Type string `yaml:"type"`
	// DSN 是数据源，语义由各后端定义。
	DSN string `yaml:"dsn"`
	// Params 是除 type/dsn 外的其余键，原样传给后端。
	Params map[string]any
}

// EngineConfig 是引擎可调参数配置。
//
// 所有字段的零值都表示「由引擎填默认值」：`applyDefaults` 不在此替引擎填默认值，
// 取值原样透传给 internal/engine.Options，由引擎的 orInt/orDuration 回落默认
// （见 docs/engine.md 7.1）。因此未配置与显式写 0 在时长/容量类键上等价。
type EngineConfig struct {
	// CommandPrefixes 是命令前缀，默认 ["/"]；空列表表示用默认。
	CommandPrefixes []string `yaml:"command_prefixes"`
	// EventTimeout 是单条事件整体处理超时，默认 30s。
	EventTimeout time.Duration `yaml:"event_timeout"`
	// RuleTimeout 是单条规则 Handler 的执行超时，默认 10s。
	RuleTimeout time.Duration `yaml:"rule_timeout"`
	// ShutdownTimeout 是优雅关闭总超时，默认 10s。
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// BusWorkers 是事件总线分片 worker 数，默认 4。
	BusWorkers int `yaml:"bus_workers"`
	// BusQueueSize 是每个分片队列容量，默认 256。
	BusQueueSize int `yaml:"bus_queue_size"`
	// DedupTTL 是事件去重时间窗，默认 5m。
	DedupTTL time.Duration `yaml:"dedup_ttl"`
	// DedupCapacity 是去重集合容量上限，默认 4096。
	DedupCapacity int `yaml:"dedup_capacity"`
	// SendRetries 是发送失败后的重试次数（不含首次）；0 表示不重试，
	// 负数非法。注意这与时长/容量类不同：0 是有意义的取值，不是「用默认」。
	SendRetries int `yaml:"send_retries"`
	// SendBackoff 是发送重试的初始退避，默认 100ms。
	SendBackoff time.Duration `yaml:"send_backoff"`
}

// BotConfig 描述一个机器人实例。
type BotConfig struct {
	// Name 是实例名，全局唯一。
	Name string
	// Adapter 是平台适配器名，只允许 [a-z0-9_-]。
	Adapter string
	// Enabled 控制是否装配该实例；nil（未写 enabled）视为启用。
	//
	// 只有显式的 enabled: false 才跳过：该实例不建立通道、不接收事件、不参与
	// 适配器校验（记 warn 日志），因此可以临时停用某个账号而不删掉它的配置。
	// 与适配器同向：缺省启用，声明本身不是启用的前提。
	Enabled *bool
	// Settings 是 YAML 中除 name/adapter/enabled/plugins 外的其余键；
	// 取值保证可被 encoding/json 编解码。
	Settings map[string]any
	// Plugins 是该实例启用的插件名，为空表示全部启用。
	Plugins []string
}

// IsEnabled 判断该实例是否启用：未显式写 enabled 时视为启用。
func (b BotConfig) IsEnabled() bool {
	return b.Enabled == nil || *b.Enabled
}

// PluginConfig 是单个插件的配置。
type PluginConfig struct {
	// Enabled 表示插件是否启用；插件未在配置中出现时为 false。
	Enabled bool
	// Settings 是 YAML 中除 enabled 外的其余键；
	// 取值保证可被 encoding/json 编解码。
	Settings map[string]any
}

// Load 从 path 读取 YAML 配置，并应用 os.Environ() 中的环境变量覆盖。
//
// 文件读取失败与解析/校验失败返回的错误都用 %w 包装，且包含 path。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: 读取配置文件 %s: %w", path, err)
	}
	cfg, err := LoadBytes(data, os.Environ())
	if err != nil {
		return nil, fmt.Errorf("config: 配置文件 %s: %w", path, err)
	}
	return cfg, nil
}

// LoadBytes 解析 data 中的 YAML 配置，并应用 env 覆盖。
//
// env 中每个元素形如 "KEY=VALUE"（通常是 os.Environ() 的返回值），
// 不满足该格式的元素被忽略；传 nil 表示不做环境变量覆盖。
func LoadBytes(data []byte, env []string) (*Config, error) {
	cfg, err := decode(data)
	if err != nil {
		return nil, err
	}
	if err := applyEnv(cfg, env); err != nil {
		return nil, err
	}
	applyDefaults(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Plugin 返回名为 name 的插件配置；插件不存在时返回零值（Enabled 为 false）。
func (c *Config) Plugin(name string) PluginConfig {
	if c == nil {
		return PluginConfig{}
	}
	return c.Plugins[name]
}

// decode 解析 YAML 并做结构转换，不做环境变量覆盖与校验。
func decode(data []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("config: YAML 解析失败: %w", err)
	}
	cfg := &Config{Plugins: map[string]PluginConfig{}}

	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return cfg, nil
		}
		root = root.Content[0]
	}
	if root.Kind == 0 || isNull(root) {
		return cfg, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config: 顶层必须是映射，实际为 %s", root.Tag)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i].Value, root.Content[i+1]
		var err error
		switch key {
		case "log":
			err = val.Decode(&cfg.Log)
		case "metrics":
			err = val.Decode(&cfg.Metrics)
		case "limits":
			err = val.Decode(&cfg.Limits)
		case "auth":
			err = val.Decode(&cfg.Auth)
		case "storage":
			cfg.Storage, err = decodeStorage(val)
		case "bots":
			cfg.Bots, err = decodeBots(val)
		case "plugins":
			cfg.Plugins, err = decodePlugins(val)
		case "adapters":
			cfg.Adapters, err = decodeAdapters(val)
		case "engine":
			cfg.Engine, err = decodeEngine(val)
		default:
			return nil, fmt.Errorf("config: 未知顶层键 %q", key)
		}
		if err != nil {
			return nil, fmt.Errorf("config: %s 段: %w", key, err)
		}
	}
	return cfg, nil
}

// eachPair 校验 node 是映射后遍历其键值对，并拒绝重复键。
//
// path 是错误文案里的字段路径（如 storage、bots[0]、plugins.echo、engine）；
// kindErr 是节点类型不符时的整条措辞，沿用各段原有文案（如 "必须是映射"）。
// 键重复时报 `<path>: 键 %q 重复`，避免合并冲突/复制粘贴的配置静默取最后一个值
// （struct 解码路径由 yaml.v3 自行报 already set）。回调返回错误时立即中止。
func eachPair(node *yaml.Node, path, kindErr string, fn func(key string, val *yaml.Node) error) error {
	if node.Kind != yaml.MappingNode {
		return errors.New(kindErr)
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%s: 键 %q 重复", path, key)
		}
		seen[key] = struct{}{}
		if err := fn(key, val); err != nil {
			return err
		}
	}
	return nil
}

// decodeBots 解析 bots 序列。
func decodeBots(node *yaml.Node) ([]BotConfig, error) {
	if isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, errors.New("bots 必须是序列")
	}
	bots := make([]BotConfig, 0, len(node.Content))
	for i, item := range node.Content {
		bot, err := decodeBot(i, item)
		if err != nil {
			return nil, err
		}
		bots = append(bots, bot)
	}
	return bots, nil
}

// decodeBot 解析单个 bot 条目；name/adapter/enabled/plugins 之外的键进入 Settings。
func decodeBot(index int, node *yaml.Node) (BotConfig, error) {
	var bot BotConfig
	err := eachPair(node, fmt.Sprintf("bots[%d]", index), fmt.Sprintf("bots[%d]: 必须是映射", index),
		func(key string, val *yaml.Node) error {
			switch key {
			case "name":
				if err := val.Decode(&bot.Name); err != nil {
					return fmt.Errorf("bots[%d].name: %w", index, err)
				}
			case "adapter":
				if err := val.Decode(&bot.Adapter); err != nil {
					return fmt.Errorf("bots[%d].adapter: %w", index, err)
				}
			case "plugins":
				list, err := decodeStringList(val)
				if err != nil {
					return fmt.Errorf("bots[%d].plugins: %w", index, err)
				}
				bot.Plugins = list
			case "enabled":
				var enabled bool
				if err := val.Decode(&enabled); err != nil {
					return fmt.Errorf("bots[%d].enabled: %w", index, err)
				}
				bot.Enabled = &enabled
			default:
				var v any
				if err := val.Decode(&v); err != nil {
					return fmt.Errorf("bots[%d].%s: %w", index, key, err)
				}
				if bot.Settings == nil {
					bot.Settings = make(map[string]any)
				}
				bot.Settings[key] = normalizeValue(v)
			}
			return nil
		})
	if err != nil {
		return BotConfig{}, err
	}
	return bot, nil
}

// decodeStorage 解析 storage 段；type/dsn 之外的键进入 Params。
func decodeStorage(node *yaml.Node) (StorageConfig, error) {
	if isNull(node) {
		return StorageConfig{}, nil
	}
	var s StorageConfig
	err := eachPair(node, "storage", "必须是映射", func(key string, val *yaml.Node) error {
		switch key {
		case "type":
			if err := val.Decode(&s.Type); err != nil {
				return fmt.Errorf("storage.type: %w", err)
			}
		case "dsn":
			if err := val.Decode(&s.DSN); err != nil {
				return fmt.Errorf("storage.dsn: %w", err)
			}
		default:
			var v any
			if err := val.Decode(&v); err != nil {
				return fmt.Errorf("storage.%s: %w", key, err)
			}
			if s.Params == nil {
				s.Params = make(map[string]any)
			}
			s.Params[key] = normalizeValue(v)
		}
		return nil
	})
	if err != nil {
		return StorageConfig{}, err
	}
	return s, nil
}

// decodePlugins 解析 plugins 映射，支持映射写法与布尔标量简写。
func decodePlugins(node *yaml.Node) (map[string]PluginConfig, error) {
	if isNull(node) {
		return map[string]PluginConfig{}, nil
	}
	out := make(map[string]PluginConfig, len(node.Content)/2)
	err := eachPair(node, "plugins", "plugins 必须是映射", func(name string, val *yaml.Node) error {
		pc, err := decodePlugin(name, val)
		if err != nil {
			return err
		}
		out[name] = pc
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// decodePlugin 解析单个插件的配置节点：布尔标量简写或映射。
func decodePlugin(name string, node *yaml.Node) (PluginConfig, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			return PluginConfig{}, nil
		}
		enabled, ok := decodeBool(node)
		if !ok {
			return PluginConfig{}, fmt.Errorf("plugins.%s: 需要布尔值或映射", name)
		}
		return PluginConfig{Enabled: enabled}, nil
	case yaml.MappingNode:
		pc := PluginConfig{}
		err := eachPair(node, "plugins."+name, fmt.Sprintf("plugins.%s: 需要布尔值或映射", name),
			func(key string, val *yaml.Node) error {
				if key == "enabled" {
					enabled, ok := decodeBool(val)
					if !ok {
						return fmt.Errorf("plugins.%s.enabled: 需要布尔值", name)
					}
					pc.Enabled = enabled
					return nil
				}
				var v any
				if err := val.Decode(&v); err != nil {
					return fmt.Errorf("plugins.%s.%s: %w", name, key, err)
				}
				if pc.Settings == nil {
					pc.Settings = make(map[string]any)
				}
				pc.Settings[key] = normalizeValue(v)
				return nil
			})
		if err != nil {
			return PluginConfig{}, err
		}
		return pc, nil
	default:
		return PluginConfig{}, fmt.Errorf("plugins.%s: 需要布尔值或映射", name)
	}
}

// AdapterConfig 描述一个适配器的声明。
//
// 声明是可选的：进程内适配器只靠 bots[].adapter 引用即可工作，此处仅用于
// 显式启用/禁用。
type AdapterConfig struct {
	// Enabled 控制是否加载该适配器；nil（未写 enabled）视为启用。
	//
	// 只有显式的 enabled: false 才会禁用：该适配器不装配，其 bots[] 条目一并
	// 跳过（记日志）。与插件相反，适配器缺省是启用的——声明本身不是启用的前提。
	Enabled *bool
}

// IsEnabled 判断该声明是否启用：未显式写 enabled 时视为启用。
func (a AdapterConfig) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

// AdapterEnabled 判断名为 name 的适配器是否启用。
//
// 未在 adapters 段声明时视为启用：进程内适配器无需声明即可通过 bots[].adapter 使用。
func (c *Config) AdapterEnabled(name string) bool {
	if c == nil {
		return true
	}
	ac, ok := c.Adapters[name]
	if !ok {
		return true
	}
	return ac.IsEnabled()
}

// decodeAdapters 解析 adapters 映射：适配器名 -> 启用开关。
func decodeAdapters(node *yaml.Node) (map[string]AdapterConfig, error) {
	if isNull(node) {
		return map[string]AdapterConfig{}, nil
	}
	out := make(map[string]AdapterConfig, len(node.Content)/2)
	err := eachPair(node, "adapters", "adapters 必须是映射", func(name string, val *yaml.Node) error {
		ac, err := decodeAdapter(name, val)
		if err != nil {
			return err
		}
		out[name] = ac
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// decodeAdapter 解析单个适配器条目，只接受 enabled 键。
func decodeAdapter(name string, node *yaml.Node) (AdapterConfig, error) {
	var ac AdapterConfig
	err := eachPair(node, "adapters."+name, fmt.Sprintf("adapters.%s: 必须是映射", name),
		func(key string, val *yaml.Node) error {
			switch key {
			case "enabled":
				enabled, ok := decodeBool(val)
				if !ok {
					return fmt.Errorf("adapters.%s.enabled: 需要布尔值", name)
				}
				ac.Enabled = &enabled
			default:
				return fmt.Errorf("adapters.%s: 未知键 %q（只支持 enabled）", name, key)
			}
			return nil
		})
	if err != nil {
		return AdapterConfig{}, err
	}
	return ac, nil
}

// decodeEngine 解析 engine 段：未知键报错，时长/整数沿用 storage.params 的取值规则
// （时长写字符串或秒数、整数不得为小数）。键存在但无值（null）视为未配置。
//
// 所有字段保持零值即可由引擎填默认值，本函数不替引擎填默认（见 EngineConfig）。
func decodeEngine(node *yaml.Node) (EngineConfig, error) {
	if isNull(node) {
		return EngineConfig{}, nil
	}
	var e EngineConfig
	durations := map[string]*time.Duration{
		"event_timeout":    &e.EventTimeout,
		"rule_timeout":     &e.RuleTimeout,
		"shutdown_timeout": &e.ShutdownTimeout,
		"dedup_ttl":        &e.DedupTTL,
		"send_backoff":     &e.SendBackoff,
	}
	ints := map[string]*int{
		"bus_workers":    &e.BusWorkers,
		"bus_queue_size": &e.BusQueueSize,
		"dedup_capacity": &e.DedupCapacity,
		"send_retries":   &e.SendRetries,
	}
	err := eachPair(node, "engine", "engine: 必须是映射", func(key string, val *yaml.Node) error {
		if isNull(val) {
			return nil
		}
		if key == "command_prefixes" {
			list, err := decodeStringList(val)
			if err != nil {
				return fmt.Errorf("engine.command_prefixes: %w", err)
			}
			e.CommandPrefixes = list
			return nil
		}
		var v any
		if err := val.Decode(&v); err != nil {
			return fmt.Errorf("engine.%s: %w", key, err)
		}
		if dst, ok := durations[key]; ok {
			d, err := decodeDurationValue(normalizeValue(v))
			if err != nil {
				return fmt.Errorf("engine.%s: %w", key, err)
			}
			*dst = d
			return nil
		}
		if dst, ok := ints[key]; ok {
			n, err := decodeIntValue(normalizeValue(v))
			if err != nil {
				return fmt.Errorf("engine.%s: %w", key, err)
			}
			*dst = n
			return nil
		}
		return fmt.Errorf("engine: 未知键 %q", key)
	})
	if err != nil {
		return EngineConfig{}, err
	}
	return e, nil
}

// decodeStringList 解析字符串序列。
func decodeStringList(node *yaml.Node) ([]string, error) {
	if isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, errors.New("必须是字符串序列")
	}
	out := make([]string, 0, len(node.Content))
	for i, item := range node.Content {
		var s string
		if err := item.Decode(&s); err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// decodeBool 解析表示布尔的 YAML 节点。
//
// 原生 !!bool 节点直接解码；字符串节点按其文本再按布尔字面量兜底，
// 因此 `enabled: "false"` 这类带引号的写法也能得到 false。
func decodeBool(node *yaml.Node) (bool, bool) {
	if node.Kind != yaml.ScalarNode {
		return false, false
	}
	var b bool
	if err := node.Decode(&b); err == nil {
		return b, true
	}
	return toBool(node.Value)
}

// isNull 判断节点是否为 YAML 空值（键存在但无内容）。
func isNull(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && (node.Tag == "!!null" || node.Tag == "" && node.Value == "")
}

// normalizeValue 递归规范化任意 YAML 值，保证结果可被 encoding/json 编解码。
func normalizeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeValue(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[toStringKey(k)] = normalizeValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = normalizeValue(x[i])
		}
		return out
	default:
		return v
	}
}

// toStringKey 把 YAML 映射的非字符串键转成字符串键。
func toStringKey(k any) string {
	if s, ok := k.(string); ok {
		return s
	}
	return fmt.Sprint(k)
}

// applyDefaults 填充缺失的默认值。
//
// Engine 段刻意保持零值：引擎参数（超时、容量、重试等）的默认值由 internal/engine
// 的 orInt/orDuration 在构造时填充，这里替它填默认只会造成两套默认值。
func applyDefaults(c *Config) {
	if c.Log.Level == "" {
		c.Log.Level = defaultLogLevel
	}
	if c.Log.Format == "" {
		c.Log.Format = defaultLogFormat
	}
	if c.Plugins == nil {
		c.Plugins = map[string]PluginConfig{}
	}
	if c.Adapters == nil {
		c.Adapters = map[string]AdapterConfig{}
	}
	if c.Storage.Type == "" {
		c.Storage.Type = "memory"
	}
}

// validate 校验配置，返回带字段路径的错误。
func (c *Config) validate() error {
	seen := make(map[string]struct{}, len(c.Bots))
	for i := range c.Bots {
		b := &c.Bots[i]
		if b.Name == "" {
			return fmt.Errorf("config: bots[%d].name: 不能为空", i)
		}
		if _, dup := seen[b.Name]; dup {
			return fmt.Errorf("config: bots[%d].name: bot 名 %q 重复", i, b.Name)
		}
		seen[b.Name] = struct{}{}
		if b.Adapter == "" {
			return fmt.Errorf("config: bots[%d].adapter: 不能为空", i)
		}
		if !namePattern.MatchString(b.Adapter) {
			return fmt.Errorf("config: bots[%d].adapter: %q 只允许 [a-z0-9_-]", i, b.Adapter)
		}
	}
	for name := range c.Plugins {
		if name == "" {
			return errors.New("config: plugins: 插件名不能为空")
		}
		if !namePattern.MatchString(name) {
			return fmt.Errorf("config: plugins.%s: 插件名只允许 [a-z0-9_-]", name)
		}
	}
	for name := range c.Adapters {
		if name == "" {
			return errors.New("config: adapters: 适配器名不能为空")
		}
		if !namePattern.MatchString(name) {
			return fmt.Errorf("config: adapters.%s: 适配器名只允许 [a-z0-9_-]", name)
		}
	}
	if err := c.Storage.validate(); err != nil {
		return err
	}
	if err := c.Log.validate(); err != nil {
		return err
	}
	if err := c.Engine.validate(); err != nil {
		return err
	}
	return c.Limits.validate()
}

// 存储在配置层校验取值的参数键。其余键属于第三方后端的扩展空间，原样透传。
const (
	paramCleanupInterval = "cleanup_interval"
	paramMaxOpenConns    = "max_open_conns"
	paramMaxIdleConns    = "max_idle_conns"
	paramConnMaxLifetime = "conn_max_lifetime"
)

// validate 校验存储配置：类型名必须匹配 namePattern，已知参数取值必须合法。
//
// 不校验类型是否存在（由装配期负责），不校验 dsn 是否必填（由各后端负责）。
// 未知参数不报错：内置后端会忽略它们（各键的适用范围见 docs/configuration.md 12.5）。
func (s StorageConfig) validate() error {
	if s.Type == "" {
		return errors.New("config: storage.type: 不能为空")
	}
	if !namePattern.MatchString(s.Type) {
		return fmt.Errorf("config: storage.type: %q 只允许 [a-z0-9_-]", s.Type)
	}
	for key, val := range s.Params {
		if err := validateStorageParam(key, val); err != nil {
			return err
		}
	}
	return nil
}

// validateStorageParam 校验一个存储参数的取值。
//
// 取值必须在配置层拦住：后端读取参数时对缺失键或类型不符的值会静默回落到默认值
// （如 max_open_conns 写成 "abc" 得到 0 = 不限制），配置层不校验就只会表现为
// 「配置没生效」。nil 等价于未配置，未知键原样透传。
func validateStorageParam(key string, val any) error {
	if val == nil {
		return nil
	}
	path := "config: storage.params." + key + ": "
	switch key {
	case paramCleanupInterval:
		d, err := decodeDurationValue(val)
		if err != nil {
			return errors.New(path + err.Error())
		}
		if d < 0 {
			return errors.New(path + "不能为负数（0 表示关闭后台清理）")
		}
	case paramConnMaxLifetime:
		d, err := decodeDurationValue(val)
		if err != nil {
			return errors.New(path + err.Error())
		}
		if d < 0 {
			return errors.New(path + "不能为负数（0 表示不按龄关闭连接）")
		}
	case paramMaxOpenConns, paramMaxIdleConns:
		n, err := decodeIntValue(val)
		if err != nil {
			return errors.New(path + err.Error())
		}
		if n < 0 {
			return errors.New(path + "不能为负数（0 表示不限制）")
		}
	}
	return nil
}

// decodeDurationValue 解析时长取值：字符串按 time.ParseDuration，数字按秒。
//
// 接受的类型与 pkg/bot.Config.Duration 一致，保证「校验通过」等于「后端能按原值
// 读到」，不会静默回落默认值。
func decodeDurationValue(v any) (time.Duration, error) {
	switch t := v.(type) {
	case time.Duration:
		return t, nil
	case string:
		d, err := time.ParseDuration(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("需要时长（如 10s），实际为 %q", t)
		}
		return d, nil
	case int:
		return time.Duration(t) * time.Second, nil
	case int64:
		return time.Duration(t) * time.Second, nil
	case float64:
		return time.Duration(t * float64(time.Second)), nil
	default:
		return 0, fmt.Errorf("需要时长（如 10s），实际为 %v", v)
	}
}

// decodeIntValue 解析整数取值：字符串按十进制解析，浮点数必须是整数值。
//
// 接受的类型与 pkg/bot.Config.Int 一致（浮点额外要求整数值，避免截断）。
func decodeIntValue(v any) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		if t != math.Trunc(t) || t < math.MinInt32 || t > math.MaxInt32 {
			return 0, fmt.Errorf("需要整数，实际为 %v", t)
		}
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("需要整数，实际为 %q", t)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("需要整数，实际为 %v", v)
	}
}

// validate 校验日志配置：级别与格式都必须在允许集合内（大小写不敏感）。
//
// 空值表示未配置，由 applyDefaults 填充默认值，此处视为合法。
func (l LogConfig) validate() error {
	if l.Level != "" {
		if _, ok := validLogLevels[strings.ToLower(l.Level)]; !ok {
			return fmt.Errorf("config: log.level: 不支持的值 %q（可选 debug|info|warn|error）", l.Level)
		}
	}
	if l.Format != "" {
		if _, ok := validLogFormats[strings.ToLower(l.Format)]; !ok {
			return fmt.Errorf("config: log.format: 不支持的值 %q（可选 text|json）", l.Format)
		}
	}
	return nil
}

// validate 校验限流配置：速率与突发容量都不能为负。
func (l LimitsConfig) validate() error {
	rates := [...]struct {
		path  string
		value float64
	}{
		{"limits.handler_rate", l.HandlerRate},
		{"limits.send_rate", l.SendRate},
	}
	for _, r := range rates {
		if r.value < 0 {
			return fmt.Errorf("config: %s: 不能为负数", r.path)
		}
	}
	bursts := [...]struct {
		path  string
		value int
	}{
		{"limits.handler_burst", l.HandlerBurst},
		{"limits.send_burst", l.SendBurst},
	}
	for _, b := range bursts {
		if b.value < 0 {
			return fmt.Errorf("config: %s: 不能为负数", b.path)
		}
	}
	return nil
}

// validate 校验引擎配置：命令行前缀每项去空白后非空；时长与容量类不允许负数。
//
// 空列表与零值合法：它们表示「用引擎默认值」（`internal/engine` 的 orInt/orDuration
// 回落到默认）。`send_retries` 的 0 是有意义的取值（不重试）而非「用默认」，但同样
// 只需拒绝负数——负数若放行会被引擎归一为 0，配置层必须拦住以免与显式 0 混为一谈。
func (e EngineConfig) validate() error {
	for i, p := range e.CommandPrefixes {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("config: engine.command_prefixes[%d]: 不能为空白", i)
		}
	}
	durations := [...]struct {
		path  string
		value time.Duration
	}{
		{"engine.event_timeout", e.EventTimeout},
		{"engine.rule_timeout", e.RuleTimeout},
		{"engine.shutdown_timeout", e.ShutdownTimeout},
		{"engine.dedup_ttl", e.DedupTTL},
		{"engine.send_backoff", e.SendBackoff},
	}
	for _, d := range durations {
		if d.value < 0 {
			return fmt.Errorf("config: %s: 不能为负数", d.path)
		}
	}
	ints := [...]struct {
		path  string
		value int
	}{
		{"engine.bus_workers", e.BusWorkers},
		{"engine.bus_queue_size", e.BusQueueSize},
		{"engine.dedup_capacity", e.DedupCapacity},
		{"engine.send_retries", e.SendRetries},
	}
	for _, n := range ints {
		if n.value < 0 {
			return fmt.Errorf("config: %s: 不能为负数", n.path)
		}
	}
	return nil
}
