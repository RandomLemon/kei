package metrics

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry 是并发安全的指标注册表，实现 Recorder 并暴露 Prometheus 文本端点。
//
// 零值不可用，必须通过 New 构造。所有 Recorder 方法对 nil 接收者安全，
// 因此未启用指标时可直接传入 (*Registry)(nil) 作为空实现。
type Registry struct {
	mu sync.RWMutex

	published map[string]*counter
	dropped   map[droppedKey]*counter
	handled   map[handledKey]*counter
	handling  map[pluginRuleKey]*histogram
	sent      map[platformResultKey]*counter
	sending   map[string]*histogram
	matched   map[pluginRuleKey]*counter
}

// droppedKey 是丢弃计数的标签组合。
type droppedKey struct{ platform, reason string }

// pluginRuleKey 是 plugin/rule 两标签的组合。
type pluginRuleKey struct{ plugin, rule string }

// handledKey 是处理计数的标签组合，在 plugin/rule 之外带上 result。
type handledKey struct {
	pluginRuleKey
	result string
}

// platformResultKey 是平台与结果两标签的组合。
type platformResultKey struct{ platform, result string }

// New 构造指标注册表。
func New() *Registry {
	return &Registry{
		published: make(map[string]*counter),
		dropped:   make(map[droppedKey]*counter),
		handled:   make(map[handledKey]*counter),
		handling:  make(map[pluginRuleKey]*histogram),
		sent:      make(map[platformResultKey]*counter),
		sending:   make(map[string]*histogram),
		matched:   make(map[pluginRuleKey]*counter),
	}
}

// EventHandled 记录一次事件处理结果与耗时；err 非 nil 时结果标签为 error。
func (r *Registry) EventHandled(plugin, rule string, d time.Duration, err error) {
	if r == nil {
		return
	}
	counterFor(&r.mu, r.handled, handledKey{pluginRuleKey{plugin, rule}, resultOf(err)}).inc()
	histogramFor(&r.mu, r.handling, pluginRuleKey{plugin, rule}, defaultBuckets).observe(d.Seconds())
}

// EventPublished 记录一次事件发布。
func (r *Registry) EventPublished(platform string) {
	if r == nil {
		return
	}
	counterFor(&r.mu, r.published, platform).inc()
}

// EventDropped 记录一次事件丢弃及其原因。
func (r *Registry) EventDropped(platform, reason string) {
	if r == nil {
		return
	}
	counterFor(&r.mu, r.dropped, droppedKey{platform, reason}).inc()
}

// MessageSent 记录一次消息发送的结果与耗时；err 非 nil 时结果标签为 error。
func (r *Registry) MessageSent(platform string, d time.Duration, err error) {
	if r == nil {
		return
	}
	counterFor(&r.mu, r.sent, platformResultKey{platform, resultOf(err)}).inc()
	histogramFor(&r.mu, r.sending, platform, defaultBuckets).observe(d.Seconds())
}

// RuleMatched 记录一次规则命中。
func (r *Registry) RuleMatched(plugin, rule string) {
	if r == nil {
		return
	}
	counterFor(&r.mu, r.matched, pluginRuleKey{plugin, rule}).inc()
}

// resultOf 把错误映射为结果标签值。
func resultOf(err error) string {
	if err != nil {
		return resultError
	}
	return resultOK
}

// counterFor 取出或创建标签对应的计数器，并发首次创建只发生一次。
func counterFor[K comparable](mu *sync.RWMutex, m map[K]*counter, key K) *counter {
	mu.RLock()
	c, ok := m[key]
	mu.RUnlock()
	if ok {
		return c
	}
	mu.Lock()
	defer mu.Unlock()
	if c, ok = m[key]; ok {
		return c
	}
	c = &counter{}
	m[key] = c
	return c
}

// histogramFor 取出或创建标签对应的直方图，并发首次创建只发生一次。
func histogramFor[K comparable](mu *sync.RWMutex, m map[K]*histogram, key K, buckets []float64) *histogram {
	mu.RLock()
	h, ok := m[key]
	mu.RUnlock()
	if ok {
		return h
	}
	mu.Lock()
	defer mu.Unlock()
	if h, ok = m[key]; ok {
		return h
	}
	h = newHistogram(buckets)
	m[key] = h
	return h
}

// Handler 返回以 Prometheus 文本格式输出全部指标的 http.Handler。
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var b strings.Builder
		b.Grow(4096)
		r.render(&b)
		_, _ = w.Write([]byte(b.String()))
	})
}

