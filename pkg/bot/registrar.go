package bot

import (
	"context"
	"regexp"
	"strings"
)

// Handler 是插件事件处理函数。
//
// 所有 Handler 都会被 Recover、Timeout 中间件包裹，因此允许 panic 与阻塞，
// 但必须响应 ctx 取消。
type Handler func(ctx context.Context, e *Event, r Reply) error

// Middleware 是洋葱模型中间件：返回的 Handler 负责在合适的时机调用 next。
type Middleware func(next Handler) Handler

// Option 用于定制注册的规则。
type Option func(*Rule)

// Rule 是一条路由规则。
//
// 匹配语义：规则的各过滤条件之间是「与」关系；未设置的过滤条件不参与判断。
// 多条规则同时命中时，按 Priority 从大到小依次执行（同一优先级按注册顺序）。
type Rule struct {
	// ID 是规则唯一标识，未显式指定时由注册器生成。
	ID string
	// Priority 是优先级，数值越大越先执行。
	Priority int
	// Platforms 限定生效平台，空表示全部平台。
	Platforms []string
	// BotIDs 限定生效的机器人（bot 名称）。
	//
	// nil 表示不做机器人过滤；非 nil（含空切片）表示白名单，此时只有对应
	// BotID 的事件才能命中，空切片即任何机器人都命中不了。引擎用它实现
	// 「每个 bot 只启用部分插件」。
	BotIDs []string
	// EventType 限定事件类型，空表示全部类型。
	EventType EventType
	// Kind 限定会话类型，空表示全部类型。
	Kind MessageKind
	// Command 限定命令名（不含前缀，比较时忽略大小写）。
	Command string
	// Regex 限定正则，对消息纯文本做匹配，取值时使用 FindStringSubmatch。
	Regex *regexp.Regexp
	// Keywords 限定关键词，命中任意一个（忽略大小写的子串匹配）即触发。
	Keywords []string
	// Match 是自定义断言，可用于权限等复杂判断。
	Match func(*Event) bool
	// AdminOnly 表示该规则只允许管理员触发，需要 Auth 中间件配合。
	AdminOnly bool
	// Handler 是命中后的处理函数。
	Handler Handler
	// Plugin 是规则所属插件名，由注册器填充。
	Plugin string
	// Middlewares 是该插件通过 Registrar.Use 声明的中间件。
	Middlewares []Middleware
}

// Matches 判断规则是否命中事件（不包含 AdminOnly 与自定义断言之外的中间件逻辑）。
func (r *Rule) Matches(e *Event) bool {
	if r.EventType != "" && e.Type != r.EventType {
		return false
	}
	if r.Kind != "" {
		if e.Message == nil || e.Message.Kind != r.Kind {
			return false
		}
	}
	if len(r.Platforms) > 0 && !containsString(r.Platforms, e.Platform) {
		return false
	}
	if r.BotIDs != nil && !containsString(r.BotIDs, e.BotID) {
		return false
	}
	if r.Command != "" {
		if e.Command == nil || !strings.EqualFold(e.Command.Name, r.Command) {
			return false
		}
	}
	if r.Regex != nil {
		if !r.Regex.MatchString(e.Text()) {
			return false
		}
	}
	if len(r.Keywords) > 0 && !matchKeyword(e.Text(), r.Keywords) {
		return false
	}
	if r.Match != nil && !r.Match(e) {
		return false
	}
	return true
}

// Registrar 是插件注册触发规则的入口。
//
// 实现必须并发安全，且同一插件注册的规则相互隔离。
type Registrar interface {
	// OnCommand 注册命令触发，name 不含前缀（例如 "echo"）。
	OnCommand(name string, h Handler, opts ...Option)
	// OnRegex 注册正则触发，expr 为 RE2 表达式，编译失败会在插件 Setup 阶段返回错误。
	OnRegex(expr string, h Handler, opts ...Option)
	// OnKeyword 注册关键词触发，命中任意词即触发。
	OnKeyword(words []string, h Handler, opts ...Option)
	// OnEvent 注册事件类型触发。
	OnEvent(t EventType, h Handler, opts ...Option)
	// OnAll 注册兜底处理，匹配所有未被其它条件排除的事件。
	OnAll(h Handler, opts ...Option)
	// Use 为该插件注册的所有规则追加中间件，按注册顺序由外到内包裹。
	Use(mw Middleware)
}

// WithPriority 设置规则优先级，数值越大越先执行。
func WithPriority(p int) Option {
	return func(r *Rule) { r.Priority = p }
}

// WithID 设置规则 ID，便于日志与指标定位。
func WithID(id string) Option {
	return func(r *Rule) { r.ID = id }
}

// WithPlatforms 限定规则只在指定平台生效。
func WithPlatforms(platforms ...string) Option {
	return func(r *Rule) { r.Platforms = append([]string(nil), platforms...) }
}

// WithKind 限定规则只对指定会话类型生效。
func WithKind(k MessageKind) Option {
	return func(r *Rule) { r.Kind = k }
}

// WithBotIDs 限定规则只在指定机器人上生效，传空列表表示任何机器人都命中不了。
func WithBotIDs(ids ...string) Option {
	return func(r *Rule) { r.BotIDs = append([]string{}, ids...) }
}

// WithAdmin 标记规则为管理员专属，需要 Auth 中间件配合。
func WithAdmin() Option {
	return func(r *Rule) { r.AdminOnly = true }
}

// WithEventType 限定规则的事件类型，等价于 OnEvent，但可用于 OnAll/OnKeyword 等。
func WithEventType(t EventType) Option {
	return func(r *Rule) { r.EventType = t }
}

// WithMatch 追加自定义断言。
func WithMatch(fn func(*Event) bool) Option {
	return func(r *Rule) { r.Match = fn }
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// matchKeyword 判断文本是否包含任意关键词，忽略大小写且不分配内存。
func matchKeyword(text string, words []string) bool {
	if text == "" {
		return false
	}
	for _, w := range words {
		if w == "" {
			continue
		}
		if containsFold(text, w) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	n, m := len(s), len(sub)
	if m == 0 || m > n {
		return false
	}
	for i := range n - m + 1 {
		if strings.EqualFold(s[i:i+m], sub) {
			return true
		}
	}
	return false
}
