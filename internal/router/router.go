// Package router 实现触发规则的路由与分发。
//
// Router 维护一份全局规则表：插件通过 bot.Registrar 注册的规则、插件级中间件
// 与全局中间件在注册时被折叠成可执行的处理链，事件到达时按优先级串行执行
// 所有命中规则。规则表以「不可变快照 + 原子替换」的方式发布，因此 Dispatch
// 可以被多个 worker 并发调用，而注册操作加锁串行化。
package router

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Options 是 Router 的构造参数。
type Options struct {
	// CommandPrefixes 是允许的命令前缀，为空时使用默认值 ["/"]。
	CommandPrefixes []string
	// Middlewares 是全局中间件，包裹在所有插件级中间件之外。
	//
	// 同一组中间件中按注册顺序由外到内包裹，即「先注册的先执行」；
	// 构造 Router 时会复制该切片，之后修改调用方的切片不影响已生效的链。
	Middlewares []bot.Middleware
	// Replies 为每一次规则调用创建独立的回复构建器；为空时退化为 bot.NoopReply。
	Replies bot.ReplyFactory
	// Logger 是可选日志器，用于记录规则注册与命中。
	Logger *slog.Logger
}

// entry 是不可变的规则执行单元。
//
// 规则注册完成后其字段不再被 Router 修改，因此可被并发读取。
type entry struct {
	// rule 是规则本体，同时作为 Rule.Matches 的匹配依据。
	rule *bot.Rule
	// handler 是已折叠插件级与全局中间件后的处理器。
	handler bot.Handler
	// trigger 是规则触发的可读描述，形如 "command:echo"。
	trigger string
}

// ruleSet 是一份按执行顺序排好的规则快照。
type ruleSet struct {
	entries []*entry
}

// Router 是规则路由表。
//
// 零值不可用，必须通过 New 构造。Router 构造后即可并发使用：
// Register/Registrar 与 Dispatch 可同时发生。
type Router struct {
	prefixes []string
	globals  []bot.Middleware
	logger   *slog.Logger
	replies  bot.ReplyFactory

	// mu 保护写入侧状态（规则表有序切片、错误累计、ID 与序号、插件中间件）。
	mu       sync.RWMutex
	ordered  []*entry
	errs     []error
	seq      map[string]int
	ids      map[string]struct{}
	pluginMW map[string][]bot.Middleware

	// snap 是并发读取的规则快照，注册时整体替换。
	snap atomic.Pointer[ruleSet]
}

// New 构造路由器。
func New(opts Options) *Router {
	prefixes := slices.Clone(opts.CommandPrefixes)
	if len(prefixes) == 0 {
		prefixes = []string{"/"}
	}
	r := &Router{
		prefixes: prefixes,
		globals:  slices.Clone(opts.Middlewares),
		logger:   opts.Logger,
		replies:  opts.Replies,
		seq:      make(map[string]int),
		ids:      make(map[string]struct{}),
		pluginMW: make(map[string][]bot.Middleware),
	}
	r.snap.Store(&ruleSet{})
	return r
}

// Registrar 返回某个插件的注册器。
//
// 同一插件名多次调用返回的注册器共享同一份规则表与插件中间件列表。
func (r *Router) Registrar(plugin string) bot.Registrar {
	return &registrar{r: r, plugin: plugin}
}

// Register 注册一条完整规则，校验失败返回错误且不注册。
//
// 调用方可以自行设置 Rule.Middlewares；Rule.ID 为空时按 plugin:kind:index
// 自动生成（kind 由规则字段推断）。Register 返回的错误不会重复记入
// RegistrationErrors。
func (r *Router) Register(rule *bot.Rule) error {
	if rule == nil {
		return errors.New("router: 规则不能为 nil")
	}
	return r.register(rule.Plugin, rule, "", false)
}

// Rules 返回规则表的快照副本，按执行顺序排列（优先级降序，同优先级按注册顺序）。
//
// 返回值与内部状态完全隔离：副本中的规则结构体及其切片都已被复制，
// 修改返回值不会影响路由器。
func (r *Router) Rules() []*bot.Rule {
	set := r.snap.Load()
	if set == nil {
		return nil
	}
	out := make([]*bot.Rule, 0, len(set.entries))
	for _, e := range set.entries {
		out = append(out, cloneRule(e.rule))
	}
	return out
}

// RegistrationErrors 返回注册阶段累计错误的副本。
//
// OnRegex 等无法通过返回值报错的注册入口会把错误记录在这里（例如正则编译失败）；
// Register 的错误直接返回给调用方，不会重复累计。
func (r *Router) RegistrationErrors() []error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.errs)
}

