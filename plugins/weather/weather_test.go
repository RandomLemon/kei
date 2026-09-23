package weather

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// memStorage 是测试用的最小 bot.Storage 实现，避免插件测试依赖 internal/。
type memStorage struct {
	mu    sync.Mutex
	items map[string]item
}

type item struct {
	value   []byte
	expires time.Time
}

func newMemStorage() *memStorage { return &memStorage{items: map[string]item{}} }

func (m *memStorage) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[key]
	if !ok || (!it.expires.IsZero() && !it.expires.After(time.Now())) {
		return nil, bot.ErrNotFound
	}
	return it.value, nil
}

func (m *memStorage) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := item{value: value}
	if ttl > 0 {
		it.expires = time.Now().Add(ttl)
	}
	m.items[key] = it
	return nil
}

func (m *memStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}

func (m *memStorage) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.items))
	for k := range m.items {
		out = append(out, k)
	}
	return out
}

type harness struct {
	reg     *bot.RecordingRegistrar
	store   *memStorage
	geocode *httptest.Server
	weather *httptest.Server

	mu      sync.Mutex
	headers http.Header
	geoHits int
}

// header 返回上游请求携带的响应头快照（并发安全）。
func (h *harness) header(key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.headers.Get(key)
}

// geocodeHits 返回地理编码接口被调用的次数。
func (h *harness) geocodeHits() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.geoHits
}

func (h *harness) close() {
	h.geocode.Close()
	h.weather.Close()
}

func newHarness(t *testing.T, cfg map[string]any, weatherHandler http.HandlerFunc) *harness {
	t.Helper()

	h := &harness{reg: bot.NewRecordingRegistrar(), store: newMemStorage(), headers: http.Header{}}

	h.geocode = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.geoHits++
		h.headers = r.Header.Clone()
		h.mu.Unlock()

		if r.URL.Query().Get("name") == "unknown" {
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{
				"name": "北京", "admin1": "北京市", "country": "中国",
				"latitude": 39.9, "longitude": 116.4,
			}},
		})
	}))
	h.weather = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if weatherHandler != nil {
			weatherHandler(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"current": map[string]any{
				"time": "2026-09-23T22:00", "temperature_2m": 12.3,
				"weather_code": 0, "wind_speed_10m": 5.4,
			},
		})
	}))

	merged := map[string]any{
		"geocode_url":  h.geocode.URL,
		"forecast_url": h.weather.URL,
	}
	for k, v := range cfg {
		merged[k] = v
	}

	pc := bot.PluginContext{
		Name:       "weather",
		Config:     bot.NewConfig(merged),
		Logger:     slog.New(slog.DiscardHandler),
		Storage:    h.store,
		HTTPClient: h.geocode.Client(),
	}
	ctx := bot.WithPluginContext(context.Background(), pc)
	if err := (&Plugin{}).Setup(ctx, h.reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return h
}

func textEvent(text string, command *bot.Command) *bot.Event {
	ev := &bot.Event{
		Type:    bot.EventMessage,
		ID:      "e1",
		Sender:  &bot.User{ID: "u1"},
		Command: command,
		Message: &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
		}},
		Channel: &bot.Channel{ID: "g1", Kind: bot.MessageGroup},
	}
	return ev
}

func call(t *testing.T, rule *bot.Rule, ctx context.Context, ev *bot.Event) string {
	t.Helper()
	reply := bot.NewNoopReply()
	if err := rule.Handler(ctx, ev, reply); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return reply.PlainText()
}

func TestWeatherCommandReplies(t *testing.T) {
	h := newHarness(t, nil, nil)
	defer h.close()

	rule, ok := h.reg.Command("weather")
	if !ok {
		t.Fatal("未注册 /weather 命令")
	}
	got := call(t, rule, context.Background(), textEvent("/weather 北京", &bot.Command{Name: "weather", Args: []string{"北京"}}))

	if !strings.Contains(got, "北京") || !strings.Contains(got, "12.3°C") || !strings.Contains(got, "晴") {
		t.Fatalf("回复 = %q", got)
	}
	if !strings.Contains(got, "2026-09-23 22:00 UTC") {
		t.Fatalf("回复缺少观测时间: %q", got)
	}
}

func TestWeatherRegexTrigger(t *testing.T) {
	h := newHarness(t, nil, nil)
	defer h.close()

	rule, ok := h.reg.Find(func(r *bot.Rule) bool { return r.Regex != nil })
	if !ok {
		t.Fatal("未注册正则触发规则")
	}
	if !rule.Regex.MatchString("天气 北京") {
		t.Fatalf("正则 %s 应匹配「天气 北京」", rule.Regex)
	}

	ctx := bot.WithRoute(context.Background(), &bot.Route{RegexMatches: []string{"天气 北京", "北京"}})
	if got := call(t, rule, ctx, textEvent("天气 北京", nil)); !strings.Contains(got, "12.3°C") {
		t.Fatalf("正则触发回复 = %q", got)
	}
}

func TestWeatherMultiTurn(t *testing.T) {
	h := newHarness(t, nil, nil)
	defer h.close()

	command, _ := h.reg.Command("weather")
	followUp, ok := h.reg.Find(func(r *bot.Rule) bool { return r.Command == "" && r.Regex == nil && r.Keywords == nil })
	if !ok {
		t.Fatal("未注册多轮对话兜底规则")
	}

	got := call(t, command, context.Background(), textEvent("/weather", &bot.Command{Name: "weather"}))
	if !strings.Contains(got, "城市") {
		t.Fatalf("缺少追问回复: %q", got)
	}
	if len(h.store.keys()) != 1 {
		t.Fatalf("应写入待输入状态, keys = %v", h.store.keys())
	}

	// 下一条普通消息被当作城市名。
	got = call(t, followUp, context.Background(), textEvent("北京", nil))
	if !strings.Contains(got, "12.3°C") {
		t.Fatalf("多轮对话回复 = %q", got)
	}
	if len(h.store.keys()) != 0 {
		t.Fatalf("回复后应清除待输入状态, keys = %v", h.store.keys())
	}
}

