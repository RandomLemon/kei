package metrics

import (
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// body 抓取 Handler 的响应体与 Content-Type。
func body(t *testing.T, r *Registry) (string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("状态码不符: got %d", rec.Code)
	}
	return rec.Body.String(), rec.Header().Get("Content-Type")
}

// TestHandlerContentType 验证 Content-Type 符合 Prometheus 文本格式约定。
func TestHandlerContentType(t *testing.T) {
	_, ct := body(t, New())
	if ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type 不符: %q", ct)
	}
}

// TestCounters 验证各计数器指标族与标签。
func TestCounters(t *testing.T) {
	r := New()
	r.EventPublished("onebot")
	r.EventPublished("onebot")
	r.EventPublished("feishu")
	r.EventDropped("onebot", "duplicate")
	r.EventDropped("onebot", "duplicate")
	r.EventDropped("onebot", "panic")
	r.EventHandled("echo", "hi", 3*time.Millisecond, nil)
	r.EventHandled("echo", "hi", 5*time.Millisecond, errors.New("炸了"))
	r.MessageSent("onebot", 10*time.Millisecond, nil)
	r.MessageSent("onebot", 20*time.Millisecond, errors.New("超时"))
	r.RuleMatched("echo", "hi")
	r.RuleMatched("echo", "hi")
	r.RuleMatched("echo", "bye")

	got, _ := body(t, r)

	wants := []string{
		"# TYPE kei_events_published_total counter",
		`kei_events_published_total{platform="feishu"} 1`,
		`kei_events_published_total{platform="onebot"} 2`,
		`kei_events_dropped_total{platform="onebot",reason="duplicate"} 2`,
		`kei_events_dropped_total{platform="onebot",reason="panic"} 1`,
		`kei_events_handled_total{plugin="echo",result="error",rule="hi"} 1`,
		`kei_events_handled_total{plugin="echo",result="ok",rule="hi"} 1`,
		`kei_messages_sent_total{platform="onebot",result="error"} 1`,
		`kei_messages_sent_total{platform="onebot",result="ok"} 1`,
		`kei_rules_matched_total{plugin="echo",rule="bye"} 1`,
		`kei_rules_matched_total{plugin="echo",rule="hi"} 2`,
		"# TYPE kei_event_handling_seconds histogram",
		"# TYPE kei_message_send_seconds histogram",
	}
	for _, w := range wants {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("输出缺少行 %q\n输出:\n%s", w, got)
		}
	}
}

// TestHistogram 验证直方图桶、+Inf、_sum 与 _count。
func TestHistogram(t *testing.T) {
	r := New()
	// 三个观测值分别落在 .005、.01（累积到前两桶）与 +Inf 桶。
	r.EventHandled("p", "r", 3*time.Millisecond, nil)
	r.EventHandled("p", "r", 8*time.Millisecond, nil)
	r.EventHandled("p", "r", 7*time.Second, nil)

	got, _ := body(t, r)
	// 标签按名字典序排列，因此 le 位于 plugin 之前。
	const prefix = `kei_event_handling_seconds{le="`

	// 桶边界按定义顺序出现，且累积计数单调不减。
	order := []string{
		prefix + `0.001",plugin="p",rule="r"} 0`,
		prefix + `0.005",plugin="p",rule="r"} 1`,
		prefix + `0.01",plugin="p",rule="r"} 2`,
		prefix + `0.025",plugin="p",rule="r"} 2`,
	}
	prev := -1
	for _, want := range order {
		idx := strings.Index(got, want+"\n")
		if idx < 0 {
			t.Fatalf("输出缺少直方图桶行 %q\n输出:\n%s", want, got)
		}
		if idx < prev {
			t.Fatalf("直方图桶顺序不符: %q", want)
		}
		prev = idx
	}

	wants := []string{
		prefix + `+Inf",plugin="p",rule="r"} 3`,
		`kei_event_handling_seconds_sum{plugin="p",rule="r"} 7.011`,
		`kei_event_handling_seconds_count{plugin="p",rule="r"} 3`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("输出缺少行 %q\n输出:\n%s", w, got)
		}
	}
}

// TestEmptyRegistry 验证无数据时仍输出全部指标族的 HELP/TYPE 头。
func TestEmptyRegistry(t *testing.T) {
	got, _ := body(t, New())
	for _, name := range []string{
		metricEventsPublished, metricEventsDropped, metricEventsHandled,
		metricEventHandling, metricMessagesSent, metricMessageSend, metricRulesMatched,
		metricAdapterReconnect, metricAdapterDisabled,
	} {
		if !strings.Contains(got, "# HELP "+name+" ") {
			t.Errorf("缺少 HELP 头: %s", name)
		}
		if !strings.Contains(got, "# TYPE "+name+" ") {
			t.Errorf("缺少 TYPE 头: %s", name)
		}
	}
}

