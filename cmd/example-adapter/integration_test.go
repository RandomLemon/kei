package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/RandomLemon/kei/internal/adaptermgr/external"
	"github.com/RandomLemon/kei/internal/grpcsrv"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

const (
	coreToken   = "change-me"
	noPermToken = "no-perm"
)

// fakeBot 是核心侧 BotAPI 的测试实现：记录核心发出的消息。
type fakeBot struct {
	mu   sync.Mutex
	sent []*bot.SendRequest
}

func (f *fakeBot) Send(_ context.Context, target bot.Target, msg *bot.Message) (*bot.SendResult, error) {
	f.mu.Lock()
	f.sent = append(f.sent, &bot.SendRequest{Target: target, Message: msg})
	f.mu.Unlock()
	return &bot.SendResult{MessageID: "core-1"}, nil
}

func (f *fakeBot) Reply(ctx context.Context, ev *bot.Event, msg *bot.Message) (*bot.SendResult, error) {
	return f.Send(ctx, bot.TargetFromEvent(ev), msg)
}

func (f *fakeBot) Logger() *slog.Logger { return slog.New(slog.DiscardHandler) }
func (f *fakeBot) Storage() bot.Storage { return storage.NewMemory() }

// nopSink 满足 bot.EventSink：外部适配器的事件经 EmitEvent 上行，不经 sink。
type nopSink struct{}

func (nopSink) Emit(context.Context, *bot.Event) error { return nil }

// harness 持有核心侧服务、示例适配器服务与核心侧客户端。
type harness struct {
	core        *grpcsrv.Server
	coreBot     *fakeBot
	svc         *service
	client      *external.Client
	controlAddr string
	botAddr     string
	events      *eventRecorder
}

// eventRecorder 记录核心侧 EmitEvent 钩子收到的事件。
type eventRecorder struct {
	mu    sync.Mutex
	items []*bot.Event
}

func (r *eventRecorder) emit(_ context.Context, adapter string, ev *bot.Event) error {
	r.mu.Lock()
	r.items = append(r.items, ev)
	r.mu.Unlock()
	return nil
}

func (r *eventRecorder) snapshot() []*bot.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*bot.Event(nil), r.items...)
}

// startCore 启动核心侧 BotService，并注册正常令牌与缺 receive_event 的令牌。
func startCore(t *testing.T, recorder *eventRecorder) (*grpcsrv.Server, *fakeBot) {
	t.Helper()
	fb := &fakeBot{}
	core, err := grpcsrv.New(grpcsrv.Options{
		Addr: "127.0.0.1:0",
		Tokens: map[string]grpcsrv.TokenInfo{
			coreToken: {Name: "example", Kind: grpcsrv.TokenAdapter,
				Permissions: []bot.Permission{bot.PermReceiveEvent, bot.PermNetwork, bot.PermNetListen}},
			noPermToken: {Name: "example-noperm", Kind: grpcsrv.TokenAdapter,
				Permissions: []bot.Permission{bot.PermNetwork}},
		},
		Bot:       fb,
		EmitEvent: recorder.emit,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("grpcsrv.New: %v", err)
	}
	if err := core.Start(); err != nil {
		t.Fatalf("core.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := core.Stop(ctx); err != nil {
			t.Errorf("core.Stop: %v", err)
		}
	})
	return core, fb
}

// startAdapter 在随机端口启动示例适配器进程内服务，返回其 AdapterService 地址。
func startAdapter(t *testing.T, platform string) (*service, string, string) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	svc := newService(logger, platform, "127.0.0.1:0")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pluginpb.RegisterAdapterServiceServer(srv, svc)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	controlAddr, err := svc.startControl(logger)
	if err != nil {
		t.Fatalf("startControl: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := svc.Shutdown(ctx, &pluginpb.Empty{}); err != nil {
			t.Errorf("svc.Shutdown: %v", err)
		}
	})
	return svc, ln.Addr().String(), controlAddr
}

// dialAdapter 通过核心侧客户端连接示例适配器（等价 cmd/bot 的装配路径）。
func dialAdapter(t *testing.T, h *harness, adapterName, token string) {
	t.Helper()
	svc, botAddr, controlAddr := startAdapter(t, "example")
	client, err := external.New(context.Background(), external.Config{
		Name:        adapterName,
		Addr:        botAddr,
		CoreAddr:    h.core.Addr(),
		Token:       token,
		Permissions: []bot.Permission{bot.PermReceiveEvent, bot.PermNetwork, bot.PermNetListen},
		Settings:    map[string]any{bot.OptListenAddr: "127.0.0.1:0"},
	}, external.Deps{})
	if err != nil {
		t.Fatalf("external.New: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("client.Close: %v", err)
		}
	})
	h.svc, h.client, h.botAddr, h.controlAddr = svc, client, botAddr, controlAddr
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	recorder := &eventRecorder{}
	h := &harness{events: recorder}
	h.core, h.coreBot = startCore(t, recorder)
	dialAdapter(t, h, "example", coreToken)
	return h
}

