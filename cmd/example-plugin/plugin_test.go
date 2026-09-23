package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

const (
	testToken    = "token-1"
	testGreeting = "你好"
	bufTarget    = "passthrough:///bufconn"
)

// fakeCore 是一个可编程的 BotService，记录插件发出的发送请求。
//
// bufconn 服务端在独立 goroutine 中运行，所有字段访问都必须持锁。
type fakeCore struct {
	pluginpb.UnimplementedBotServiceServer

	mu      sync.Mutex
	sends   []*pluginpb.SendRequest
	rpcErr  error
	respErr string
}

func (f *fakeCore) SendMessage(_ context.Context, req *pluginpb.SendRequest) (*pluginpb.SendResponse, error) {
	f.mu.Lock()
	f.sends = append(f.sends, req)
	rpcErr, respErr := f.rpcErr, f.respErr
	f.mu.Unlock()
	if rpcErr != nil {
		return nil, rpcErr
	}
	return &pluginpb.SendResponse{MessageId: "mid-1", Error: respErr}, nil
}

// snapshotSends 返回已收到的发送请求副本。
func (f *fakeCore) snapshotSends() []*pluginpb.SendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pluginpb.SendRequest(nil), f.sends...)
}

// newTestPlugin 用 bufconn 上挂着假核心的连接构造插件，测试结束自动回收。
func newTestPlugin(t *testing.T, core *fakeCore) *plugin {
	t.Helper()

	ln := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginpb.RegisterBotServiceServer(srv, core)
	// Serve 在 Stop 之后返回，该 goroutine 必然退出。
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = ln.Close()
	})

	conn, err := grpc.NewClient(bufTarget,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return ln.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	p, err := newPlugin(options{
		name:     "demo",
		greeting: testGreeting,
		token:    testToken,
		coreAddr: "bufconn",
		coreConn: conn,
		logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("newPlugin: %v", err)
	}
	return p
}

// testLogger 返回丢弃全部输出的日志器，避免测试噪音。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// textEvent 构造一条只含文本段的消息事件。
func textEvent(text string) *pluginpb.Event {
	data, _ := json.Marshal(map[string]string{bot.KeyText: text})
	return &pluginpb.Event{
		Id:       "evt-1",
		Type:     string(bot.EventMessage),
		Platform: "mock",
		BotId:    "bot-a",
		Message: &pluginpb.Message{
			Id:       "m-1",
			Kind:     string(bot.MessageGroup),
			Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: string(data)}},
		},
		Sender:  &pluginpb.User{Id: "u-1"},
		Channel: &pluginpb.Channel{Id: "room-1", Kind: string(bot.MessageGroup)},
	}
}

// sentText 从发送请求中还原文本内容。
func sentText(t *testing.T, req *pluginpb.SendRequest) string {
	t.Helper()
	segs := req.GetMessage().GetSegments()
	if len(segs) != 1 {
		t.Fatalf("发送消息段数 = %d，期望 1", len(segs))
	}
	var data struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(segs[0].GetDataJson()), &data); err != nil {
		t.Fatalf("data_json %q 解析失败: %v", segs[0].GetDataJson(), err)
	}
	return data.Text
}

func TestNewPluginValidatesOptions(t *testing.T) {
	core := &fakeCore{}
	valid := options{name: "demo", greeting: "hi", token: testToken, coreConn: mustConn(t, core)}

	cases := []struct {
		name string
		opts options
	}{
		{name: "缺少 name", opts: options{greeting: "hi", token: testToken, coreConn: valid.coreConn}},
		{name: "缺少 token", opts: options{name: "demo", greeting: "hi", coreConn: valid.coreConn}},
		{name: "缺少 coreConn", opts: options{name: "demo", greeting: "hi", token: testToken}},
		{name: "缺少 greeting", opts: options{name: "demo", token: testToken, coreConn: valid.coreConn}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newPlugin(tc.opts); err == nil {
				t.Fatal("期望返回错误")
			}
		})
	}
	if _, err := newPlugin(valid); err != nil {
		t.Fatalf("合法参数应成功: %v", err)
	}
}

