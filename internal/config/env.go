package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// envPrefix 是所有环境变量覆盖项的前缀。
const envPrefix = "KEI_"

// applyEnv 用 env 覆盖 cfg。env 元素格式为 "KEY=VALUE"；
// 不以 KEI_ 开头、或无法匹配任何已知路径的变量一律被忽略。
//
// bot 名的匹配基于进入本函数时的原始名字快照，因此
// KEI_BOTS_X_NAME 与 KEI_BOTS_X_OTHER_KEY 的先后顺序不影响结果。
func applyEnv(c *Config, env []string) error {
	names := make([]string, len(c.Bots))
	for i := range c.Bots {
		names[i] = canonicalName(c.Bots[i].Name)
	}
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || len(key) <= len(envPrefix) || !strings.EqualFold(key[:len(envPrefix)], envPrefix) {
			continue
		}
		segments := canonicalSegments(key[len(envPrefix):])
		if len(segments) == 0 {
			continue
		}
		if err := applyEnvPath(c, names, segments, key, value); err != nil {
			return err
		}
	}
	return nil
}

// applyEnvPath 把一条环境变量应用到对应字段；路径未知时静默忽略。
func applyEnvPath(c *Config, botNames []string, segments []string, key, value string) error {
	full := strings.Join(segments, "_")
	switch full {
	case "log_level":
		c.Log.Level = value
		return nil
	case "log_format":
		c.Log.Format = value
		return nil
	case "metrics_addr":
		c.Metrics.Addr = value
		return nil
	case "grpc_addr":
		c.Grpc.Addr = value
		return nil
	case "grpc_cert_file":
		c.Grpc.CertFile = value
		return nil
	case "grpc_key_file":
		c.Grpc.KeyFile = value
		return nil
	case "grpc_ca_file":
		c.Grpc.CAFile = value
		return nil
	case "limits_handler_rate":
		return envFloat(key, "Limits.HandlerRate", value, &c.Limits.HandlerRate)
	case "limits_handler_burst":
		return envInt(key, "Limits.HandlerBurst", value, &c.Limits.HandlerBurst)
	case "limits_send_rate":
		return envFloat(key, "Limits.SendRate", value, &c.Limits.SendRate)
	case "limits_send_burst":
		return envInt(key, "Limits.SendBurst", value, &c.Limits.SendBurst)
	case "auth_admin_users":
		c.Auth.AdminUsers = splitList(value)
		return nil
	}
	switch {
	case strings.HasPrefix(full, "bots_"):
		applyBotEnv(c, botNames, full[len("bots_"):], value)
	case strings.HasPrefix(full, "plugins_"):
		applyPluginEnv(c, full[len("plugins_"):], value)
	case strings.HasPrefix(full, "adapters_"):
		return applyAdapterEnv(c, full[len("adapters_"):], value)
	}
	return nil
}

// applyBotEnv 处理 KEI_BOTS_<BOTNAME>_... 形式的覆盖。
//
// bot 名与键名都做了规范化（小写，"-"/"." 视作 "_"），因此
// KEI_BOTS_FEISHU_MAIN_APP_ID 能命中名为 "feishu-main" 的 bot。
// 同时命中多个 bot 时取名字规范化后最长者，长度相同时全部应用。
func applyBotEnv(c *Config, botNames []string, rest, value string) {
	best, tail := -1, ""
	var indices []int
	for i, name := range botNames {
		if name == "" {
			continue
		}
		prefix := name + "_"
		if len(rest) <= len(prefix) || !strings.HasPrefix(rest, prefix) {
			continue
		}
		if len(name) > best {
			best = len(name)
			indices = indices[:0]
		}
		if len(name) == best {
			indices = append(indices, i)
			tail = rest[len(prefix):]
		}
	}
	for _, i := range indices {
		bot := &c.Bots[i]
		switch tail {
		case "name":
			bot.Name = value
		case "adapter":
			bot.Adapter = value
		case "plugins":
			bot.Plugins = splitList(value)
		case "enabled":
			if enabled, ok := envBool(value); ok {
				bot.Enabled = &enabled
			}
		default:
			setSetting(&bot.Settings, tail, value)
		}
	}
}

// applyPluginEnv 处理 KEI_PLUGINS_<PLUGIN>_... 形式的覆盖。
//
// 只有配置中已存在的插件名才会被命中，未知插件名一律忽略。
// 同时命中多个插件时取名字规范化后最长者，长度相同时全部应用。
func applyPluginEnv(c *Config, rest, value string) {
	best, tail := -1, ""
	var names []string
	for name := range c.Plugins {
		canonical := canonicalName(name)
		if canonical == "" {
			continue
		}
		prefix := canonical + "_"
		if len(rest) <= len(prefix) || !strings.HasPrefix(rest, prefix) {
			continue
		}
		if len(canonical) > best {
			best = len(canonical)
			names = names[:0]
		}
		if len(canonical) == best {
			names = append(names, name)
			tail = rest[len(prefix):]
		}
	}
	for _, name := range names {
		pc := c.Plugins[name]
		if tail == "enabled" {
			if enabled, ok := envBool(value); ok {
				pc.Enabled = enabled
			}
			c.Plugins[name] = pc
			continue
		}
		setSetting(&pc.Settings, tail, value)
		c.Plugins[name] = pc
	}
}

