package onebot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/pkg/message"
)

const groupArrayEvent = `{
  "time": 1700000000,
  "self_id": 10000,
  "post_type": "message",
  "message_type": "group",
  "sub_type": "normal",
  "message_id": 12345,
  "user_id": 20001,
  "group_id": 30001,
  "raw_message": "hello ",
  "message": [
    {"type": "text", "data": {"text": "hello "}},
    {"type": "at", "data": {"qq": "10001", "name": "Alice"}},
    {"type": "image", "data": {"url": "http://example.com/a.png", "file": "a.png"}},
    {"type": "face", "data": {"id": "4"}},
    {"type": "reply", "data": {"id": "999"}},
    {"type": "file", "data": {"file": "file:///tmp/a.txt", "name": "a.txt"}},
    {"type": "mystery", "data": {"x": 1}}
  ],
  "sender": {"user_id": 20001, "nickname": "bob", "card": "群名片"}
}`

const privateCQEvent = `{
  "time": 1700000001,
  "self_id": 10000,
  "post_type": "message",
  "message_type": "private",
  "message_id": "12346",
  "user_id": 20002,
  "message": "你好 world",
  "sender": {"user_id": 20002, "nickname": "carol"}
}`

var testClient = &http.Client{Timeout: 5 * time.Second}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sinkRecorder 记录被投递的事件，供断言使用。
type sinkRecorder struct {
	mu     sync.Mutex
	events []*bot.Event
	ch     chan *bot.Event
}

func newSinkRecorder() *sinkRecorder {
	return &sinkRecorder{ch: make(chan *bot.Event, 16)}
}

func (s *sinkRecorder) Emit(_ context.Context, ev *bot.Event) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	select {
	case s.ch <- ev:
	default:
	}
	return nil
}

func (s *sinkRecorder) next(t *testing.T) *bot.Event {
	t.Helper()
	select {
	case ev := <-s.ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("等待事件超时")
		return nil
	}
}

// blockedSink 在 Emit 中阻塞，用于验证适配器先回响应再投递事件。
type blockedSink struct {
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
	events  []*bot.Event
}

func newBlockedSink() *blockedSink {
	return &blockedSink{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (s *blockedSink) Emit(_ context.Context, ev *bot.Event) error {
	s.entered <- struct{}{}
	<-s.release
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	return nil
}

func (s *blockedSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// testHarness 是一次测试用的适配器运行实例。
type testHarness struct {
	adapter *Adapter
	sink    *sinkRecorder
	base    string
}

// startHarness 以 127.0.0.1:0 启动适配器，返回可用的基地址。
func startHarness(t *testing.T, opts Options) *testHarness {
	t.Helper()
	if opts.Name == "" {
		opts.Name = "bot1"
	}
	opts.ListenAddr = "127.0.0.1:0"
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 3 * time.Second}
	}

	a, err := New(opts)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sink := newSinkRecorder()
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx, sink) }()

	base := waitForAddr(t, a)

	t.Cleanup(func() {
		_ = a.Stop(context.Background())
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Start 返回错误: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Start 未在超时内退出")
		}
	})

	return &testHarness{adapter: a, sink: sink, base: base}
}

// waitForAddr 轮询 Start 写入的监听地址。
func waitForAddr(t *testing.T, a *Adapter) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := a.Addr(); addr != "" {
			return "http://" + addr
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("等待监听地址超时")
	return ""
}

