// Package middleware 提供事件处理链路上的通用中间件。
//
// 中间件均为 bot.Middleware，遵循洋葱模型：Chain 的第一个中间件最先执行、
// 最后返回。它们通过 bot.RouteFrom 读取当前路由信息，与 internal/router
// 与 internal/engine 协作。
package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/RandomLemon/kei/internal/dedup"
	"github.com/RandomLemon/kei/internal/metrics"
	"github.com/RandomLemon/kei/internal/ratelimit"
	"github.com/RandomLemon/kei/pkg/bot"
)

// 限流作用域，用于 RateLimit 决定以什么粒度生成限流键。
const (
	// ScopeUser 按发送者限流。
	ScopeUser = "user"
	// ScopeChannel 按会话（群/频道）限流。
	ScopeChannel = "channel"
	// ScopePlugin 按插件限流。
	ScopePlugin = "plugin"
)

// 中间件统一使用的错误，调用方可用 errors.Is 判定拦截原因。
var (
	// ErrPanic 表示 handler 发生 panic 且已被 Recover 捕获。
	ErrPanic = errors.New("middleware: handler panic")
	// ErrTimeout 表示 handler 执行超时。
	ErrTimeout = errors.New("middleware: handler timeout")
	// ErrDuplicate 表示事件重复，已被 Dedup 拦截。
	ErrDuplicate = errors.New("middleware: duplicate event")
	// ErrRateLimited 表示请求超出限流配额。
	ErrRateLimited = errors.New("middleware: rate limited")
	// ErrUnauthorized 表示调用者不具备规则要求的管理员身份。
	ErrUnauthorized = errors.New("middleware: unauthorized")
)

// passthrough 原样返回 next，用于空链与禁用状态下的中间件。
func passthrough(next bot.Handler) bot.Handler { return next }

// DefaultLabel 是 handler 耗时指标与日志中无法确定插件名时的占位标签。
const DefaultLabel = "unknown"

// Recover 捕获 handler 的 panic（含非 error 值），记录堆栈后返回错误。
//
// 返回值包装 ErrPanic，可用 errors.Is(err, ErrPanic) 判定；next 正常返回的
// 错误原样透传。log 为 nil 时不记录日志，仍返回错误。
func Recover(log *slog.Logger) bot.Middleware {
	if log == nil {
		// 未安装处理器时也要吞掉 panic，只是不记录日志。
		log = slog.New(slog.DiscardHandler)
	}
	return func(next bot.Handler) bot.Handler {
		return func(ctx context.Context, e *bot.Event, r bot.Reply) (err error) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				log.Error("中间件: handler panic",
					"plugin", pluginName(ctx),
					"rule", ruleID(ctx),
					"platform", platformOf(e),
					"event_id", eventID(e),
					"panic", fmt.Sprint(v),
					"stack", string(debug.Stack()),
				)
				err = fmt.Errorf("%w: %v", ErrPanic, v)
			}()
			return next(ctx, e, r)
		}
	}
}

// Logger 记录 handler 调用前后的结构化日志。
//
// 成功调用记 Debug；失败调用按错误类型分级：正常的策略拒绝（ErrDuplicate、
// ErrRateLimited、ErrUnauthorized）记 Warn 并带 "decision"="denied" 字段，避免
// 运维把预期的拒绝当成故障；其余错误（含 ErrTimeout、ErrPanic）记 Error。
// 未安装任何日志处理器（log == nil）时透传。
// 字段包含 plugin、rule、platform、kind、user、channel、event_id、duration_ms、
// error（拒绝时另有 decision），便于按插件与规则聚合排查。
func Logger(log *slog.Logger) bot.Middleware {
	if log == nil {
		return passthrough
	}
	return func(next bot.Handler) bot.Handler {
		return func(ctx context.Context, e *bot.Event, r bot.Reply) error {
			start := time.Now()
			err := next(ctx, e, r)
			d := time.Since(start)
			// 路由信息在调用前由路由器注入外层 ctx，此处读取即可拿到当前规则。
			plugin, rule := pluginName(ctx), ruleID(ctx)
			args := []any{
				"plugin", plugin,
				"rule", rule,
				"platform", platformOf(e),
				"kind", kindOf(e),
				"user", userIDOf(e),
				"channel", channelIDOf(e),
				"event_id", eventID(e),
				"duration_ms", float64(d.Microseconds()) / 1000,
			}
			if err == nil {
				// 未启用调试级别时 slog 会直接丢弃，Debug 无输出成本。
				log.DebugContext(ctx, "中间件: 事件处理完成", args...)
				return nil
			}
			args = append(args, "error", err)
			if isDenied(err) {
				args = append(args, "decision", "denied")
				log.WarnContext(ctx, "中间件: 事件被拒绝", args...)
				return err
			}
			log.ErrorContext(ctx, "中间件: 事件处理失败", args...)
			return err
		}
	}
}

