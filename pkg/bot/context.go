package bot

import "context"

type ctxKey int

const (
	pluginContextKey ctxKey = iota
	routeKey
)

// Route 描述当前正在执行的路由规则，由路由器写入 ctx 供中间件与日志使用。
type Route struct {
	// Plugin 是规则所属插件名。
	Plugin string
	// RuleID 是规则 ID。
	RuleID string
	// Trigger 是触发方式的可读描述，例如 "command:echo"、"regex:^hi"。
	Trigger string
	// Priority 是规则优先级。
	Priority int
	// AdminOnly 表示规则要求管理员身份，由 Auth 中间件校验。
	AdminOnly bool
	// RegexMatches 是正则规则的捕获结果（含完整匹配），非正则规则为空。
	RegexMatches []string
}

// WithPluginContext 把插件运行期依赖写入 ctx。
func WithPluginContext(ctx context.Context, pc PluginContext) context.Context {
	return context.WithValue(ctx, pluginContextKey, pc)
}

// PluginContextFrom 读取插件运行期依赖，未注入时返回 false。
func PluginContextFrom(ctx context.Context) (PluginContext, bool) {
	pc, ok := ctx.Value(pluginContextKey).(PluginContext)
	if !ok {
		return PluginContext{}, false
	}
	return pc, true
}

// WithRoute 把当前路由信息写入 ctx。
func WithRoute(ctx context.Context, r *Route) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, routeKey, r)
}

// RouteFrom 读取当前路由信息，未注入时返回 false。
func RouteFrom(ctx context.Context) (*Route, bool) {
	r, ok := ctx.Value(routeKey).(*Route)
	return r, ok
}