// postJSON 发送一次上报请求。
func postJSON(t *testing.T, url, body string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s 失败: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestNewValidation(t *testing.T) {
	base := Options{Name: "bot1", APIURL: "http://127.0.0.1:3000", ListenAddr: "127.0.0.1:0"}

	cases := map[string]Options{
		"缺少 Name":          {APIURL: base.APIURL, ListenAddr: base.ListenAddr},
		"Name 仅空白":         {Name: "  ", APIURL: base.APIURL, ListenAddr: base.ListenAddr},
		"缺少 ListenAddr":    {Name: base.Name, APIURL: base.APIURL},
		"APIURL 非法":        {Name: base.Name, APIURL: "http://[::1:3000", ListenAddr: base.ListenAddr},
		"Path 不以斜杠开头":      {Name: base.Name, APIURL: base.APIURL, ListenAddr: base.ListenAddr, Path: "onebot/event"},
		"WSPath 不以斜杠开头":    {Name: base.Name, APIURL: base.APIURL, ListenAddr: base.ListenAddr, WSPath: "onebot/ws"},
		"WSPath 与 Path 相同": {Name: base.Name, APIURL: base.APIURL, ListenAddr: base.ListenAddr, Path: "/onebot/event", WSPath: "/onebot/event"},
	}
	for name, opts := range cases {
		if _, err := New(opts); err == nil {
			t.Errorf("%s: 期望返回错误，实际为 nil", name)
		}
	}

	a, err := New(base)
	if err != nil {
		t.Fatalf("合法配置 New 失败: %v", err)
	}
	if got := a.Name(); got != "onebot" {
		t.Errorf("Name() = %q, 期望 %q", got, "onebot")
	}
	if got := a.Addr(); got != "" {
		t.Errorf("Start 之前 Addr() = %q, 期望空字符串", got)
	}
	if a.path != defaultPath {
		t.Errorf("默认 Path = %q, 期望 %q", a.path, defaultPath)
	}
	if got := a.WSPath(); got != defaultWSPath {
		t.Errorf("默认 WSPath = %q, 期望 %q", got, defaultWSPath)
	}
	if a.log == nil || a.client == nil || a.hub == nil {
		t.Error("默认 Logger/HTTPClient/wsHub 未填充")
	}
	if got := a.hub.pingInterval; got != defaultPingInterval {
		t.Errorf("默认心跳间隔 = %v, 期望 %v", got, defaultPingInterval)
	}

	// APIURL 可选：只跑反向 WebSocket 时不配置 HTTP API。
	wsOnly, err := New(Options{Name: base.Name, ListenAddr: base.ListenAddr, WSPath: "/custom/ws", PingInterval: -time.Second})
	if err != nil {
		t.Fatalf("不配置 APIURL 时 New 失败: %v", err)
	}
	if got := wsOnly.WSPath(); got != "/custom/ws" {
		t.Errorf("WSPath() = %q, 期望 %q", got, "/custom/ws")
	}
	if got := wsOnly.hub.pingInterval; got != -time.Second {
		t.Errorf("负值心跳间隔应原样保留，得到 %v", got)
	}
}

func TestCapabilities(t *testing.T) {
	a, err := New(Options{Name: "bot1", APIURL: "http://127.0.0.1:3000", ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	caps := a.Capabilities()
	if !caps.Text || !caps.Image || !caps.At || !caps.File || !caps.Reply || !caps.Private || !caps.Group {
		t.Errorf("缺少能力: %+v", caps)
	}
	if caps.Markdown || caps.Card {
		t.Errorf("不应声明 Markdown/Card 能力: %+v", caps)
	}
	if !caps.Supports(bot.SegFace) {
		t.Error("表情段应由 Text 能力支撑")
	}
	if caps.Supports(bot.SegMarkdown) || caps.Supports(bot.SegCard) {
		t.Error("不应支持 Markdown/卡片段")
	}
	if got := (bot.Capabilities{Text: true}); !got.Supports(bot.SegFace) {
		t.Error("Face 依赖 Text 的约定被破坏")
	}
}

func TestGroupMessageArrayEvent(t *testing.T) {
	h := startHarness(t, Options{})

	resp := postJSON(t, h.base+defaultPath, groupArrayEvent, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204", resp.StatusCode)
	}

	ev := h.sink.next(t)
	if ev.ID != "12345" {
		t.Errorf("ID = %q, 期望 %q", ev.ID, "12345")
	}
	if ev.Type != bot.EventMessage {
		t.Errorf("Type = %q, 期望 %q", ev.Type, bot.EventMessage)
	}
	if ev.Platform != "onebot" {
		t.Errorf("Platform = %q, 期望 %q", ev.Platform, "onebot")
	}
	if ev.BotID != "bot1" {
		t.Errorf("BotID = %q, 期望 %q", ev.BotID, "bot1")
	}
	want := time.Unix(1700000000, 0).UTC()
	if !ev.Time.Equal(want) || ev.Time.Location() != time.UTC {
		t.Errorf("Time = %v, 期望 %v (UTC)", ev.Time, want)
	}
	if ev.Command != nil {
		t.Errorf("适配器不应解析命令，实际 = %+v", ev.Command)
	}

	if ev.Sender == nil || ev.Sender.ID != "20001" || ev.Sender.Name != "群名片" || ev.Sender.IsBot {
		t.Errorf("Sender = %+v, 期望 id=20001 name=群名片 isBot=false", ev.Sender)
	}
	if ev.Channel == nil || ev.Channel.ID != "30001" || ev.Channel.Kind != bot.MessageGroup {
		t.Errorf("Channel = %+v, 期望 group 30001", ev.Channel)
	}

	msg := ev.Message
	if msg == nil || msg.ID != "12345" || msg.Kind != bot.MessageGroup {
		t.Fatalf("Message = %+v, 期望 group 消息且 ID=12345", msg)
	}
	if len(msg.Segments) != 6 {
		t.Fatalf("段数 = %d, 期望 6（未知段应被跳过）: %+v", len(msg.Segments), msg.Segments)
	}

	checkSegment(t, msg.Segments[0], bot.SegText, map[string]any{bot.KeyText: "hello "})
	checkSegment(t, msg.Segments[1], bot.SegAt, map[string]any{bot.KeyUserID: "10001", bot.KeyUserName: "Alice"})
	checkSegment(t, msg.Segments[2], bot.SegImage, map[string]any{bot.KeyURL: "http://example.com/a.png", bot.KeyFile: "a.png"})
	checkSegment(t, msg.Segments[3], bot.SegFace, map[string]any{bot.KeyFaceID: "4"})
	checkSegment(t, msg.Segments[4], bot.SegReply, map[string]any{bot.KeyMessageID: "999"})
	checkSegment(t, msg.Segments[5], bot.SegFile, map[string]any{bot.KeyFile: "file:///tmp/a.txt", bot.KeyFileName: "a.txt"})

	if got := ev.Text(); got != "hello " {
		t.Errorf("Text() = %q, 期望 %q", got, "hello ")
	}

	raw, ok := ev.Raw.(json.RawMessage)
	if !ok || !json.Valid(raw) {
		t.Fatalf("Raw 类型 = %T, 期望合法的 json.RawMessage", ev.Raw)
	}
	var meta struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil || meta.MessageID != 12345 {
		t.Errorf("Raw 内容不正确: %s (err=%v)", raw, err)
	}
}

func checkSegment(t *testing.T, seg bot.Segment, wantType bot.SegmentType, wantData map[string]any) {
	t.Helper()
	if seg.Type != wantType {
		t.Errorf("段类型 = %q, 期望 %q", seg.Type, wantType)
		return
	}
	for k, want := range wantData {
		if got := seg.Data[k]; got != want {
			t.Errorf("%s 段 %s = %v, 期望 %v", wantType, k, got, want)
		}
	}
	if len(seg.Data) != len(wantData) {
		t.Errorf("%s 段数据键数 = %d, 期望 %d: %+v", wantType, len(seg.Data), len(wantData), seg.Data)
	}
}

func TestPrivateCQStringEvent(t *testing.T) {
	h := startHarness(t, Options{})

	resp := postJSON(t, h.base+defaultPath, privateCQEvent, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204", resp.StatusCode)
	}

	ev := h.sink.next(t)
	if ev.ID != "12346" || ev.Type != bot.EventMessage {
		t.Errorf("ID/Type = %q/%q, 期望 12346/message", ev.ID, ev.Type)
	}
	if ev.Channel != nil {
		t.Errorf("私聊事件 Channel 应为 nil, 实际 = %+v", ev.Channel)
	}
	msg := ev.Message
	if msg == nil || msg.Kind != bot.MessagePrivate {
		t.Fatalf("Message = %+v, 期望私聊消息", msg)
	}
	if len(msg.Segments) != 1 {
		t.Fatalf("段数 = %d, 期望 1（CQ 字符串整体作为文本段）: %+v", len(msg.Segments), msg.Segments)
	}
	checkSegment(t, msg.Segments[0], bot.SegText, map[string]any{bot.KeyText: "你好 world"})
	if ev.Sender == nil || ev.Sender.ID != "20002" || ev.Sender.Name != "carol" {
		t.Errorf("Sender = %+v, 期望 id=20002 name=carol", ev.Sender)
	}
}

func TestEventTypesAndDeterministicIDs(t *testing.T) {
	h := startHarness(t, Options{SelfID: "10000"})

	cases := []struct {
		name  string
		body  string
		wantT bot.EventType
		wantI string
	}{
		{
			name:  "notice 使用确定性拼接 ID",
			body:  `{"time":1700000002,"self_id":10000,"post_type":"notice","notice_type":"group_recall","sub_type":"","group_id":30001,"user_id":20001}`,
			wantT: bot.EventNotice,
			wantI: "10000-notice-1700000002-group_recall-30001-20001",
		},
		{
			name:  "request 使用确定性拼接 ID",
			body:  `{"time":1700000003,"self_id":10000,"post_type":"request","request_type":"friend","user_id":20001}`,
			wantT: bot.EventRequest,
			wantI: "10000-request-1700000003-friend-20001",
		},
		{
			name:  "meta_event 使用确定性拼接 ID",
			body:  `{"time":1700000004,"self_id":10000,"post_type":"meta_event","meta_event_type":"heartbeat","sub_type":"heartbeat"}`,
			wantT: bot.EventMeta,
			wantI: "10000-meta_event-1700000004-heartbeat-heartbeat",
		},
		{
			name:  "消息缺少 message_id 时回退到拼接 ID",
			body:  `{"time":1700000005,"self_id":10000,"post_type":"message","message_type":"private","user_id":20001,"message":"hi","sender":{"user_id":20001,"nickname":"x"}}`,
			wantT: bot.EventMessage,
			wantI: "10000-message-1700000005-20001",
		},
		{
			name:  "缺少 self_id 时使用 Options.SelfID",
			body:  `{"time":1700000006,"post_type":"meta_event","meta_event_type":"lifecycle"}`,
			wantT: bot.EventMeta,
			wantI: "10000-meta_event-1700000006-lifecycle",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postJSON(t, h.base+defaultPath, tc.body, nil)
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("状态码 = %d, 期望 204", resp.StatusCode)
			}
			ev := h.sink.next(t)
			if ev.Type != tc.wantT {
				t.Errorf("Type = %q, 期望 %q", ev.Type, tc.wantT)
			}
			if ev.ID != tc.wantI {
				t.Errorf("ID = %q, 期望 %q", ev.ID, tc.wantI)
			}
			if ev.Type != bot.EventMessage && ev.Message != nil {
				t.Errorf("非消息事件 Message 应为 nil, 实际 = %+v", ev.Message)
			}
			if ev.Platform != "onebot" || ev.BotID != "bot1" {
				t.Errorf("Platform/BotID = %q/%q", ev.Platform, ev.BotID)
			}
		})
	}

	// 同一事件重复上报必须得到相同 ID，供上游去重。
	dup := `{"time":1700000007,"self_id":10000,"post_type":"notice","notice_type":"poke","group_id":30001,"user_id":20001}`
	first := postJSON(t, h.base+defaultPath, dup, nil)
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204", first.StatusCode)
	}
	e1 := h.sink.next(t)
	second := postJSON(t, h.base+defaultPath, dup, nil)
	if second.StatusCode != http.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204", second.StatusCode)
	}
	e2 := h.sink.next(t)
	if e1.ID != e2.ID {
		t.Errorf("重复上报的 ID 不一致: %q vs %q", e1.ID, e2.ID)
	}
}

