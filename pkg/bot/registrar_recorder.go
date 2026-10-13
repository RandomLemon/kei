package bot

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
)

// RecordingRegistrar 是 Registrar 的记录式实现，供插件作者编写单元测试。
//
// 它只记录规则而不做匹配，测试代码可以取出规则后直接调用其 Handler。
// 语义与路由器保持一致：未设置 EventType 的消息类触发默认限定为
// EventMessage，正则编译失败或触发参数为空（空命令名、空正则、空关键词
// 列表或含空词、空事件类型）会记录到 Errors 并跳过该规则，不会 panic；
// Use 只影响调用之后注册的规则，每条规则在注册时复制当时的中间件列表。
type RecordingRegistrar struct {
	mu       sync.Mutex
	rules    []*Rule
	errors   []error
	counters map[string]int
	pending  []Middleware
}

// 确保 RecordingRegistrar 满足 Registrar。
var _ Registrar = (*RecordingRegistrar)(nil)

// NewRecordingRegistrar 构造空的记录式注册器。
func NewRecordingRegistrar() *RecordingRegistrar {
	return &RecordingRegistrar{counters: make(map[string]int)}
}

// OnCommand 记录一个命令规则；命令名为空时记录错误并跳过。
func (r *RecordingRegistrar) OnCommand(name string, h Handler, opts ...Option) {
	if name == "" {
		r.recordErr(errors.New("bot: OnCommand: 命令名不能为空"))
		return
	}
	r.add("command", &Rule{Command: name, EventType: EventMessage, Handler: h}, opts)
}

// OnRegex 记录一个正则规则；表达式为空或非法时记录错误并跳过该规则。
func (r *RecordingRegistrar) OnRegex(expr string, h Handler, opts ...Option) {
	if expr == "" {
		r.recordErr(errors.New("bot: OnRegex: 正则表达式不能为空"))
		return
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		r.recordErr(err)
		return
	}
	r.add("regex", &Rule{Regex: re, EventType: EventMessage, Handler: h}, opts)
}

// OnKeyword 记录一个关键词规则；列表为空或含空词时记录错误并跳过。
func (r *RecordingRegistrar) OnKeyword(words []string, h Handler, opts ...Option) {
	if len(words) == 0 {
		r.recordErr(errors.New("bot: OnKeyword: 关键词列表不能为空"))
		return
	}
	for i, w := range words {
		if w == "" {
			r.recordErr(fmt.Errorf("bot: OnKeyword: 第 %d 个关键词为空", i+1))
			return
		}
	}
	r.add("keyword", &Rule{Keywords: slices.Clone(words), EventType: EventMessage, Handler: h}, opts)
}

// OnEvent 记录一个事件类型规则；事件类型为空时记录错误并跳过。
func (r *RecordingRegistrar) OnEvent(t EventType, h Handler, opts ...Option) {
	if t == "" {
		r.recordErr(errors.New("bot: OnEvent: 事件类型不能为空"))
		return
	}
	r.add("event", &Rule{EventType: t, Handler: h}, opts)
}

// OnAll 记录一个兜底规则。
func (r *RecordingRegistrar) OnAll(h Handler, opts ...Option) {
	r.add("all", &Rule{Handler: h}, opts)
}

// Use 记录插件级中间件，只影响调用之后注册的规则（与路由器一致）；
// mw 为 nil 时忽略。
func (r *RecordingRegistrar) Use(mw Middleware) {
	if mw == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, mw)
}

// Rules 返回已记录规则的快照。
func (r *RecordingRegistrar) Rules() []*Rule {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Rule(nil), r.rules...)
}

// Errors 返回记录到的错误（例如非法正则）。
func (r *RecordingRegistrar) Errors() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.errors...)
}

// Command 返回指定命令名的规则。
func (r *RecordingRegistrar) Command(name string) (*Rule, bool) {
	return r.Find(func(rule *Rule) bool { return rule.Command == name })
}

// Find 返回第一条满足条件的规则。
func (r *RecordingRegistrar) Find(fn func(*Rule) bool) (*Rule, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rule := range r.rules {
		if fn(rule) {
			return rule, true
		}
	}
	return nil, false
}

// recordErr 记录一个注册错误（例如非法正则或空触发参数）。
func (r *RecordingRegistrar) recordErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, err)
}

func (r *RecordingRegistrar) add(kind string, rule *Rule, opts []Option) {
	for _, opt := range opts {
		if opt != nil {
			opt(rule)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	index := r.counters[kind]
	r.counters[kind] = index + 1
	if rule.ID == "" {
		rule.ID = "rec:" + kind + ":" + itoa(index)
	}
	rule.Middlewares = slices.Clone(r.pending)
	r.rules = append(r.rules, rule)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