// isDenied 判断错误是否属于预期的策略拒绝（去重、限流、鉴权），
// 这类错误记 Warn 而不是 Error。
func isDenied(err error) bool {
	return errors.Is(err, ErrDuplicate) ||
		errors.Is(err, ErrRateLimited) ||
		errors.Is(err, ErrUnauthorized)
}

// Metrics 把插件与规则的耗时、结果写入指标记录器 rec。
//
// plugin/rule 取自 bot.RouteFrom，缺失时使用 DefaultLabel；rec 为 nil 时透传。
// next 返回的错误原样透传，不会改变调用链行为。
func Metrics(rec metrics.Recorder) bot.Middleware {
	if rec == nil {
		return passthrough
	}
	return func(next bot.Handler) bot.Handler {
		return func(ctx context.Context, e *bot.Event, r bot.Reply) error {
			plugin, rule := pluginName(ctx), ruleID(ctx)
			start := time.Now()
			err := next(ctx, e, r)
			rec.EventHandled(plugin, rule, time.Since(start), err)
			return err
		}
	}
}

// Timeout 限制 next 的执行时间，d <= 0 时透传表示不限时。
//
// next 在独立 goroutine 中执行并把结果写入容量为 1 的 channel，因此超时后
// goroutine 不会阻塞在发送上；ctx 取消同样触发超时错误。返回错误包装 ErrTimeout。
func Timeout(d time.Duration) bot.Middleware {
	if d <= 0 {
		return passthrough
	}
	return func(next bot.Handler) bot.Handler {
		return func(ctx context.Context, e *bot.Event, r bot.Reply) error {
			callCtx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			results := make(chan error, 1)
			go func() { results <- next(callCtx, e, r) }()
			select {
			case err := <-results:
				return err
			case <-callCtx.Done():
				return fmt.Errorf("%w: %w", ErrTimeout, callCtx.Err())
			}
		}
	}
}

// Dedup 丢弃重复事件。
//
// 全局中间件链是按规则包裹执行的：同一事件可能命中多条规则，每条规则都会
// 走一遍链。因此去重键必须带规则标识，否则第二条命中规则会被误判为重复。
//
// 键的构造顺序：有路由信息时以 route.RuleID + "|" 为前缀；基础键优先取 e.ID，
// 为空时退化为 SessionKey + "|" + 消息 ID（无消息时只用 SessionKey）。基础键
// 为空（拿不到任何标识）时放行，不做去重。set 为 nil 时透传。
func Dedup(set *dedup.Set) bot.Middleware {
	if set == nil {
		return passthrough
	}
	return func(next bot.Handler) bot.Handler {
		return func(ctx context.Context, e *bot.Event, r bot.Reply) error {
			key := dedupKey(ctx, e)
			if key == "" {
				// 无法识别事件，宁可放行也不要误伤。
				return next(ctx, e, r)
			}
			if !set.Add(key) {
				return fmt.Errorf("%w: %s", ErrDuplicate, key)
			}
			return next(ctx, e, r)
		}
	}
}