func TestAuthMethodAndMalformedRequest(t *testing.T) {
	h := startHarness(t, Options{Secret: "s3cr3t"})
	url := h.base + defaultPath

	t.Run("GET 不被接受", func(t *testing.T) {
		resp, err := testClient.Get(url)
		if err != nil {
			t.Fatalf("GET 失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("状态码 = %d, 期望 405", resp.StatusCode)
		}
	})

	t.Run("缺失令牌返回 401", func(t *testing.T) {
		if got := postJSON(t, url, groupArrayEvent, nil).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("状态码 = %d, 期望 401", got)
		}
	})

	t.Run("错误 Bearer 令牌返回 401", func(t *testing.T) {
		resp := postJSON(t, url, groupArrayEvent, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer nope")
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("状态码 = %d, 期望 401", resp.StatusCode)
		}
	})

	t.Run("错误 access_token 返回 401", func(t *testing.T) {
		if got := postJSON(t, url+"?access_token=nope", groupArrayEvent, nil).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("状态码 = %d, 期望 401", got)
		}
	})

	t.Run("Bearer 令牌通过", func(t *testing.T) {
		resp := postJSON(t, url, groupArrayEvent, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer s3cr3t")
		})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("状态码 = %d, 期望 204", resp.StatusCode)
		}
		if ev := h.sink.next(t); ev.ID != "12345" {
			t.Errorf("ID = %q, 期望 12345", ev.ID)
		}
	})

	t.Run("access_token 查询参数通过", func(t *testing.T) {
		resp := postJSON(t, url+"?access_token=s3cr3t", groupArrayEvent, nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("状态码 = %d, 期望 204", resp.StatusCode)
		}
		h.sink.next(t)
	})

	t.Run("Bearer 错误但 query 正确仍通过", func(t *testing.T) {
		resp := postJSON(t, url+"?access_token=s3cr3t", groupArrayEvent, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer nope")
		})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("状态码 = %d, 期望 204", resp.StatusCode)
		}
		h.sink.next(t)
	})

	t.Run("非法 JSON 返回 400", func(t *testing.T) {
		resp := postJSON(t, url, `{"post_type":`, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer s3cr3t")
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
		}
	})

	t.Run("未知 post_type 返回 400", func(t *testing.T) {
		resp := postJSON(t, url, `{"time":1,"post_type":"weird"}`, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer s3cr3t")
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
		}
	})

	t.Run("message 字段类型错误返回 400", func(t *testing.T) {
		body := `{"time":1,"self_id":1,"post_type":"message","message_type":"private","message":42}`
		resp := postJSON(t, url, body, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer s3cr3t")
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
		}
	})

	t.Run("失败请求不投递事件", func(t *testing.T) {
		h.sink.mu.Lock()
		n := len(h.sink.events)
		h.sink.mu.Unlock()
		if n != 3 {
			t.Errorf("已投递事件数 = %d, 期望 3（仅三次鉴权通过的请求）", n)
		}
	})
}