// TestAdapterMetrics 验证外部适配器重连与停用计数器的标签与累加。
func TestAdapterMetrics(t *testing.T) {
	r := New()
	r.AdapterReconnectFailed("myim")
	r.AdapterReconnectFailed("myim")
	r.AdapterDisabled("myim")
	r.AdapterReconnected("myim")
	r.AdapterReconnectFailed("other")

	got, _ := body(t, r)
	wants := []string{
		`kei_adapter_reconnects_total{adapter="myim",result="error"} 2`,
		`kei_adapter_reconnects_total{adapter="myim",result="ok"} 1`,
		`kei_adapter_reconnects_total{adapter="other",result="error"} 1`,
		`kei_adapter_disabled_total{adapter="myim"} 1`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("输出缺少行 %q\n输出:\n%s", w, got)
		}
	}
}

// TestLabelEscaping 验证标签值中的反斜杠、引号与换行被转义。
func TestLabelEscaping(t *testing.T) {
	r := New()
	plugin := "a\\b\"c\nd"
	r.RuleMatched(plugin, "r")
	got, _ := body(t, r)

	if !strings.Contains(got, `kei_rules_matched_total{plugin="a\\b\"c\nd",rule="r"} 1`) {
		t.Fatalf("标签值未正确转义:\n%s", got)
	}
	// 输出必须保持单行结构：换行被转义，样本行本身不换行。
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasPrefix(line, "kei_rules_matched_total") &&
			!strings.HasSuffix(line, `",rule="r"} 1`) {
			t.Fatalf("标签转义破坏了样本行结构: %q", line)
		}
	}
}

// TestNilReceiver 验证所有 Recorder 方法对 nil 接收者安全。
func TestNilReceiver(t *testing.T) {
	var r *Registry
	r.EventHandled("p", "r", time.Millisecond, errors.New("x"))
	r.EventPublished("p")
	r.EventDropped("p", "reason")
	r.MessageSent("p", time.Millisecond, nil)
	r.RuleMatched("p", "r")
	// nil 接收者的 Handler 也应可安全调用。
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("nil 接收者状态码不符: %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("nil 接收者不应输出指标: %q", rec.Body.String())
	}
}

// TestInterfaceSatisfied 验证 *Registry 实现 Recorder。
func TestInterfaceSatisfied(t *testing.T) {
	var rec Recorder = New()
	rec.EventPublished("p")
	rec.EventHandled("p", "r", time.Millisecond, nil)
	rec.EventDropped("p", "r")
	rec.MessageSent("p", time.Millisecond, nil)
	rec.RuleMatched("p", "r")
}

// TestConcurrentRecording 在 -race 下验证并发写入与并发渲染无竞态。
func TestConcurrentRecording(t *testing.T) {
	r := New()
	const workers = 8
	const perWorker = 50

	var wg sync.WaitGroup
	// 写入者。
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			platform := "onebot"
			if w%2 == 0 {
				platform = "feishu"
			}
			for i := range perWorker {
				r.EventPublished(platform)
				r.EventDropped(platform, "duplicate")
				r.EventHandled("p", "r", time.Duration(i)*time.Microsecond, nil)
				r.MessageSent(platform, time.Duration(i)*time.Microsecond, nil)
				r.RuleMatched("p", "r")
				r.EventHandled(platform, "r", time.Millisecond, errors.New("x"))
			}
		}()
	}
	// 并发渲染者。
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				var b strings.Builder
				r.render(&b)
				if b.Len() == 0 {
					t.Error("并发渲染输出为空")
					return
				}
			}
		}()
	}
	wg.Wait()

	got, _ := body(t, r)
	// 每类写入 workers*perWorker 次（EventHandled 成功/失败各占一半写入）。
	wants := []string{
		`kei_events_published_total{platform="onebot"} 200`,
		`kei_events_published_total{platform="feishu"} 200`,
		`kei_events_dropped_total{platform="onebot",reason="duplicate"} 200`,
		`kei_events_handled_total{plugin="p",result="ok",rule="r"} 400`,
		`kei_rules_matched_total{plugin="p",rule="r"} 400`,
		`kei_message_send_seconds_count{platform="onebot"} 200`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("并发写入计数不符，缺少 %q", w)
		}
	}
}