// RateLimit 按 scope 指定的粒度限流，超过配额返回 ErrRateLimited。
//
// scope 为 ScopeChannel 时用会话 ID（为空则回退 SessionKey），ScopePlugin 时
// 用插件名（为空则为 DefaultLabel），其余 scope（含 ScopeUser）一律按发送者
// ID 限流。l 为 nil 时透传表示不限流。
func RateLimit(l *ratelimit.Limiter, scope string) bot.Middleware {
	if l == nil {
		return passthrough
	}
	return func(next bot.Handler) bot.Handler {
		return func(ctx context.Context, e *bot.Event, r bot.Reply) error {
			var key string
			switch scope {
			case ScopeChannel:
				key = channelIDOf(e)
				if key == "" {
					key = sessionKey(e)
				}
			case ScopePlugin:
				key = pluginName(ctx)
			default:
				key = userIDOf(e)
			}
			if !l.Allow(key) {
				return fmt.Errorf("%w: scope=%s key=%s", ErrRateLimited, scope, key)
			}
			return next(ctx, e, r)
		}
	}
}

// Auth 校验规则的管理员权限。
//
// 仅当 Route.AdminOnly 为 true 时生效：isAdmin 为 nil 或返回 false 时返回
// ErrUnauthorized，且不调用 next。
func Auth(isAdmin func(*bot.Event) bool) bot.Middleware {
	return func(next bot.Handler) bot.Handler {
		return func(ctx context.Context, e *bot.Event, r bot.Reply) error {
			if route, ok := bot.RouteFrom(ctx); ok && route.AdminOnly {
				if isAdmin == nil || !isAdmin(e) {
					return fmt.Errorf("%w: plugin=%s rule=%s", ErrUnauthorized, route.Plugin, route.RuleID)
				}
			}
			return next(ctx, e, r)
		}
	}
}

// dedupKey 生成事件去重键，带规则前缀以避免同一事件的其它规则被误判重复。
//
// 返回空串表示无法识别事件（无 Route、无 ID 且无会话信息），调用方应放行。
func dedupKey(ctx context.Context, e *bot.Event) string {
	if e == nil {
		return ""
	}
	base := e.ID
	if base == "" {
		base = sessionKey(e)
		if e.Message != nil && e.Message.ID != "" {
			base += "|" + e.Message.ID
		}
	}
	if base == "" {
		return ""
	}
	if route, ok := bot.RouteFrom(ctx); ok && route.RuleID != "" {
		return route.RuleID + "|" + base
	}
	return base
}

// pluginName 读取当前插件名，缺失时返回 DefaultLabel。
func pluginName(ctx context.Context) string {
	if route, ok := bot.RouteFrom(ctx); ok && route.Plugin != "" {
		return route.Plugin
	}
	return DefaultLabel
}

// ruleID 读取当前规则 ID，缺失时返回空串。
func ruleID(ctx context.Context) string {
	if route, ok := bot.RouteFrom(ctx); ok {
		return route.RuleID
	}
	return ""
}

// sessionKey 安全地计算会话键（e 为 nil 时返回空串）。
func sessionKey(e *bot.Event) string {
	if e == nil {
		return ""
	}
	return e.SessionKey()
}

// platformOf 安全地读取事件平台名。
func platformOf(e *bot.Event) string {
	if e == nil {
		return ""
	}
	return e.Platform
}

// eventID 安全地读取事件 ID。
func eventID(e *bot.Event) string {
	if e == nil {
		return ""
	}
	return e.ID
}

// userIDOf 安全地读取发送者 ID。
func userIDOf(e *bot.Event) string {
	if e == nil || e.Sender == nil {
		return ""
	}
	return e.Sender.ID
}

// channelIDOf 安全地读取会话 ID。
func channelIDOf(e *bot.Event) string {
	if e == nil || e.Channel == nil {
		return ""
	}
	return e.Channel.ID
}

// kindOf 安全地读取会话类型。
func kindOf(e *bot.Event) string {
	if e == nil || e.Channel == nil {
		return ""
	}
	return string(e.Channel.Kind)
}

// 确保 *metrics.Registry 持续满足本包使用的 metrics.Recorder 契约（编译期校验）。
var _ metrics.Recorder = (*metrics.Registry)(nil)