func TestWeatherFollowUpIgnoresUnrelatedMessages(t *testing.T) {
	h := newHarness(t, nil, nil)
	defer h.close()

	followUp, _ := h.reg.Find(func(r *bot.Rule) bool { return r.Command == "" && r.Regex == nil && r.Keywords == nil })

	// 没有待输入状态：保持沉默且不报错。
	if got := call(t, followUp, context.Background(), textEvent("今天吃什么", nil)); got != "" {
		t.Fatalf("不应回复无关消息, got %q", got)
	}
	// 命令消息由各自规则处理，兜底规则跳过。
	if got := call(t, followUp, context.Background(), textEvent("/echo hi", &bot.Command{Name: "echo", Args: []string{"hi"}})); got != "" {
		t.Fatalf("不应回复命令消息, got %q", got)
	}
	// 空文本跳过。
	if got := call(t, followUp, context.Background(), textEvent("   ", nil)); got != "" {
		t.Fatalf("空文本不应回复, got %q", got)
	}
}

func TestWeatherUnknownCity(t *testing.T) {
	h := newHarness(t, nil, nil)
	defer h.close()

	rule, _ := h.reg.Command("weather")
	got := call(t, rule, context.Background(), textEvent("/weather unknown", &bot.Command{Name: "weather", Args: []string{"unknown"}}))
	if !strings.Contains(got, "未找到城市") {
		t.Fatalf("回复 = %q", got)
	}
}

func TestWeatherUpstreamFailure(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer h.close()

	rule, _ := h.reg.Command("weather")
	got := call(t, rule, context.Background(), textEvent("/weather 北京", &bot.Command{Name: "weather", Args: []string{"北京"}}))
	if !strings.Contains(got, "查询失败") || !strings.Contains(got, "500") {
		t.Fatalf("回复 = %q", got)
	}
}

func TestWeatherAPIKeyHeader(t *testing.T) {
	h := newHarness(t, map[string]any{
		"api_key":        "secret",
		"api_key_header": "X-Token",
		"api_key_prefix": "Token ",
	}, nil)
	defer h.close()

	rule, _ := h.reg.Command("weather")
	call(t, rule, context.Background(), textEvent("/weather 北京", &bot.Command{Name: "weather", Args: []string{"北京"}}))

	if got := h.header("X-Token"); got != "Token secret" {
		t.Fatalf("鉴权头 = %q, want %q", got, "Token secret")
	}
}

func TestWeatherSetupRequiresContext(t *testing.T) {
	p := &Plugin{}
	if err := p.Setup(context.Background(), bot.NewRecordingRegistrar()); err == nil {
		t.Fatal("缺少 PluginContext 时应返回错误")
	}

	pc := bot.PluginContext{Name: "weather", Config: bot.NewConfig(nil), Storage: newMemStorage()}
	ctx := bot.WithPluginContext(context.Background(), pc)
	if err := p.Setup(ctx, bot.NewRecordingRegistrar()); err == nil {
		t.Fatal("缺少 HTTPClient（未声明 network 权限）时应返回错误")
	}
}

func TestMetadataDeclaresRequiredPermissions(t *testing.T) {
	meta := (&Plugin{}).Metadata()
	if !meta.HasPermission(bot.PermNetwork) || !meta.HasPermission(bot.PermStorage) {
		t.Fatalf("weather 需要 network 与 storage 权限: %+v", meta.Permissions)
	}
}

func TestWeatherCodeText(t *testing.T) {
	if got := weatherText(95); got != "雷阵雨" {
		t.Fatalf("weatherText(95) = %q", got)
	}
	if got := weatherText(1234); !strings.Contains(got, "1234") {
		t.Fatalf("未知天气码应回显原始值: %q", got)
	}
}

func TestGeocodeHitsUpstreamOnce(t *testing.T) {
	h := newHarness(t, nil, nil)
	defer h.close()

	rule, _ := h.reg.Command("weather")
	call(t, rule, context.Background(), textEvent("/weather 北京", &bot.Command{Name: "weather", Args: []string{"北京"}}))
	if got := h.geocodeHits(); got != 1 {
		t.Fatalf("地理编码请求次数 = %d, want 1", got)
	}
}

func TestPendingStateStorageError(t *testing.T) {
	p := &Plugin{}
	pc := bot.PluginContext{
		Name:       "weather",
		Config:     bot.NewConfig(nil),
		Logger:     slog.New(slog.DiscardHandler),
		Storage:    failingStorage{},
		HTTPClient: http.DefaultClient,
	}
	reg := bot.NewRecordingRegistrar()
	if err := p.Setup(bot.WithPluginContext(context.Background(), pc), reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	rule, _ := reg.Command("weather")

	reply := bot.NewNoopReply()
	err := rule.Handler(context.Background(), textEvent("/weather", &bot.Command{Name: "weather"}), reply)
	if err == nil {
		t.Fatal("存储失败时应返回错误")
	}
}

type failingStorage struct{}

func (failingStorage) Get(context.Context, string) ([]byte, error) { return nil, errors.New("boom") }
func (failingStorage) Set(context.Context, string, []byte, time.Duration) error {
	return errors.New("boom")
}
func (failingStorage) Delete(context.Context, string) error { return errors.New("boom") }
