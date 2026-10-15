package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RandomLemon/kei/pkg/bot"
)

// metricsText 抓取引擎指标端点的 Prometheus 文本输出。
func metricsText(t *testing.T, eng *Engine) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	eng.met.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics 抓取返回状态 %d", rec.Code)
	}
	return rec.Body.String()
}

// assertMetricLine 断言指标文本按行精确包含某条样本。
func assertMetricLine(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want+"\n") {
		t.Fatalf("指标输出缺少行 %q\n输出:\n%s", want, got)
	}
}

// TestEventSinkRepeatedEventIsDroppedNotPublished 验证重复事件不再计入 published：
// 同一 ID 投递两次后只发布 1 条，另 1 条记为 dropped{reason="duplicate"}，
// 且重复投递对外仍是成功（返回 nil）。
func TestEventSinkRepeatedEventIsDroppedNotPublished(t *testing.T) {
	eng := newTestEngine(t, Options{})
	sink := &eventSink{eng: eng}
	ev := &bot.Event{ID: "dup-1", Type: bot.EventMessage, Platform: "mock", BotID: "mock-main"}

	for i := range 2 {
		if err := sink.Emit(context.Background(), ev); err != nil {
			t.Fatalf("第 %d 次 Emit: %v", i+1, err)
		}
	}

	got := metricsText(t, eng)
	assertMetricLine(t, got, `kei_events_published_total{platform="mock"} 1`)
	assertMetricLine(t, got, `kei_events_dropped_total{platform="mock",reason="duplicate"} 1`)
}

// TestEventSinkNormalizesUnknownPlatformLabel 验证上报的未知平台被归一化到
// platform="unknown"：两个不同的伪造平台落在同一条序列上、其取值不出现在指标
// 文本中，而已装配适配器的平台名保持原标签。
func TestEventSinkNormalizesUnknownPlatformLabel(t *testing.T) {
	eng := newTestEngine(t, Options{})
	sink := &eventSink{eng: eng}

	for i, platform := range []string{"spoof-a", "spoof-b"} {
		ev := &bot.Event{
			ID:       fmt.Sprintf("spoof-%d", i),
			Type:     bot.EventMessage,
			Platform: platform,
			BotID:    "mock-main",
		}
		if err := sink.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit(%s): %v", platform, err)
		}
	}
	// 已装配平台（mock 实例的 Name()）必须保持原标签。
	known := &bot.Event{ID: "known-1", Type: bot.EventMessage, Platform: "mock", BotID: "mock-main"}
	if err := sink.Emit(context.Background(), known); err != nil {
		t.Fatalf("Emit(mock): %v", err)
	}

	got := metricsText(t, eng)
	if strings.Contains(got, "spoof-a") || strings.Contains(got, "spoof-b") {
		t.Fatalf("未归一化的平台值出现在指标标签中:\n%s", got)
	}
	assertMetricLine(t, got, `kei_events_published_total{platform="unknown"} 2`)
	assertMetricLine(t, got, `kei_events_published_total{platform="mock"} 1`)
}