func TestResponseBeforeEmit(t *testing.T) {
	opts := Options{Name: "bot1", APIURL: "http://127.0.0.1:1", ListenAddr: "127.0.0.1:0", Logger: quietLogger()}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := newBlockedSink()
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx, sink) }()
	base := waitForAddr(t, a)

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := testClient.Post(base+defaultPath, "application/json", strings.NewReader(groupArrayEvent))
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	select {
	case <-sink.entered:
	case err := <-errCh:
		t.Fatalf("上报失败: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("sink 未收到事件")
	}

	// sink.Emit 仍阻塞在 release 上；响应必须已经返回，证明先 ACK 后投递。
	select {
	case resp := <-respCh:
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("状态码 = %d, 期望 204", resp.StatusCode)
		}
		_ = resp.Body.Close()
	case err := <-errCh:
		t.Fatalf("上报失败: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Emit 阻塞期间未收到响应，ACK 不及时")
	}

	close(sink.release)
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop 失败: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start 未退出")
	}
	if sink.count() != 1 {
		t.Errorf("投递事件数 = %d, 期望 1", sink.count())
	}
}

// apiCall 记录伪造 API 服务器收到的一次调用。
type apiCall struct {
	action    string
	params    map[string]any
	echo      string
	auth      string
	bodyBytes int
}

