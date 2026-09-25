// Package metrics 提供极简的 Prometheus 文本格式指标实现。
//
// 不引入任何第三方客户端：内部用原子计数与互斥保护的直方图桶实现
// counter / histogram，Registry 暴露为 /metrics 可用的 http.Handler。
// 所有 Recorder 方法对 nil 接收者安全，便于在未启用指标的部署中零成本关闭。
package metrics

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder 是事件链路上报指标的接口，由 Registry 实现。
//
// 中间件层只依赖该接口，测试可注入假实现。
type Recorder interface {
	// EventHandled 记录一次事件处理结果与耗时，err 非 nil 时 result 记为 error。
	EventHandled(plugin, rule string, d time.Duration, err error)
	// EventPublished 记录一次事件发布。
	EventPublished(platform string)
	// EventDropped 记录一次事件丢弃及其原因。
	EventDropped(platform, reason string)
	// MessageSent 记录一次消息发送的结果与耗时。
	MessageSent(platform string, d time.Duration, err error)
	// RuleMatched 记录一次规则命中。
	RuleMatched(plugin, rule string)
	// AdapterReconnected 记录一次外部适配器重连成功。
	AdapterReconnected(name string)
	// AdapterReconnectFailed 记录一次外部适配器重连失败。
	AdapterReconnectFailed(name string)
	// AdapterDisabled 记录一次外部适配器实例被停用（重连失败达上限）。
	AdapterDisabled(name string)
}

// 指标名。
const (
	metricEventsPublished  = "kei_events_published_total"
	metricEventsDropped    = "kei_events_dropped_total"
	metricEventsHandled    = "kei_events_handled_total"
	metricEventHandling    = "kei_event_handling_seconds"
	metricMessagesSent     = "kei_messages_sent_total"
	metricMessageSend      = "kei_message_send_seconds"
	metricRulesMatched     = "kei_rules_matched_total"
	metricAdapterReconnect = "kei_adapter_reconnects_total"
	metricAdapterDisabled  = "kei_adapter_disabled_total"
)

// 指标帮助文本。
const (
	helpEventsPublished  = "已发布的事件总数"
	helpEventsDropped    = "被丢弃的事件总数"
	helpEventsHandled    = "已处理的事件总数"
	helpEventHandling    = "事件处理耗时（秒）"
	helpMessagesSent     = "已发送的消息总数"
	helpMessageSend      = "消息发送耗时（秒）"
	helpRulesMatched     = "规则命中总数"
	helpAdapterReconnect = "外部适配器重连次数（result 为 ok 或 error）"
	helpAdapterDisabled  = "被停用的外部适配器实例数（重连失败达上限）"
)

// 结果标签取值。
const (
	resultOK    = "ok"
	resultError = "error"
)

// Prometheus 指标类型名。
const (
	typeCounter   = "counter"
	typeHistogram = "histogram"
)

// defaultBuckets 是耗时直方图的默认桶边界（单位秒），覆盖毫秒级到秒级。
var defaultBuckets = []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

// counter 是并发安全的单调递增计数器。
type counter struct{ v atomic.Uint64 }

// inc 计数加一。
func (c *counter) inc() { c.v.Add(1) }

// value 返回当前计数值。
func (c *counter) value() uint64 { return c.v.Load() }

// histogram 是并发安全的固定桶直方图，桶计数与总和/总量分开保护。
type histogram struct {
	counts  []atomic.Uint64 // 与 buckets 一一对应，不含 +Inf
	buckets []float64

	mu    sync.Mutex
	sum   float64
	total uint64
}

// newHistogram 按给定桶边界构造直方图。
func newHistogram(buckets []float64) *histogram {
	return &histogram{
		counts:  make([]atomic.Uint64, len(buckets)),
		buckets: buckets,
	}
}

// observe 记录一次观测值，使用互斥保护 sum/total 以支持 -race 校验。
func (h *histogram) observe(v float64) {
	// 二分定位首个 v <= bucket 的桶；超出上界时只累加 +Inf。
	i := sort.SearchFloat64s(h.buckets, v)
	if i < len(h.counts) {
		h.counts[i].Add(1)
	}
	h.mu.Lock()
	h.sum += v
	h.total++
	h.mu.Unlock()
}

// snapshot 返回桶上界、各桶累计计数、总和与观测总数。
func (h *histogram) snapshot() (buckets []float64, cumulative []uint64, sum float64, total uint64) {
	cumulative = make([]uint64, len(h.counts))
	var running uint64
	for i := range h.counts {
		running += h.counts[i].Load()
		cumulative[i] = running
	}
	h.mu.Lock()
	sum, total = h.sum, h.total
	h.mu.Unlock()
	return h.buckets, cumulative, sum, total
}

// label 是一个 Prometheus 标签键值对。
type label struct{ name, value string }

// escapeLabelValue 转义标签值中的反斜杠、双引号与换行。
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		// 常规值直接返回，避免无谓分配。
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := range len(s) {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// formatLabels 把标签渲染为 {k="v",...} 形式，顺序由调用方保证（字典序）。
func formatLabels(labels []label) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(l.value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// formatFloat 按 Prometheus 文本格式输出浮点数。
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// writeLine 向 builder 写入一行样本。
func writeLine(b *strings.Builder, name string, labels []label, value string) {
	b.WriteString(name)
	b.WriteString(formatLabels(labels))
	b.WriteByte(' ')
	b.WriteString(value)
	b.WriteByte('\n')
}
