package mock

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

type recordingSink struct {
	events chan *bot.Event
}

func (s *recordingSink) Emit(_ context.Context, ev *bot.Event) error {
	s.events <- ev
	return nil
}

func startAdapter(t *testing.T, opts Options) (*Adapter, *recordingSink, func()) {
	t.Helper()
	ad := New(opts)
	sink := &recordingSink{events: make(chan *bot.Event, 16)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ad.Start(ctx, sink) }()

	deadline := time.Now().Add(2 * time.Second)
	for !ad.Started() {
		if time.Now().After(deadline) {
			t.Fatal("适配器未在超时内启动")
		}
		time.Sleep(time.Millisecond)
	}

	stop := func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start 返回错误: %v", err)
		}
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
		defer cancelShutdown()
		if err := ad.Stop(shutdownCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}
	return ad, sink, stop
}

func TestInjectRequiresRunningAdapter(t *testing.T) {
	ad := New(Options{Name: "mock-main"})
	if _, err := ad.InjectText(context.Background(), "/echo hi"); err == nil {
		t.Fatal("未启动时注入应返回错误")
	}
	if err := ad.Inject(context.Background(), nil); err == nil {
		t.Fatal("nil 事件应返回错误")
	}
}

func TestInjectFillsDefaults(t *testing.T) {
	ad, sink, stop := startAdapter(t, Options{Name: "mock-main", Platform: "mock"})
	defer stop()

	if _, err := ad.InjectText(context.Background(), "/echo hello"); err != nil {
		t.Fatalf("InjectText: %v", err)
	}

	select {
	case ev := <-sink.events:
		if ev.Platform != "mock" || ev.BotID != "mock-main" {
			t.Fatalf("Platform/BotID = %q/%q", ev.Platform, ev.BotID)
		}
		if ev.Type != bot.EventMessage || ev.ID == "" || ev.Time.IsZero() {
			t.Fatalf("事件默认字段未填充: %+v", ev)
		}
		if ev.Sender == nil || ev.Sender.ID == "" {
			t.Fatalf("Sender 未填充: %+v", ev.Sender)
		}
		if ev.Message == nil || ev.Message.Kind != bot.MessageGroup || ev.Channel == nil {
			t.Fatalf("Message/Channel 未填充: %+v", ev)
		}
		if got := ev.Text(); got != "/echo hello" {
			t.Fatalf("Text = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到注入的事件")
	}
}

func TestSendRecordsAndWaitSent(t *testing.T) {
	ad, _, stop := startAdapter(t, Options{Name: "mock-main"})
	defer stop()

	res, err := ad.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: "mock", ChannelID: "g1"},
		Message: &bot.Message{Kind: bot.MessageGroup},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID == "" {
		t.Fatal("SendResult.MessageID 不应为空")
	}
	if !ad.WaitSent(1, time.Second) {
		t.Fatal("WaitSent 未观察到发送记录")
	}
	sent := ad.Sent()
	if len(sent) != 1 || sent[0].Target.ChannelID != "g1" {
		t.Fatalf("Sent = %+v", sent)
	}
	if _, err := ad.Send(context.Background(), nil); err == nil {
		t.Fatal("nil 请求应返回错误")
	}

	ad.Reset()
	if len(ad.Sent()) != 0 {
		t.Fatal("Reset 后不应有记录")
	}
}