// startInstance 启动一个 bot 实例并等待 Start RPC 生效。
func startInstance(t *testing.T, h *harness, botID string) (bot.Adapter, context.CancelFunc, chan error) {
	t.Helper()
	inst, err := h.client.Instance(botID, bot.NewConfig(map[string]any{bot.OptListenAddr: "127.0.0.1:0"}))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- inst.Start(ctx, nopSink{}) }()

	deadline := time.Now().Add(3 * time.Second)
	for !inst.Capabilities().Text {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("实例未在超时内启动")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return inst, cancel, done
}

// postInject 向控制面注入一条文本事件。
func postInject(t *testing.T, controlAddr, botID, text string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"bot_id":%q,"text":%q}`, botID, text)
	resp, err := http.Post("http://"+controlAddr+"/inject", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /inject: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// getSent 查询控制面的发送记录。
func getSent(t *testing.T, controlAddr, botID string) []sendRecord {
	t.Helper()
	resp, err := http.Get("http://" + controlAddr + "/sent?bot_id=" + botID)
	if err != nil {
		t.Fatalf("GET /sent: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sent 状态码 = %d", resp.StatusCode)
	}
	var out []sendRecord
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析 /sent: %v", err)
	}
	return out
}

func TestExternalAdapterEndToEnd(t *testing.T) {
	h := newHarness(t)
	inst, cancel, done := startInstance(t, h, "example-main")

	// 发送：核心 -> AdapterService.Send，适配器记录并返回消息 ID。
	res, err := inst.Send(context.Background(), &bot.SendRequest{
		BotID: "example-main",
		Target: bot.Target{
			Platform: "example", BotID: "example-main", ChannelID: "example-group", Kind: bot.MessageGroup,
		},
		Message: &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi"}},
		}},
		ReplyTo: "m0",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.HasPrefix(res.MessageID, "example-") {
		t.Fatalf("消息 ID = %q", res.MessageID)
	}
	// 发送必须经适配器进程，核心侧的 BotAPI 不应被用到。
	h.coreBot.mu.Lock()
	coreSends := len(h.coreBot.sent)
	h.coreBot.mu.Unlock()
	if coreSends != 0 {
		t.Fatalf("核心 BotAPI 不应收到发送请求: %d", coreSends)
	}

	sent := getSent(t, h.controlAddr, "example-main")
	if len(sent) != 1 {
		t.Fatalf("发送记录 = %+v", sent)
	}
	if sent[0].Target.GetChannelId() != "example-group" || sent[0].ReplyTo != "m0" {
		t.Fatalf("发送记录 = %+v", sent[0])
	}
	if got := sent[0].Message.GetSegments()[0].GetDataJson(); !strings.Contains(got, "hi") {
		t.Fatalf("发送内容 = %q", got)
	}

	// 上行：平台事件 -> EmitEvent -> 核心钩子。
	status, body := postInject(t, h.controlAddr, "example-main", "/echo hi from adapter")
	if status != http.StatusAccepted {
		t.Fatalf("POST /inject 状态码 = %d, body = %s", status, body)
	}
	var ack struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil || ack.EventID == "" {
		t.Fatalf("注入应答 = %s", body)
	}

	deadline := time.Now().Add(3 * time.Second)
	for len(h.events.snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("核心未收到事件")
		}
		time.Sleep(2 * time.Millisecond)
	}
	ev := h.events.snapshot()[0]
	if ev.ID != ack.EventID || ev.Platform != "example" || ev.BotID != "example-main" {
		t.Fatalf("事件 = %+v", ev)
	}
	if ev.Sender == nil || ev.Sender.ID != "example-user" {
		t.Fatalf("事件发送者 = %+v", ev.Sender)
	}
	if got := ev.Text(); got != "/echo hi from adapter" {
		t.Fatalf("事件文本 = %q", got)
	}
	if ev.Channel == nil || ev.Channel.Kind != bot.MessageGroup {
		t.Fatalf("事件会话 = %+v", ev.Channel)
	}

	// 未知 bot：控制面拒绝，且不产生事件。
	before := len(h.events.snapshot())
	if status, _ := postInject(t, h.controlAddr, "unknown-bot", "hi"); status != http.StatusBadRequest {
		t.Fatalf("未知 bot 状态码 = %d", status)
	}
	if got := len(h.events.snapshot()); got != before {
		t.Fatalf("未知 bot 不应产生事件: %d -> %d", before, got)
	}

	// 关闭：Start 退出会下发 Stop，Close 再发 Shutdown。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start 退出: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start 未在超时内退出")
	}
	if _, ok := h.svc.instance("example-main"); ok {
		t.Fatal("实例应已从适配器进程移除")
	}
	if err := h.client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h.svc.mu.Lock()
	shutdown := h.svc.shutdown
	h.svc.mu.Unlock()
	if !shutdown {
		t.Fatal("适配器应已收到 Shutdown")
	}
}

func TestExternalAdapterRejectsMissingReceiveEventPermission(t *testing.T) {
	h := newHarness(t)

	// 换用缺 receive_event 的令牌：核心必须拒绝事件，控制面转成 502。
	dialAdapter(t, h, "example-noperm", noPermToken)
	_, cancel, done := startInstance(t, h, "example-noperm-main")
	defer func() {
		cancel()
		<-done
	}()

	status, body := postInject(t, h.controlAddr, "example-noperm-main", "hi")
	if status != http.StatusBadGateway {
		t.Fatalf("缺少 receive_event 时状态码 = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "receive_event") {
		t.Fatalf("错误信息应包含权限名: %s", body)
	}
	if got := len(h.events.snapshot()); got != 0 {
		t.Fatalf("被拒事件不应进入核心: %d", got)
	}
}

func TestExternalAdapterRejectsInvalidInit(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	svc := newService(logger, "example", "127.0.0.1:0")

	if resp, err := svc.Init(context.Background(), &pluginpb.AdapterInitRequest{CoreAddr: "127.0.0.1:1"}); err != nil ||
		resp.GetOk() || !strings.Contains(resp.GetError(), "token") {
		t.Fatalf("缺少 token 的 Init = %+v, %v", resp, err)
	}
	if resp, err := svc.Init(context.Background(), &pluginpb.AdapterInitRequest{Token: "t"}); err != nil ||
		resp.GetOk() || !strings.Contains(resp.GetError(), "core_addr") {
		t.Fatalf("缺少 core_addr 的 Init = %+v, %v", resp, err)
	}
	if resp, err := svc.Init(context.Background(), &pluginpb.AdapterInitRequest{
		Token: "t", CoreAddr: "127.0.0.1:1", ConfigJson: "{",
	}); err != nil || resp.GetOk() {
		t.Fatalf("非法配置 JSON 的 Init = %+v, %v", resp, err)
	}

	// Send 到未知实例按业务失败上报，不返回 RPC 错误。
	resp, err := svc.Send(context.Background(), &pluginpb.AdapterSendRequest{BotId: "missing"})
	if err != nil || resp.GetError() == "" {
		t.Fatalf("未知 bot 的 Send = %+v, %v", resp, err)
	}
	if _, err := svc.Stop(context.Background(), &pluginpb.AdapterStopRequest{BotId: "missing"}); err != nil {
		t.Fatalf("Stop 未知实例不应报错: %v", err)
	}
}

func TestExternalAdapterStartIsIdempotent(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	svc := newService(logger, "example", "127.0.0.1:0")
	if _, err := svc.Init(context.Background(), &pluginpb.AdapterInitRequest{Token: "t", CoreAddr: "127.0.0.1:1"}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	first, err := svc.Start(context.Background(), &pluginpb.AdapterInstance{BotId: "b1"})
	if err != nil || !first.GetOk() {
		t.Fatalf("Start = %+v, %v", first, err)
	}
	second, err := svc.Start(context.Background(), &pluginpb.AdapterInstance{BotId: "b1"})
	if err != nil || !second.GetOk() {
		t.Fatalf("重复 Start = %+v, %v", second, err)
	}
	if first.GetPlatform() != "example" || second.GetPlatform() != "example" {
		t.Fatalf("平台名 = %q / %q", first.GetPlatform(), second.GetPlatform())
	}
	if !second.GetCapabilities().GetText() {
		t.Fatal("应声明文本能力")
	}

	// 未知实例不会让实现崩溃。
	if _, err := svc.Stop(context.Background(), &pluginpb.AdapterStopRequest{BotId: "b1"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := svc.Stop(context.Background(), &pluginpb.AdapterStopRequest{BotId: "b1"}); err != nil {
		t.Fatalf("重复 Stop: %v", err)
	}
	if _, ok := svc.instance("b1"); ok {
		t.Fatal("实例应已移除")
	}

	if _, err := svc.Shutdown(context.Background(), &pluginpb.Empty{}); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if resp, err := svc.Start(context.Background(), &pluginpb.AdapterInstance{BotId: "b2"}); err != nil || resp.GetOk() {
		t.Fatalf("关闭后 Start 应失败: %+v, %v", resp, err)
	}
	if resp, err := svc.Init(context.Background(), &pluginpb.AdapterInitRequest{Token: "t", CoreAddr: "127.0.0.1:1"}); err != nil ||
		resp.GetOk() || !strings.Contains(resp.GetError(), "已关闭") {
		t.Fatalf("关闭后 Init 应失败: %+v, %v", resp, err)
	}
}