// UpdateRules 对已注册的规则做最后一次调整，例如引擎按 bot 白名单收窄 Rule.BotIDs。
//
// fn 会收到规则表里的原始 *bot.Rule（不是 Rules 返回的拷贝），可以直接修改其字段。
// 持写锁执行，因此规则表的替换是原子的，不会出现半更新状态；但 Dispatch 读取
// 规则对象本身不加锁，所以仍必须在开始 Dispatch 之前调用完毕。
//
// 调整完成后会按 Priority 降序、同优先级按注册顺序重建快照并重新排序（fn 不保证
// 不改 Priority），因此调整后的顺序立即生效。fn 为 nil 时返回错误；
// 若 fn 把某条规则的 Handler 置空，则返回错误且不发布新快照。
func (r *Router) UpdateRules(fn func(*bot.Rule)) error {
	if fn == nil {
		return errors.New("router: UpdateRules 的 fn 不能为 nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	entries := make([]*entry, 0, len(r.ordered))
	for _, old := range r.ordered {
		fn(old.rule)
		if old.rule.Handler == nil {
			return fmt.Errorf("router: 规则 %q 的 Handler 为 nil", old.rule.ID)
		}
		entries = append(entries, &entry{
			rule:    old.rule,
			handler: fold(r.globals, old.rule.Middlewares, old.rule.Handler),
			trigger: triggerOf(old.rule),
		})
	}
	slices.SortStableFunc(entries, func(a, b *entry) int {
		return cmp.Compare(b.rule.Priority, a.rule.Priority)
	})
	r.ordered = entries
	r.snap.Store(&ruleSet{entries: slices.Clone(r.ordered)})
	if r.logger != nil {
		r.logger.Debug("router: 规则已更新", "rules", len(entries))
	}
	return nil
}

// Dispatch 分发一个事件：必要时解析命令，然后按优先级串行执行所有命中规则。
//
// 事件非空且 Command 为空但 Message 非空时，会用配置的前缀解析命令并写回 ev；
// 同一次事件命中的多条规则都会被完整执行，其错误用 errors.Join 聚合返回，
// 没有任何规则命中时返回 nil。
func (r *Router) Dispatch(ctx context.Context, ev *bot.Event) error {
	if ev == nil {
		return nil
	}
	if ev.Command == nil && ev.Message != nil {
		ev.Command = bot.ParseCommand(ev.Text(), r.prefixes)
	}
	set := r.snap.Load()
	if set == nil || len(set.entries) == 0 {
		return nil
	}
	var errs []error
	for _, e := range set.entries {
		if !e.rule.Matches(ev) {
			continue
		}
		if r.logger != nil {
			r.logger.Debug("router: 命中规则",
				"plugin", e.rule.Plugin, "rule", e.rule.ID, "trigger", e.trigger,
				"priority", e.rule.Priority, "platform", ev.Platform, "type", string(ev.Type))
		}
		rctx := bot.WithRoute(ctx, &bot.Route{
			Plugin:       e.rule.Plugin,
			RuleID:       e.rule.ID,
			Trigger:      e.trigger,
			Priority:     e.rule.Priority,
			AdminOnly:    e.rule.AdminOnly,
			RegexMatches: regexMatches(e.rule, ev),
		})
		// 每条规则使用独立的 Reply，避免多条规则的回复内容互相污染。
		reply := r.newReply(rctx, ev)
		if err := e.handler(rctx, ev, reply); err != nil {
			errs = append(errs, fmt.Errorf("router: 规则 %s 执行失败: %w", e.rule.ID, err))
		}
	}
	return errors.Join(errs...)
}

// newReply 为单次规则调用创建独立的回复构建器，工厂不可用时退化为 NoopReply。
func (r *Router) newReply(ctx context.Context, ev *bot.Event) bot.Reply {
	if r.replies == nil {
		return bot.NewNoopReply()
	}
	if reply := r.replies(ctx, ev); reply != nil {
		return reply
	}
	return bot.NewNoopReply()
}

// register 是注册的唯一入口，由 Register 与 registrar 共用。
//
// applyPluginMW 为真时（经 Registrar 注册）用插件当前的 Use 中间件快照覆盖
// 规则的中间件列表，因此插件中间件只影响调用 Use 之后注册的规则。
func (r *Router) register(plugin string, rule *bot.Rule, kind string, applyPluginMW bool) error {
	if rule == nil {
		return errors.New("router: 规则不能为 nil")
	}
	if rule.Handler == nil {
		return fmt.Errorf("router: 规则缺少 Handler（插件 %q）", plugin)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if rule.Plugin == "" {
		rule.Plugin = plugin
	}
	if kind == "" {
		kind = ruleKind(rule)
	}
	if rule.ID == "" {
		rule.ID = r.nextID(rule.Plugin, kind)
	}
	if _, dup := r.ids[rule.ID]; dup {
		return fmt.Errorf("router: 规则 ID 已存在: %q", rule.ID)
	}
	if applyPluginMW {
		rule.Middlewares = slices.Clone(r.pluginMW[plugin])
	}

	e := &entry{
		rule:    rule,
		handler: fold(r.globals, rule.Middlewares, rule.Handler),
		trigger: triggerOf(rule),
	}
	r.ids[rule.ID] = struct{}{}
	r.ordered = append(r.ordered, e)
	slices.SortStableFunc(r.ordered, func(a, b *entry) int {
		return cmp.Compare(b.rule.Priority, a.rule.Priority)
	})
	r.snap.Store(&ruleSet{entries: slices.Clone(r.ordered)})
	if r.logger != nil {
		r.logger.Debug("router: 注册规则",
			"plugin", rule.Plugin, "rule", rule.ID, "trigger", e.trigger, "priority", rule.Priority)
	}
	return nil
}

// recordErr 累计一个无法通过返回值上报的注册错误，并记录日志。
func (r *Router) recordErr(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	r.errs = append(r.errs, err)
	r.mu.Unlock()
	if r.logger != nil {
		r.logger.Warn("router: 注册规则失败", "error", err)
	}
}

// nextID 生成 plugin:kind:index 形式的规则 ID，序号按「插件 + 类别」独立递增。
func (r *Router) nextID(plugin, kind string) string {
	key := plugin + "\x00" + kind
	index := r.seq[key]
	r.seq[key] = index + 1
	return plugin + ":" + kind + ":" + strconv.Itoa(index)
}

// ruleKind 按规则字段推断类别，优先级与 triggerOf 一致。
func ruleKind(rule *bot.Rule) string {
	switch {
	case rule.Command != "":
		return kindCommand
	case rule.Regex != nil:
		return kindRegex
	case len(rule.Keywords) > 0:
		return kindKeyword
	case rule.EventType != "":
		return kindEvent
	default:
		return kindAll
	}
}

// triggerOf 返回规则触发的可读描述。
//
// 规则同时设置多个过滤条件时，按 command → regex → keyword → event → all
// 的优先级取第一个，与匹配条件的强弱一致。
func triggerOf(rule *bot.Rule) string {
	switch {
	case rule.Command != "":
		return "command:" + rule.Command
	case rule.Regex != nil:
		return "regex:" + rule.Regex.String()
	case len(rule.Keywords) > 0:
		return "keyword:" + strings.Join(rule.Keywords, ",")
	case rule.EventType != "":
		return "event:" + string(rule.EventType)
	default:
		return "all"
	}
}

// fold 把中间件折叠成处理器。
//
// 由内到外依次是 handler → 插件的 Use 中间件 → 全局中间件；同一组内按注册
// 顺序由外到内包裹，即「先注册的先执行」（pkg/bot.Registrar.Use 的约定）。
func fold(globals, pluginMWs []bot.Middleware, h bot.Handler) bot.Handler {
	return wrap(globals, wrap(pluginMWs, h))
}

// wrap 用一组中间件包裹处理器，先注册的位于更外层。
func wrap(mws []bot.Middleware, h bot.Handler) bot.Handler {
	for _, mw := range slices.Backward(mws) {
		if mw == nil {
			continue
		}
		h = mw(h)
	}
	return h
}

// regexMatches 返回正则规则对事件文本的捕获组（含完整匹配）；非正则规则返回 nil。
//
// 只在规则已命中后调用，因此正常情况下一定匹配；若文本在匹配后被外部修改
// 导致不再匹配，则返回 nil 而不是旧结果。
func regexMatches(rule *bot.Rule, ev *bot.Event) []string {
	if rule.Regex == nil {
		return nil
	}
	return rule.Regex.FindStringSubmatch(ev.Text())
}

// cloneRule 复制规则及其切片字段，供 Rules 返回隔离的快照。
func cloneRule(in *bot.Rule) *bot.Rule {
	if in == nil {
		return nil
	}
	out := *in
	out.Platforms = slices.Clone(in.Platforms)
	out.Keywords = slices.Clone(in.Keywords)
	out.BotIDs = slices.Clone(in.BotIDs)
	out.Middlewares = slices.Clone(in.Middlewares)
	return &out
}