func TestControlPlane(t *testing.T) {
	ad, sink, stop := startAdapter(t, Options{Name: "mock-main", Platform: "mock", ListenAddr: "127.0.0.1:0"})
	defer stop()

	base := "http://" + ad.Addr()
	if ad.Addr() == "" {
		t.Fatal("启用控制面后 Addr 不应为空")
	}

	resp, err := http.Post(base+"/inject", "application/json", strings.NewReader(`{"text":"/echo hi","user_id":"u9","kind":"private","bot_id":"mock-alt","platform":"mock"}`))
	if err != nil {
		t.Fatalf("POST /inject: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /inject 状态 = %d, want 202", resp.StatusCode)
	}
	select {
	case ev := <-sink.events:
		if ev.Sender.ID != "u9" || ev.Message.Kind != bot.MessagePrivate {
			t.Fatalf("注入事件与请求不符: %+v", ev)
		}
		// 显式给出的 bot_id 必须覆盖默认值，便于多 bot 场景手工联调。
		if ev.BotID != "mock-alt" || ev.Platform != "mock" {
			t.Fatalf("Platform/BotID = %q/%q, want mock/mock-alt", ev.Platform, ev.BotID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("控制面未投递事件")
	}

	if _, err := http.Post(base+"/inject", "application/json", strings.NewReader("{")); err != nil {
		t.Fatalf("非法 JSON 请求失败: %v", err)
	} else if resp, err := http.Post(base+"/inject", "application/json", strings.NewReader("{")); err == nil {
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("非法 JSON 状态 = %d, want 400", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	if _, err := ad.Send(context.Background(), &bot.SendRequest{Message: &bot.Message{}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sentResp, err := http.Get(base + "/sent")
	if err != nil {
		t.Fatalf("GET /sent: %v", err)
	}
	defer func() { _ = sentResp.Body.Close() }()
	var records []SendRecord
	if err := json.NewDecoder(sentResp.Body).Decode(&records); err != nil {
		t.Fatalf("解析 /sent: %v", err)
	}
	if len(records) != 1 || records[0].Err != "" {
		t.Fatalf("/sent = %+v", records)
	}

	req, err := http.NewRequest(http.MethodDelete, base+"/sent", nil)
	if err != nil {
		t.Fatalf("构造 DELETE: %v", err)
	}
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /sent: %v", err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /sent 状态 = %d, want 204", delResp.StatusCode)
	}
	if len(ad.Sent()) != 0 {
		t.Fatal("DELETE /sent 后不应有记录")
	}

	health, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = health.Body.Close() }()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d", health.StatusCode)
	}
}

func TestControlPlaneWithoutListener(t *testing.T) {
	ad, _, stop := startAdapter(t, Options{Name: "mock-main"})
	defer stop()
	if ad.Addr() != "" {
		t.Fatalf("未配置 listen_addr 时 Addr 应为空, got %q", ad.Addr())
	}
	if err := ad.Stop(context.Background()); err != nil {
		t.Fatalf("无控制面时 Stop 应安全: %v", err)
	}
	if err := ad.Stop(context.Background()); err != nil {
		t.Fatalf("重复 Stop 应幂等: %v", err)
	}
}

func TestStopRejectsFurtherInjection(t *testing.T) {
	ad, _, stop := startAdapter(t, Options{Name: "mock-main"})
	stop()
	if _, err := ad.InjectText(context.Background(), "/echo hi"); err == nil {
		t.Fatal("停止后注入应返回错误")
	}
}

func TestCapabilitiesDefaultToFull(t *testing.T) {
	ad := New(Options{})
	caps := ad.Capabilities()
	if !caps.Text || !caps.Markdown || !caps.Image || !caps.At || !caps.Card || !caps.File || !caps.Reply || !caps.Private || !caps.Group {
		t.Fatalf("默认能力应为全能力: %+v", caps)
	}
	if ad.Name() != "mock" {
		t.Fatalf("Name = %q, want mock", ad.Name())
	}

	custom := New(Options{Capabilities: bot.Capabilities{Text: true}})
	if custom.Capabilities().Image {
		t.Fatal("显式能力不应被默认值覆盖")
	}
}

func TestSendRespectsContext(t *testing.T) {
	ad, _, stop := startAdapter(t, Options{Name: "mock-main"})
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ad.Send(ctx, &bot.SendRequest{Message: &bot.Message{}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send = %v, want context.Canceled", err)
	}
}

func TestHTTPHandlerRejectsGarbage(t *testing.T) {
	ad := New(Options{Name: "mock-main"})
	srv := httptest.NewServer(ad.handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/inject", "application/json", strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("POST /inject: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("未启动的适配器注入应返回 409, got %d", resp.StatusCode)
	}
}

// TestListenFailureKeepsAdapterRestartable 断言控制面监听失败不会把实例置为
// 运行状态：实例不能自称健康（Started），且端口释放后可用同一实例重试启动。
func TestListenFailureKeepsAdapterRestartable(t *testing.T) {
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	addr := holder.Addr().String()

	ad := New(Options{Name: "mock-main", ListenAddr: addr})
	if err := ad.Start(context.Background(), &recordingSink{events: make(chan *bot.Event, 1)}); err == nil {
		t.Fatal("地址被占用时 Start 应返回错误")
	}
	if ad.Started() {
		t.Fatal("监听失败后 Started 必须为 false")
	}
	if ad.Addr() != "" {
		t.Fatalf("监听失败后 Addr 应为空, got %q", ad.Addr())
	}
	if _, err := ad.InjectText(context.Background(), "/echo hi"); err == nil {
		t.Fatal("监听失败后注入应返回错误")
	}

	if err := holder.Close(); err != nil {
		t.Fatalf("释放占位监听: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ad.Start(ctx, &recordingSink{events: make(chan *bot.Event, 1)}) }()
	deadline := time.Now().Add(2 * time.Second)
	for !ad.Started() {
		if time.Now().After(deadline) {
			t.Fatal("释放端口后重试启动未在超时内成功")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("重试 Start 返回错误: %v", err)
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err := ad.Stop(shutdownCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestControlPlaneKindClosedSet 断言 /inject 只接受闭集内的 kind：
// 未知取值返回 400 且不投递事件，channel 等合法取值照常注入。
func TestControlPlaneKindClosedSet(t *testing.T) {
	ad, sink, stop := startAdapter(t, Options{Name: "mock-main", ListenAddr: "127.0.0.1:0"})
	defer stop()
	base := "http://" + ad.Addr()

	resp, err := http.Post(base+"/inject", "application/json", strings.NewReader(`{"text":"hi","kind":"bogus"}`))
	if err != nil {
		t.Fatalf("POST /inject: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知 kind 状态 = %d, want 400", resp.StatusCode)
	}
	select {
	case ev := <-sink.events:
		t.Fatalf("未知 kind 不应投递事件: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	resp, err = http.Post(base+"/inject", "application/json", strings.NewReader(`{"text":"hi","kind":"channel","channel_id":"c1"}`))
	if err != nil {
		t.Fatalf("POST /inject: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("channel kind 状态 = %d, want 202", resp.StatusCode)
	}
	select {
	case ev := <-sink.events:
		if ev.Message.Kind != bot.MessageChannel || ev.Channel == nil || ev.Channel.ID != "c1" {
			t.Fatalf("channel 事件与请求不符: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("控制面未投递 channel 事件")
	}
}

// TestControlPlaneAddrValidation 断言控制面默认只接受回环地址，opt-in 后放行。
func TestControlPlaneAddrValidation(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		loopback bool
	}{
		{name: "ipv4", addr: "127.0.0.1:0", loopback: true},
		{name: "ipv4-port", addr: "127.0.0.1:18080", loopback: true},
		{name: "localhost", addr: "localhost:18080", loopback: true},
		{name: "localhost-case", addr: "LOCALHOST:18080", loopback: true},
		{name: "ipv6", addr: "[::1]:0", loopback: true},
		{name: "unset", addr: "", loopback: true},
		{name: "all-interfaces", addr: ":18080"},
		{name: "any", addr: "0.0.0.0:18080"},
		{name: "ipv6-any", addr: "[::]:18080"},
		{name: "lan", addr: "192.168.1.10:18080"},
		{name: "hostname", addr: "example.com:18080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ad := New(Options{Name: "mock-main", ListenAddr: tc.addr})
			err := ad.checkControlPlaneAddr()
			if tc.loopback && err != nil {
				t.Fatalf("回环地址/未配置控制面不应被拒绝: %v", err)
			}
			if !tc.loopback && err == nil {
				t.Fatal("非回环地址默认应被拒绝")
			}

			ad = New(Options{Name: "mock-main", ListenAddr: tc.addr, AllowRemoteControlPlane: true})
			if err := ad.checkControlPlaneAddr(); err != nil {
				t.Fatalf("显式 opt-in 后不应拒绝: %v", err)
			}
		})
	}

	// Start 用同一份校验在监听前拒绝非回环地址。
	ad := New(Options{Name: "mock-main", ListenAddr: "0.0.0.0:18080"})
	err := ad.Start(context.Background(), &recordingSink{events: make(chan *bot.Event, 1)})
	if err == nil || !strings.Contains(err.Error(), "不是回环地址") {
		t.Fatalf("Start 应拒绝非回环控制面地址, got %v", err)
	}
	if ad.Started() {
		t.Fatal("Start 被拒绝后 Started 必须为 false")
	}
}
