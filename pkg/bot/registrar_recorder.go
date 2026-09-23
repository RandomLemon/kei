package bot

import (
	"regexp"
	"sync"
)

// RecordingRegistrar 是 Registrar 的记录式实现，供插件作者编写单元测试。
//
// 它只记录规则而不做匹配，测试代码可以取出规则后直接调用其 Handler。
// 语义与路由器保持一致：未设置 EventType 的消息类触发默认限定为
// EventMessage，正则编译失败会记录到 Errors 而不会 panic。
type RecordingRegistrar struct {
	mu       sync.Mutex
	rules    []*Rule
	errors   []error
	counters map[string]int
}

// 确保 RecordingRegistrar 满足 Registrar。
var _ Registrar = (*RecordingRegistrar)(nil)

// NewRecordingRegistrar 构造空的记录式注册器。
func NewRecordingRegistrar() *RecordingRegistrar {
	return &RecordingRegistrar{counters: make(map[string]int)}
}

// OnCommand 记录一个命令规则。
func (r *RecordingRegistrar) OnCommand(name string, h Handler, opts ...Option) {
	r.add("command", &Rule{Command: name, EventType: EventMessage, Handler: h}, opts)
}

// OnRegex 记录一个正则规则；表达式非法时记录错误并跳过该规则。
func (r *RecordingRegistrar) OnRegex(expr string, h Handler, opts ...Option) {
	re, err := regexp.Compile(expr)
	if err != nil {
		r.mu.Lock()
		r.errors = append(r.errors, err)
		r.mu.Unlock()
		return
	}
	r.add("regex", &Rule{Regex: re, EventType: EventMessage, Handler: h}, opts)
}

// OnKeyword 记录一个关键词规则。
func (r *RecordingRegistrar) OnKeyword(words []string, h Handler, opts ...Option) {
	r.add("keyword", &Rule{Keywords: append([]string(nil), words...), EventType: EventMessage, Handler: h}, opts)
}

// OnEvent 记录一个事件类型规则。
func (r *RecordingRegistrar) OnEvent(t EventType, h Handler, opts ...Option) {
	r.add("event", &Rule{EventType: t, Handler: h}, opts)
}

// OnAll 记录一个兜底规则。
func (r *RecordingRegistrar) OnAll(h Handler, opts ...Option) {
	r.add("all", &Rule{Handler: h}, opts)
}

// Use 记录插件级中间件，不改变已记录的规则。
func (r *RecordingRegistrar) Use(mw Middleware) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rule := range r.rules {
		rule.Middlewares = append(rule.Middlewares, mw)
	}
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