// render 渲染全部指标族。
//
// 先在读锁内复制各映射的指针快照，随后无锁渲染：计数器是原子的，直方图
// 自带互斥，因此渲染期间的并发写入不会产生竞态。
func (r *Registry) render(b *strings.Builder) {
	if r == nil {
		return
	}
	r.mu.RLock()
	published, dropped, handled, handling, sent, sending, matched :=
		copyMap(r.published), copyMap(r.dropped), copyMap(r.handled), copyMap(r.handling),
		copyMap(r.sent), copyMap(r.sending), copyMap(r.matched)
	r.mu.RUnlock()

	writeFamily(b, metricEventsPublished, helpEventsPublished, typeCounter, func() {
		for _, k := range sortedKeys(published, func(k string) string { return k }) {
			writeLine(b, metricEventsPublished, []label{{"platform", k}}, formatUint(published[k].value()))
		}
	})
	writeFamily(b, metricEventsDropped, helpEventsDropped, typeCounter, func() {
		for _, k := range sortedKeys(dropped, func(k droppedKey) string { return k.platform + "\x00" + k.reason }) {
			writeLine(b, metricEventsDropped,
				[]label{{"platform", k.platform}, {"reason", k.reason}},
				formatUint(dropped[k].value()))
		}
	})
	writeFamily(b, metricEventsHandled, helpEventsHandled, typeCounter, func() {
		for _, k := range sortedKeys(handled, func(k handledKey) string {
			return k.plugin + "\x00" + k.rule + "\x00" + k.result
		}) {
			writeLine(b, metricEventsHandled,
				sortLabels([]label{{"plugin", k.plugin}, {"rule", k.rule}, {"result", k.result}}),
				formatUint(handled[k].value()))
		}
	})
	writeFamily(b, metricEventHandling, helpEventHandling, typeHistogram, func() {
		for _, k := range sortedKeys(handling, func(k pluginRuleKey) string { return k.plugin + "\x00" + k.rule }) {
			writeHistogram(b, metricEventHandling,
				[]label{{"plugin", k.plugin}, {"rule", k.rule}}, handling[k])
		}
	})
	writeFamily(b, metricMessagesSent, helpMessagesSent, typeCounter, func() {
		for _, k := range sortedKeys(sent, func(k platformResultKey) string { return k.platform + "\x00" + k.result }) {
			writeLine(b, metricMessagesSent,
				sortLabels([]label{{"platform", k.platform}, {"result", k.result}}),
				formatUint(sent[k].value()))
		}
	})
	writeFamily(b, metricMessageSend, helpMessageSend, typeHistogram, func() {
		for _, k := range sortedKeys(sending, func(k string) string { return k }) {
			writeHistogram(b, metricMessageSend, []label{{"platform", k}}, sending[k])
		}
	})
	writeFamily(b, metricRulesMatched, helpRulesMatched, typeCounter, func() {
		for _, k := range sortedKeys(matched, func(k pluginRuleKey) string { return k.plugin + "\x00" + k.rule }) {
			writeLine(b, metricRulesMatched,
				sortLabels([]label{{"plugin", k.plugin}, {"rule", k.rule}}),
				formatUint(matched[k].value()))
		}
	})
}

// writeHistogram 写出单个直方图的所有样本行。
//
// 依次是各 le 桶的累计计数、+Inf 桶（等于观测总数）、_sum 与 _count；
// 无任何观测时也输出 +Inf/_sum/_count，保持 Prometheus 语义。
func writeHistogram(b *strings.Builder, name string, base []label, h *histogram) {
	buckets, cumulative, sum, total := h.snapshot()
	for i, ub := range buckets {
		writeLine(b, name, sortLabels(withLabel(base, "le", formatFloat(ub))), formatUint(cumulative[i]))
	}
	writeLine(b, name, sortLabels(withLabel(base, "le", "+Inf")), formatUint(total))
	writeLine(b, name+"_sum", sortLabels(base), formatFloat(sum))
	writeLine(b, name+"_count", sortLabels(base), formatUint(total))
}

// withLabel 在基础标签上追加一个标签，返回新切片。
func withLabel(base []label, name, value string) []label {
	out := make([]label, 0, len(base)+1)
	out = append(out, base...)
	return append(out, label{name, value})
}

// sortLabels 按标签名、再按标签值字典序排序，返回传入切片本身。
func sortLabels(labels []label) []label {
	sort.Slice(labels, func(i, j int) bool {
		if labels[i].name != labels[j].name {
			return labels[i].name < labels[j].name
		}
		return labels[i].value < labels[j].value
	})
	return labels
}

// copyMap 浅复制指标映射（键可比较、值为指针）。
func copyMap[K comparable, V any](src map[K]V) map[K]V {
	dst := make(map[K]V, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// sortedKeys 返回按 labelKey 字典序排序后的映射键。
func sortedKeys[K comparable, V any](m map[K]V, labelKey func(K) string) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return labelKey(keys[i]) < labelKey(keys[j]) })
	return keys
}

// writeFamily 写出一个指标族的 HELP/TYPE 头，随后由 body 输出样本。
func writeFamily(b *strings.Builder, name, help, typ string, body func()) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(help)
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(typ)
	b.WriteByte('\n')
	body()
}

// formatUint 输出整数计数，避免浮点格式化带来的精度问题。
func formatUint(v uint64) string {
	return strconv.FormatUint(v, 10)
}