// newFakeAPI 启动伪造的 OneBot HTTP API，返回调用记录与响应体内容。
func newFakeAPI(t *testing.T, respBody string, status int) (*httptest.Server, *apiCall) {
	t.Helper()
	call := &apiCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体失败: %v", err)
			return
		}
		call.bodyBytes = len(body)
		call.auth = r.Header.Get("Authorization")
		var req struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
			Echo   string         `json:"echo"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("解析请求体失败: %v (%s)", err, body)
			return
		}
		call.action, call.params, call.echo = req.Action, req.Params, req.Echo
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, call
}

// messageParams 取出请求中的 message 段列表。
func messageParams(t *testing.T, call *apiCall) []onebotSegment {
	t.Helper()
	raw, ok := call.params["message"]
	if !ok {
		t.Fatalf("params 缺少 message 字段: %+v", call.params)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("重新编码 message 失败: %v", err)
	}
	var segs []onebotSegment
	if err := json.Unmarshal(encoded, &segs); err != nil {
		t.Fatalf("message 不是合法的段数组: %v (%s)", err, encoded)
	}
	return segs
}

func TestSendGroupWithDegrade(t *testing.T) {
	api, call := newFakeAPI(t, `{"status":"ok","retcode":0,"data":{"message_id":123}}`, http.StatusOK)
	h := startHarness(t, Options{APIURL: api.URL, AccessToken: "tok"})

	msg := &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
		message.Markdown("# 标题"),
		message.Text(" 正文"),
		message.Image("http://example.com/i.png"),
		message.At("10001"),
		message.Face("4"),
		message.Reply("999"),
		message.Card(map[string]any{"k": "v"}),
	}}
	res, err := h.adapter.Send(context.Background(), &bot.SendRequest{Target: bot.Target{ChannelID: "30001"}, Message: msg})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	if call.action != actionSendGroup {
		t.Errorf("action = %q, 期望 %q", call.action, actionSendGroup)
	}
	if got := call.params["group_id"]; got != "30001" {
		t.Errorf("group_id = %v, 期望 30001", got)
	}
	if _, ok := call.params["user_id"]; ok {
		t.Errorf("群聊请求不应带 user_id: %+v", call.params)
	}
	if call.auth != "Bearer tok" {
		t.Errorf("Authorization = %q, 期望 %q", call.auth, "Bearer tok")
	}
	if len(call.echo) != 36 || strings.Count(call.echo, "-") != 4 {
		t.Errorf("echo = %q, 期望 UUID 形式", call.echo)
	}

	segs := messageParams(t, call)
	// Markdown → 文本；Card → "[card] {…}" 文本；其余原样映射。
	want := []onebotSegment{
		{Type: "text", Data: map[string]any{"text": "# 标题"}},
		{Type: "text", Data: map[string]any{"text": " 正文"}},
		{Type: "image", Data: map[string]any{"file": "http://example.com/i.png", "url": "http://example.com/i.png"}},
		{Type: "at", Data: map[string]any{"qq": "10001"}},
		{Type: "face", Data: map[string]any{"id": "4"}},
		{Type: "reply", Data: map[string]any{"id": "999"}},
		{Type: "text", Data: map[string]any{"text": `[card] {"k":"v"}`}},
	}
	if len(segs) != len(want) {
		t.Fatalf("段数 = %d, 期望 %d: %+v", len(segs), len(want), segs)
	}
	for i := range want {
		if segs[i].Type != want[i].Type {
			t.Errorf("第 %d 段类型 = %q, 期望 %q", i, segs[i].Type, want[i].Type)
			continue
		}
		for k, v := range want[i].Data {
			if segs[i].Data[k] != v {
				t.Errorf("第 %d 段 %s = %v, 期望 %v", i, k, segs[i].Data[k], v)
			}
		}
	}

	if res.MessageID != "123" {
		t.Errorf("MessageID = %q, 期望 %q", res.MessageID, "123")
	}
	raw, ok := res.Raw.(json.RawMessage)
	if !ok || !strings.Contains(string(raw), `"retcode":0`) {
		t.Errorf("Raw = %v, 期望原始响应体", res.Raw)
	}
}

func TestSendPrivateAndTargetInference(t *testing.T) {
	t.Run("空 Kind 由 UserID 推断私聊", func(t *testing.T) {
		api, call := newFakeAPI(t, `{"status":"ok","retcode":0,"data":{"message_id":"abc"}}`, http.StatusOK)
		h := startHarness(t, Options{APIURL: api.URL})

		res, err := h.adapter.Send(context.Background(), &bot.SendRequest{
			Target:  bot.Target{UserID: "20001"},
			Message: message.Plain(bot.MessagePrivate, "hi"),
		})
		if err != nil {
			t.Fatalf("Send 失败: %v", err)
		}
		if call.action != actionSendPrivate {
			t.Errorf("action = %q, 期望 %q", call.action, actionSendPrivate)
		}
		if got := call.params["user_id"]; got != "20001" {
			t.Errorf("user_id = %v, 期望 20001", got)
		}
		if call.auth != "" {
			t.Errorf("未配置 AccessToken 时不应带 Authorization: %q", call.auth)
		}
		if res.MessageID != "abc" {
			t.Errorf("MessageID = %q, 期望 abc", res.MessageID)
		}
		if segs := messageParams(t, call); len(segs) != 1 || segs[0].Data["text"] != "hi" {
			t.Errorf("私聊消息段 = %+v", segs)
		}
	})

	t.Run("显式 Kind 优先于 ChannelID", func(t *testing.T) {
		api, call := newFakeAPI(t, `{"status":"ok","retcode":0,"data":{}}`, http.StatusOK)
		h := startHarness(t, Options{APIURL: api.URL})

		_, err := h.adapter.Send(context.Background(), &bot.SendRequest{
			Target:  bot.Target{Kind: bot.MessagePrivate, ChannelID: "30001", UserID: "20001"},
			Message: message.Plain(bot.MessagePrivate, "hi"),
		})
		if err != nil {
			t.Fatalf("Send 失败: %v", err)
		}
		if call.action != actionSendPrivate || call.params["user_id"] != "20001" {
			t.Errorf("action/params = %q/%+v, 期望私聊发送到 20001", call.action, call.params)
		}
	})

	t.Run("发送失败仍带 context", func(t *testing.T) {
		api, _ := newFakeAPI(t, `{"status":"ok","retcode":0,"data":{}}`, http.StatusOK)
		h := startHarness(t, Options{APIURL: api.URL, HTTPClient: &http.Client{Timeout: time.Second}})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := h.adapter.Send(ctx, &bot.SendRequest{
			Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
			Message: message.Plain(bot.MessageGroup, "hi"),
		})
		if err == nil {
			t.Fatal("已取消的 context 应导致错误")
		}
	})
}

func TestSendErrors(t *testing.T) {
	body := message.Plain(bot.MessageGroup, "hi")

	cases := []struct {
		name     string
		respBody string
		status   int
		wantSub  string
	}{
		{
			name:     "retcode 非零",
			respBody: `{"status":"failed","retcode":100,"wording":"参数错误","data":null}`,
			status:   http.StatusOK,
			wantSub:  "retcode=100",
		},
		{
			name:     "status 非 ok",
			respBody: `{"status":"async","retcode":1,"msg":"bad","data":null}`,
			status:   http.StatusOK,
			wantSub:  "status=async",
		},
		{
			name:     "HTTP 500",
			respBody: `internal error`,
			status:   http.StatusInternalServerError,
			wantSub:  "HTTP 500",
		},
		{
			name:     "响应不是 JSON",
			respBody: `not json`,
			status:   http.StatusOK,
			wantSub:  "解析",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, _ := newFakeAPI(t, tc.respBody, tc.status)
			h := startHarness(t, Options{APIURL: api.URL})

			_, err := h.adapter.Send(context.Background(), &bot.SendRequest{
				Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
				Message: body,
			})
			if err == nil {
				t.Fatal("期望返回错误，实际为 nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 = %q, 期望包含 %q", err.Error(), tc.wantSub)
			}
		})
	}

	t.Run("wording 出现在错误信息中", func(t *testing.T) {
		api, _ := newFakeAPI(t, `{"status":"failed","retcode":100,"wording":"参数错误"}`, http.StatusOK)
		h := startHarness(t, Options{APIURL: api.URL})
		_, err := h.adapter.Send(context.Background(), &bot.SendRequest{
			Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
			Message: body,
		})
		if err == nil || !strings.Contains(err.Error(), "参数错误") {
			t.Errorf("错误信息 = %v, 期望包含 wording", err)
		}
	})

	t.Run("非法目标", func(t *testing.T) {
		api, _ := newFakeAPI(t, `{"status":"ok","retcode":0}`, http.StatusOK)
		h := startHarness(t, Options{APIURL: api.URL})

		requests := map[string]*bot.SendRequest{
			"无目标":   {Message: body},
			"群聊缺群号": {Target: bot.Target{Kind: bot.MessageGroup}, Message: body},
			"私聊缺用户": {Target: bot.Target{Kind: bot.MessagePrivate}, Message: body},
			"消息为空":  {Target: bot.Target{Kind: bot.MessageGroup, ChannelID: "1"}},
		}
		for name, req := range requests {
			if _, err := h.adapter.Send(context.Background(), req); err == nil {
				t.Errorf("%s: 期望返回错误，实际为 nil", name)
			}
		}
		if _, err := h.adapter.Send(context.Background(), nil); err == nil {
			t.Error("nil 请求应返回错误")
		}
	})

	t.Run("API 不可达", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("监听失败: %v", err)
		}
		apiURL := "http://" + ln.Addr().String()
		_ = ln.Close()

		h := startHarness(t, Options{APIURL: apiURL})
		if _, err := h.adapter.Send(context.Background(), &bot.SendRequest{
			Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
			Message: body,
		}); err == nil {
			t.Fatal("API 不可达时应返回错误")
		}
	})
}

func TestCustomPathAndStop(t *testing.T) {
	opts := Options{
		Name:       "bot1",
		APIURL:     "http://127.0.0.1:1",
		ListenAddr: "127.0.0.1:0",
		Path:       "/custom/event",
		Logger:     quietLogger(),
		HTTPClient: &http.Client{Timeout: time.Second},
	}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := newSinkRecorder()
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx, sink) }()
	base := waitForAddr(t, a)

	if got := postJSON(t, base+"/custom/event", groupArrayEvent, nil).StatusCode; got != http.StatusNoContent {
		t.Errorf("自定义路径状态码 = %d, 期望 204", got)
	}
	sink.next(t)
	if got := postJSON(t, base+defaultPath, groupArrayEvent, nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("默认路径状态码 = %d, 期望 404", got)
	}

	host, port, err := net.SplitHostPort(a.Addr())
	if err != nil {
		t.Fatalf("Addr() = %q 不是 host:port: %v", a.Addr(), err)
	}
	if host != "127.0.0.1" || port == "0" {
		t.Errorf("Addr() = %q, 期望 127.0.0.1 上的真实端口", a.Addr())
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("首次 Stop 失败: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("重复 Stop 应返回 nil, 实际 %v", err)
	}
	if _, err := net.DialTimeout("tcp", a.Addr(), 500*time.Millisecond); err == nil {
		t.Error("Stop 之后监听端口仍可连接")
	}

	// Stop 之后 Start 必须明确失败，而不是静默挂起。
	if err := a.Start(context.Background(), sink); err == nil {
		t.Error("Stop 之后 Start 应返回错误")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start 未退出")
	}
}

func TestStartRejectsNilSink(t *testing.T) {
	a, err := New(Options{Name: "bot1", APIURL: "http://127.0.0.1:1", ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := a.Start(context.Background(), nil); err == nil {
		t.Error("nil EventSink 应返回错误")
	}
}