// applyAdapterEnv 处理 KEI_ADAPTERS_<NAME>_... 形式的覆盖。
//
// 只有配置中已存在的适配器名才会被命中，未知适配器名一律忽略。
// 同时命中多个适配器时取名字规范化后最长者，长度相同时全部应用。
// 支持 enabled（控制启用状态）与 grpc_addr/token/platform/permissions/timeout，
// 其余键写入该适配器的进程级配置。
func applyAdapterEnv(c *Config, rest, value string) error {
	best, tail := -1, ""
	var names []string
	for name := range c.Adapters {
		canonical := canonicalName(name)
		if canonical == "" {
			continue
		}
		prefix := canonical + "_"
		if len(rest) <= len(prefix) || !strings.HasPrefix(rest, prefix) {
			continue
		}
		if len(canonical) > best {
			best = len(canonical)
			names = names[:0]
		}
		if len(canonical) == best {
			names = append(names, name)
			tail = rest[len(prefix):]
		}
	}
	for _, name := range names {
		ac := c.Adapters[name]
		switch tail {
		case "enabled":
			if enabled, ok := envBool(value); ok {
				ac.Enabled = &enabled
			}
		case "grpc_addr":
			ac.GrpcAddr = value
		case "token":
			ac.Token = value
		case "platform":
			ac.Platform = value
		case "permissions":
			ac.Permissions = splitList(value)
		case "timeout":
			d, err := parseDurationValue(tail, value)
			if err != nil {
				return fmt.Errorf("config: 环境变量 KEI_ADAPTERS_%s_%s: %w", strings.ToUpper(name), strings.ToUpper(tail), err)
			}
			ac.Timeout = d
		default:
			setSetting(&ac.Settings, tail, value)
		}
		c.Adapters[name] = ac
	}
	return nil
}

// parseDurationValue 解析环境变量中的时长：先按 time.ParseDuration，
// 纯数字按秒处理；两者都失败时报错。
func parseDurationValue(field, value string) (time.Duration, error) {
	if d, err := time.ParseDuration(strings.TrimSpace(value)); err == nil {
		return d, nil
	}
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("需要时长（如 10s），实际为 %q", value)
}

// setSetting 写入一个 Settings 键。path 是规范化后的路径（小写下划线形式）。
//
// 键名优先沿用 YAML 中规范化后同名的原始写法，否则使用 path 本身；
// 值按 YAML 规则做类型转换。
func setSetting(settings *map[string]any, path, value string) {
	if path == "" {
		return
	}
	target, matched := path, false
	for existing := range *settings {
		if canonicalName(existing) != path {
			continue
		}
		// 多个键规范化后同名时取字典序最小者，保证结果确定。
		if !matched || existing < target {
			target, matched = existing, true
		}
	}
	if *settings == nil {
		*settings = make(map[string]any)
	}
	(*settings)[target] = convertValue(value)
}

// canonicalSegments 把环境变量名的后缀按 "_" 切分并逐段规范化，空段被丢弃。
func canonicalSegments(s string) []string {
	parts := strings.Split(s, "_")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = canonicalName(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// canonicalName 把名称/路径段规范化：转小写，并把 "-"、"." 视作 "_"。
func canonicalName(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z':
			b.WriteByte(ch + ('a' - 'A'))
		case ch == '-' || ch == '.':
			b.WriteByte('_')
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// convertValue 按 YAML 规则把环境变量字符串解析成 any。
//
// true→bool、3→int、1.5→float64、其余→string；空值视为空字符串。
// 无法按 YAML 解析时退化为原始字符串，避免个别字符导致加载失败。
func convertValue(value string) any {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	var v any
	if err := yaml.Unmarshal([]byte(value), &v); err != nil {
		return value
	}
	return normalizeValue(v)
}

// envBool 把环境变量值解析成布尔：优先按 YAML 规则转换（bool 原样），
// 失败时按文本字面量兜底，因此 "false"、false、0 都得到 false。
func envBool(value string) (bool, bool) {
	if v := convertValue(value); v != nil {
		if b, ok := v.(bool); ok {
			return b, true
		}
	}
	return toBool(value)
}

// toBool 解析布尔字面量，识别 true/false/1/0（忽略大小写与首尾空白）。
func toBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

// envFloat 解析浮点型覆盖值；空值表示不覆盖。
func envFloat(key, path, value string, dst *float64) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return fmt.Errorf("config: 环境变量 %s 覆盖 %s 失败，需要数字，实际为 %q", key, path, value)
	}
	*dst = f
	return nil
}

// envInt 解析整型覆盖值；空值表示不覆盖。
func envInt(key, path, value string, dst *int) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("config: 环境变量 %s 覆盖 %s 失败，需要整数，实际为 %q", key, path, value)
	}
	*dst = n
	return nil
}

// splitList 按逗号切分列表值，忽略空白与空项；无有效项时返回 nil。
func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
