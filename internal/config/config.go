// Package config 负责加载、合并与校验 kei 的 YAML 配置。
//
// 配置有两层来源：YAML 文件与环境变量。环境变量统一以 KEI_ 为前缀，
// 路径段之间用 "_" 连接；匹配时不区分大小写，且 "-"、"." 与 "_" 等价。
// 例如 KEI_LOG_LEVEL 覆盖 Log.Level，KEI_BOTS_FEISHU_MAIN_APP_ID 覆盖
// 名为 "feishu-main" 的 bot 的 Settings["app_id"]。未匹配任何已知路径的
// KEI_* 变量会被忽略（不报错）。
//
// 顶层允许的键为 log、metrics、bots、plugins、grpc、limits、auth，
// 其余顶层键一律报错。
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// 日志默认值。
const (
	defaultLogLevel  = "info"
	defaultLogFormat = "text"
)

// adapterNamePattern 限制 adapter 名称的字符集。
var adapterNamePattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

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
	// Bots 是机器人实例列表，至少需要一个。
	Bots []BotConfig `yaml:"bots"`
	// Plugins 是插件名到插件配置的映射。
	Plugins map[string]PluginConfig `yaml:"plugins"`
	// Grpc 是外部插件 gRPC 通道配置。
	Grpc GrpcConfig `yaml:"grpc"`
	// Limits 是限流配置。
	Limits LimitsConfig `yaml:"limits"`
	// Auth 是权限配置。
	Auth AuthConfig `yaml:"auth"`
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

// GrpcConfig 是外部插件 gRPC 通道配置。
type GrpcConfig struct {
	// Addr 是核心 gRPC 服务（BotService）监听地址，为空表示不启动外部插件通道。
	Addr string `yaml:"addr"`
	// CertFile 是服务端 TLS 证书文件。
	CertFile string `yaml:"cert_file"`
	// KeyFile 是服务端 TLS 私钥文件。
	KeyFile string `yaml:"key_file"`
	// CAFile 是客户端 CA 文件，用于 mTLS。
	CAFile string `yaml:"ca_file"`
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

// BotConfig 描述一个机器人实例。
type BotConfig struct {
	// Name 是实例名，全局唯一。
	Name string
	// Adapter 是平台适配器名，只允许 [a-z0-9_-]。
	Adapter string
	// Settings 是 YAML 中除 name/adapter/plugins 外的其余键；
	// 取值保证可被 encoding/json 编解码。
	Settings map[string]any
	// Plugins 是该实例启用的插件名，为空表示全部启用。
	Plugins []string
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
		case "grpc":
			err = val.Decode(&cfg.Grpc)
		case "limits":
			err = val.Decode(&cfg.Limits)
		case "auth":
			err = val.Decode(&cfg.Auth)
		case "bots":
			cfg.Bots, err = decodeBots(val)
		case "plugins":
			cfg.Plugins, err = decodePlugins(val)
		default:
			return nil, fmt.Errorf("config: 未知顶层键 %q", key)
		}
		if err != nil {
			return nil, fmt.Errorf("config: %s 段: %w", key, err)
		}
	}
	return cfg, nil
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

// decodeBot 解析单个 bot 条目，未声明的键进入 Settings。
func decodeBot(index int, node *yaml.Node) (BotConfig, error) {
	if node.Kind != yaml.MappingNode {
		return BotConfig{}, fmt.Errorf("bots[%d]: 必须是映射", index)
	}
	var bot BotConfig
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "name":
			if err := val.Decode(&bot.Name); err != nil {
				return BotConfig{}, fmt.Errorf("bots[%d].name: %w", index, err)
			}
		case "adapter":
			if err := val.Decode(&bot.Adapter); err != nil {
				return BotConfig{}, fmt.Errorf("bots[%d].adapter: %w", index, err)
			}
		case "plugins":
			list, err := decodeStringList(val)
			if err != nil {
				return BotConfig{}, fmt.Errorf("bots[%d].plugins: %w", index, err)
			}
			bot.Plugins = list
		default:
			var v any
			if err := val.Decode(&v); err != nil {
				return BotConfig{}, fmt.Errorf("bots[%d].%s: %w", index, key, err)
			}
			if bot.Settings == nil {
				bot.Settings = make(map[string]any)
			}
			bot.Settings[key] = normalizeValue(v)
		}
	}
	return bot, nil
}

// decodePlugins 解析 plugins 映射，支持映射写法与布尔标量简写。
func decodePlugins(node *yaml.Node) (map[string]PluginConfig, error) {
	if isNull(node) {
		return map[string]PluginConfig{}, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("plugins 必须是映射")
	}
	out := make(map[string]PluginConfig, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		pc, err := decodePlugin(name, node.Content[i+1])
		if err != nil {
			return nil, err
		}
		out[name] = pc
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
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i].Value, node.Content[i+1]
			if key == "enabled" {
				enabled, ok := decodeBool(val)
				if !ok {
					return PluginConfig{}, fmt.Errorf("plugins.%s.enabled: 需要布尔值", name)
				}
				pc.Enabled = enabled
				continue
			}
			var v any
			if err := val.Decode(&v); err != nil {
				return PluginConfig{}, fmt.Errorf("plugins.%s.%s: %w", name, key, err)
			}
			if pc.Settings == nil {
				pc.Settings = make(map[string]any)
			}
			pc.Settings[key] = normalizeValue(v)
		}
		return pc, nil
	default:
		return PluginConfig{}, fmt.Errorf("plugins.%s: 需要布尔值或映射", name)
	}
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
}

// validate 校验配置，返回带字段路径的错误。
func (c *Config) validate() error {
	if len(c.Bots) == 0 {
		return errors.New("config: bots: 至少需要一个 bot")
	}
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
		if !adapterNamePattern.MatchString(b.Adapter) {
			return fmt.Errorf("config: bots[%d].adapter: %q 只允许 [a-z0-9_-]", i, b.Adapter)
		}
	}
	for name := range c.Plugins {
		if name == "" {
			return errors.New("config: plugins: 插件名不能为空")
		}
	}
	if err := c.Log.validate(); err != nil {
		return err
	}
	if err := c.Limits.validate(); err != nil {
		return err
	}
	return c.Grpc.validate()
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

// validate 校验 gRPC 配置：TLS 的 cert/key/ca 三个文件必须同时提供。
func (g GrpcConfig) validate() error {
	files := [...]struct{ path, value string }{
		{"grpc.cert_file", g.CertFile},
		{"grpc.key_file", g.KeyFile},
		{"grpc.ca_file", g.CAFile},
	}
	var missing []string
	for _, f := range files {
		if f.value == "" {
			missing = append(missing, f.path)
		}
	}
	if len(missing) == 0 || len(missing) == len(files) {
		return nil
	}
	return fmt.Errorf("config: grpc: cert_file/key_file/ca_file 必须同时提供，缺少 %s",
		strings.Join(missing, ", "))
}