// mustConn 为只做校验的测试提供一个可用的核心连接。
func mustConn(t *testing.T, core *fakeCore) *grpc.ClientConn {
	t.Helper()
	ln := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	pluginpb.RegisterBotServiceServer(srv, core)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = ln.Close()
	})
	conn, err := grpc.NewClient(bufTarget,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return ln.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestInitValidatesToken(t *testing.T) {
	p := newTestPlugin(t, &fakeCore{})

	resp, err := p.Init(context.Background(), &pluginpb.InitRequest{Token: "wrong"})
	if err != nil {
		t.Fatalf("Init 不应返回 RPC 错误: %v", err)
	}
	if resp.GetOk() || resp.GetError() != "bad token" {
		t.Fatalf("InitResponse = %+v，期望 ok=false 且 error=bad token", resp)
	}

	resp, err = p.Init(context.Background(), &pluginpb.InitRequest{
		Token:       testToken,
		ConfigJson:  `{"greeting":"hi"}`,
		Permissions: []string{string(bot.PermSendMessage)},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !resp.GetOk() {
		t.Fatalf("InitResponse = %+v，期望 ok=true", resp)
	}
}

func TestInitRejectsMalformedConfig(t *testing.T) {
	p := newTestPlugin(t, &fakeCore{})

	resp, err := p.Init(context.Background(), &pluginpb.InitRequest{Token: testToken, ConfigJson: "not-json"})
	if err != nil {
		t.Fatalf("Init 不应返回 RPC 错误: %v", err)
	}
	if resp.GetOk() || resp.GetError() == "" {
		t.Fatalf("InitResponse = %+v，期望 ok=false 且带原因", resp)
	}
}

func TestInitAcceptsEmptyConfig(t *testing.T) {
	p := newTestPlugin(t, &fakeCore{})

	// 空 config_json 表示「没有配置」，必须被接受而不是当成解析失败。
	resp, err := p.Init(context.Background(), &pluginpb.InitRequest{Token: testToken})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !resp.GetOk() {
		t.Fatalf("InitResponse = %+v，期望 ok=true", resp)
	}
}

func TestHandleEventRepliesToCommand(t *testing.T) {
	core := &fakeCore{}
	p := newTestPlugin(t, core)

	res, err := p.HandleEvent(context.Background(), textEvent("/ext hi"))
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if !res.GetHandled() || res.GetError() != "" {
		t.Fatalf("HandleResult = %+v，期望 handled=true 且无错误", res)
	}

	sends := core.snapshotSends()
	if len(sends) != 1 {
		t.Fatalf("发送次数 = %d，期望 1", len(sends))
	}
	req := sends[0]
	if got := sentText(t, req); got != testGreeting+": hi" {
		t.Fatalf("回复文本 = %q，期望 %q", got, testGreeting+": hi")
	}
	if req.GetToken() != testToken {
		t.Fatalf("token = %q，期望 %q", req.GetToken(), testToken)
	}
	if req.GetReplyTo() != "" {
		t.Fatalf("reply_to = %q，期望留空", req.GetReplyTo())
	}
	target := req.GetTarget()
	if target.GetPlatform() != "mock" || target.GetChannelId() != "room-1" ||
		target.GetUserId() != "u-1" || target.GetKind() != string(bot.MessageGroup) {
		t.Fatalf("target = %+v，期望从事件回填", target)
	}
}

func TestHandleEventRepliesToKeyword(t *testing.T) {
	core := &fakeCore{}
	p := newTestPlugin(t, core)

	res, err := p.HandleEvent(context.Background(), textEvent("我是 external 插件"))
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if !res.GetHandled() {
		t.Fatalf("HandleResult = %+v，期望 handled=true", res)
	}
	sends := core.snapshotSends()
	if len(sends) != 1 {
		t.Fatalf("发送次数 = %d，期望 1", len(sends))
	}
	if got := sentText(t, sends[0]); got != testGreeting+": 我是 external 插件" {
		t.Fatalf("回复文本 = %q", got)
	}
}

func TestHandleEventIgnoresUnmatchedEvents(t *testing.T) {
	cases := []struct {
		name string
		ev   *pluginpb.Event
	}{
		{name: "无关文本", ev: textEvent("随便说说")},
		{name: "仅命令前缀无内容", ev: textEvent("/ext")},
		{name: "非消息事件", ev: &pluginpb.Event{Id: "e", Type: string(bot.EventNotice)}},
		{name: "空文本消息", ev: textEvent("   ")},
		{name: "混合段消息", ev: mixedEvent()},
		{name: "无段消息", ev: &pluginpb.Event{Id: "e", Type: string(bot.EventMessage), Message: &pluginpb.Message{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core := &fakeCore{}
			p := newTestPlugin(t, core)

			res, err := p.HandleEvent(context.Background(), tc.ev)
			if err != nil {
				t.Fatalf("HandleEvent: %v", err)
			}
			if res.GetHandled() {
				t.Fatalf("HandleResult = %+v，期望 handled=false", res)
			}
			// 未命中时必须完全不打扰核心，否则等于对外产生噪声。
			if got := len(core.snapshotSends()); got != 0 {
				t.Fatalf("未命中事件不应发送消息，实际发送 %d 次", got)
			}
		})
	}
}

// mixedEvent 构造一条含图片段的消息事件。
func mixedEvent() *pluginpb.Event {
	ev := textEvent("/ext hi")
	ev.Message.Segments = append(ev.Message.Segments,
		&pluginpb.Segment{Type: string(bot.SegImage), DataJson: `{"url":"https://example.com/a.png"}`})
	return ev
}

func TestHandleEventReportsSendFailures(t *testing.T) {
	cases := []struct {
		name      string
		core      *fakeCore
		errSubstr string
	}{
		{
			name:      "核心返回业务错误",
			core:      &fakeCore{respErr: "平台拒绝发送"},
			errSubstr: "平台拒绝发送",
		},
		{
			name:      "RPC 失败",
			core:      &fakeCore{rpcErr: status.Error(codes.Unauthenticated, "未认证的插件令牌")},
			errSubstr: "Unauthenticated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPlugin(t, tc.core)

			res, err := p.HandleEvent(context.Background(), textEvent("/ext hi"))
			if err != nil {
				t.Fatalf("HandleEvent 不应返回 RPC 错误，错误应落在 HandleResult: %v", err)
			}
			// 事件已被本插件消费（handled=true），只是回复没发出去。
			if !res.GetHandled() {
				t.Fatalf("HandleResult = %+v，期望 handled=true", res)
			}
			if !contains(res.GetError(), tc.errSubstr) {
				t.Fatalf("HandleResult.Error = %q，期望包含 %q", res.GetError(), tc.errSubstr)
			}
		})
	}
}

func TestHandleEventUsesSenderWhenChannelMissing(t *testing.T) {
	core := &fakeCore{}
	p := newTestPlugin(t, core)

	ev := textEvent("/ext hi")
	ev.Channel = nil
	ev.Message.Kind = string(bot.MessagePrivate)
	ev.Sender = &pluginpb.User{Id: "u-9"}

	if _, err := p.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	target := core.snapshotSends()[0].GetTarget()
	if target.GetUserId() != "u-9" || target.GetChannelId() != "" || target.GetKind() != string(bot.MessagePrivate) {
		t.Fatalf("target = %+v，期望私聊回填 user_id 且 channel_id 为空", target)
	}
}

func TestShutdownReturnsEmpty(t *testing.T) {
	p := newTestPlugin(t, &fakeCore{})

	resp, err := p.Shutdown(context.Background(), &pluginpb.Empty{})
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if resp == nil {
		t.Fatal("Shutdown 应返回非 nil 应答")
	}
}

func TestReplyContentRules(t *testing.T) {
	p := newTestPlugin(t, &fakeCore{})

	cases := []struct {
		text    string
		want    string
		matched bool
	}{
		{text: "/ext 你好", want: "你好", matched: true},
		{text: "/ext   多空格  ", want: "多空格", matched: true},
		{text: "/ext", matched: false},
		{text: "/ext   ", matched: false},
		{text: "contains external here", want: "contains external here", matched: true},
		// 前缀优先于关键词：/ext 后的内容才是回复体，而不是整段原文。
		{text: "/ext external", want: "external", matched: true},
		{text: "external", want: "external", matched: true},
		{text: "nothing", matched: false},
	}
	for _, tc := range cases {
		got, ok := p.replyContent(tc.text)
		if ok != tc.matched || got != tc.want {
			t.Fatalf("replyContent(%q) = (%q, %v)，期望 (%q, %v)", tc.text, got, ok, tc.want, tc.matched)
		}
	}
}

func TestRequiredFlagsReportsMissingInOrder(t *testing.T) {
	missing := missingFlags(
		flagValue{"-listen", ""},
		flagValue{"-core-addr", "127.0.0.1:1"},
		flagValue{"-token", ""},
	)
	if len(missing) != 2 || missing[0] != "-listen" || missing[1] != "-token" {
		t.Fatalf("missingFlags = %v，期望 [-listen -token]", missing)
	}
}

func TestCoreCredentialsRejectsPartialTLS(t *testing.T) {
	if _, err := coreCredentials("ca.pem", "", ""); err == nil {
		t.Fatal("只提供部分 TLS 参数时应返回错误")
	}
	creds, err := coreCredentials("", "", "")
	if err != nil {
		t.Fatalf("未提供 TLS 参数时应返回明文凭据: %v", err)
	}
	if creds.Info().SecurityProtocol != "insecure" {
		t.Fatalf("SecurityProtocol = %q，期望 insecure", creds.Info().SecurityProtocol)
	}
}

// contains 判断子串是否出现。
func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
