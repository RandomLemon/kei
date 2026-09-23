package external

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

const (
	testPlugin = "demo"
	testToken  = "token-demo"
	// bufTarget 是 bufconn 使用的伪地址，由 contextDialer 接管实际连接。
	bufTarget = "passthrough:///bufconn"
)

// fakePlugin 是一个可编程的 PluginService，记录核心侧发来的请求并返回预设结果。
//
// bufconn 服务端在独立 goroutine 中运行，因此所有字段访问都必须持锁。
type fakePlugin struct {
	pluginpb.UnimplementedPluginServiceServer

	mu         sync.Mutex
	initReq    *pluginpb.InitRequest
	initResp   *pluginpb.InitResponse
	initErr    error
	events     []*pluginpb.Event
	handleResp *pluginpb.HandleResult
	handleErr  error
	handleWait time.Duration
	shutdowns  int
	shutdownEr error
}

func (f *fakePlugin) Init(_ context.Context, req *pluginpb.InitRequest) (*pluginpb.InitResponse, error) {
	f.mu.Lock()
	f.initReq = req
	resp, err := f.initResp, f.initErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &pluginpb.InitResponse{Ok: true}, nil
	}
	return resp, nil
}

func (f *fakePlugin) HandleEvent(ctx context.Context, ev *pluginpb.Event) (*pluginpb.HandleResult, error) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	wait, resp, err := f.handleWait, f.handleResp, f.handleErr
	f.mu.Unlock()

	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &pluginpb.HandleResult{Handled: true}, nil
	}
	return resp, nil
}

func (f *fakePlugin) Shutdown(context.Context, *pluginpb.Empty) (*pluginpb.Empty, error) {
	f.mu.Lock()
	f.shutdowns++
	err := f.shutdownEr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &pluginpb.Empty{}, nil
}

// snapshotInit 返回最近一次 Init 请求的副本。
func (f *fakePlugin) snapshotInit() *pluginpb.InitRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.initReq
}

// snapshotEvents 返回已收到的事件副本。
func (f *fakePlugin) snapshotEvents() []*pluginpb.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pluginpb.Event(nil), f.events...)
}

// shutdownCount 返回 Shutdown 被调用的次数。
func (f *fakePlugin) shutdownCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shutdowns
}

// startBufconn 启动一个挂在 bufconn 上的 PluginService，返回客户端连接。
func startBufconn(t *testing.T, fake *fakePlugin) *grpc.ClientConn {
	t.Helper()

	ln := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, fake)
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
	return conn
}

// newTestPlugin 用 bufconn 上的假插件构造一个已初始化的 Plugin。
func newTestPlugin(t *testing.T, cfg Config, fake *fakePlugin) *Plugin {
	t.Helper()
	p, err := NewWithConn(startBufconn(t, fake), cfg, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewWithConn: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// testLogger 返回写入丢弃点的日志器，避免测试输出噪音。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// discard 是丢弃所有日志的 io.Writer。
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// baseConfig 返回一份最小可用配置，测试按需覆盖字段。
func baseConfig() Config {
	return Config{
		Name:     testPlugin,
		Addr:     "bufconn",
		CoreAddr: "127.0.0.1:9000",
		Token:    testToken,
		Timeout:  time.Second,
	}
}

func TestNewWithConnSendsInitRequest(t *testing.T) {
	fake := &fakePlugin{}
	p := newTestPlugin(t, Config{
		Name:        testPlugin,
		Addr:        "bufconn",
		CoreAddr:    "127.0.0.1:9000",
		Token:       testToken,
		Permissions: []bot.Permission{bot.PermSendMessage, bot.PermStorage},
		Settings:    map[string]any{"greeting": "hi", "retries": 3},
	}, fake)

	req := fake.snapshotInit()
	if req == nil {
		t.Fatal("Init 未被调用")
	}
	if req.GetName() != testPlugin {
		t.Fatalf("name = %q，期望 %q", req.GetName(), testPlugin)
	}
	if req.GetVersion() != "external" || req.GetAuthor() != "config" || req.GetDescription() != "gRPC 外部插件" {
		t.Fatalf("元信息 = (%q, %q, %q)", req.GetVersion(), req.GetAuthor(), req.GetDescription())
	}
	if req.GetToken() != testToken {
		t.Fatalf("token = %q，期望 %q", req.GetToken(), testToken)
	}
	if req.GetReplyServiceAddr() != "127.0.0.1:9000" {
		t.Fatalf("reply_service_addr = %q", req.GetReplyServiceAddr())
	}
	want := []string{string(bot.PermSendMessage), string(bot.PermStorage)}
	if len(req.GetPermissions()) != len(want) {
		t.Fatalf("permissions = %v，期望 %v", req.GetPermissions(), want)
	}
	for i, perm := range want {
		if req.GetPermissions()[i] != perm {
			t.Fatalf("permissions[%d] = %q，期望 %q", i, req.GetPermissions()[i], perm)
		}
	}
	// config_json 必须携带完整配置，插件据此初始化自身行为。
	var settings map[string]any
	if err := json.Unmarshal([]byte(req.GetConfigJson()), &settings); err != nil {
		t.Fatalf("config_json %q 解析失败: %v", req.GetConfigJson(), err)
	}
	if settings["greeting"] != "hi" || settings["retries"] != float64(3) {
		t.Fatalf("config_json 内容 = %v", settings)
	}

	meta := p.Metadata()
	if meta.Name != testPlugin || meta.Version != "external" || meta.Author != "config" || meta.Description != "gRPC 外部插件" {
		t.Fatalf("Metadata() = %+v", meta)
	}
	if len(meta.Permissions) != 2 || meta.Permissions[0] != bot.PermSendMessage {
		t.Fatalf("Metadata().Permissions = %v", meta.Permissions)
	}
	if p.Name() != testPlugin {
		t.Fatalf("Name() = %q", p.Name())
	}
}

func TestNewWithConnConfigJSONBoundary(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]any
		want     string
	}{
		{name: "nil 配置编码为空串", settings: nil, want: ""},
		{name: "空映射编码为空对象", settings: map[string]any{}, want: "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePlugin{}
			cfg := baseConfig()
			cfg.Settings = tc.settings
			newTestPlugin(t, cfg, fake)

			if got := fake.snapshotInit().GetConfigJson(); got != tc.want {
				t.Fatalf("config_json = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestNewWithConnFailsWhenPluginRejectsInit(t *testing.T) {
	fake := &fakePlugin{initResp: &pluginpb.InitResponse{Ok: false, Error: "bad token"}}
	_, err := NewWithConn(startBufconn(t, fake), baseConfig(), Deps{Logger: testLogger()})
	if err == nil {
		t.Fatal("插件拒绝初始化时应返回错误")
	}
	if !strings.Contains(err.Error(), "bad token") || !strings.Contains(err.Error(), testPlugin) {
		t.Fatalf("错误信息 = %v，期望同时包含插件名与插件返回的原因", err)
	}
}

func TestNewWithConnFailsOnInitRPCError(t *testing.T) {
	fake := &fakePlugin{initErr: status.Error(codes.Internal, "插件内部故障")}
	_, err := NewWithConn(startBufconn(t, fake), baseConfig(), Deps{Logger: testLogger()})
	if err == nil {
		t.Fatal("Init RPC 失败时应返回错误")
	}
	if !strings.Contains(err.Error(), testPlugin) || !statusCodeIs(err, codes.Internal) {
		t.Fatalf("错误 = %v，期望包含插件名并保留 gRPC 状态码", err)
	}
}

func TestNewWithConnRejectsNilConn(t *testing.T) {
	if _, err := NewWithConn(nil, baseConfig(), Deps{}); err == nil {
		t.Fatal("conn 为 nil 时应返回错误")
	}
}

func TestNewRejectsEmptyAddr(t *testing.T) {
	cfg := baseConfig()
	cfg.Addr = "   "
	if _, err := New(cfg, Deps{}); err == nil {
		t.Fatal("Addr 为空时应快速失败")
	} else if !strings.Contains(err.Error(), testPlugin) {
		t.Fatalf("错误 = %v，期望包含插件名", err)
	}
}

func TestNewFailsFastWhenAddrUnreachable(t *testing.T) {
	addr := freeAddr(t)

	cfg := baseConfig()
	cfg.Addr = addr
	// 就绪等待窗口要短，否则测试会白白等满默认 10s。
	cfg.Timeout = 200 * time.Millisecond

	start := time.Now()
	_, err := New(cfg, Deps{Logger: testLogger()})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("插件不可达时应返回错误")
	}
	// 真·不可达必须在 timeout 附近返回，而不是卡满 10s。
	if elapsed > time.Second {
		t.Fatalf("不可达耗时 %v，应在 ~200ms 内返回", elapsed)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("不可达耗时仅 %v，未真正等待就绪窗口", elapsed)
	}
	// 错误必须指出是「等不到插件」，并带上地址，否则排查方向会被带偏。
	if !strings.Contains(err.Error(), addr) {
		t.Fatalf("错误 = %v，期望包含地址 %s", err, addr)
	}
	if !strings.Contains(err.Error(), "未就绪") {
		t.Fatalf("错误 = %v，期望标明是等待超时而非插件拒绝", err)
	}
}

func TestNewWaitsForLateStartingPlugin(t *testing.T) {
	addr := freeAddr(t)
	fake := &fakePlugin{}

	cfg := baseConfig()
	cfg.Addr = addr
	cfg.Timeout = 5 * time.Second

	// 插件在 New 已经进入等待之后才监听，模拟「核心先起、插件后起」，
	// 以及编排器同时拉起两侧时的随机顺序。
	started := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// 时延内端口被抢占属于环境异常，用 t.Errorf 记录而非 panic。
			t.Errorf("插件延迟监听 %s: %v", addr, err)
			close(started)
			return
		}
		srv := grpc.NewServer()
		pluginpb.RegisterPluginServiceServer(srv, fake)
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() {
			srv.Stop()
			_ = ln.Close()
		})
		close(started)
	}()

	p, err := New(cfg, Deps{Logger: testLogger()})
	<-started
	if err != nil {
		t.Fatalf("插件稍晚就绪时 New 应成功: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// New 开始时地址确实无人监听，因此这个成功只能来自 WaitForReady 的等待。
	if fake.snapshotInit() == nil {
		t.Fatal("Init 未被投递")
	}
}

// freeAddr 返回一个确定无人监听的 localhost 地址。
//
// 先占用再释放：端口由内核分配，避免与其它测试或本机服务冲突。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return addr
}

// captureRegistrar 记录插件注册的规则，用于断言注册行为。
type captureRegistrar struct {
	all       []bot.Handler
	allOpts   [][]bot.Option
	otherCall int
}

func (r *captureRegistrar) OnCommand(string, bot.Handler, ...bot.Option) { r.otherCall++ }
func (r *captureRegistrar) OnRegex(string, bot.Handler, ...bot.Option)   { r.otherCall++ }
func (r *captureRegistrar) OnKeyword([]string, bot.Handler, ...bot.Option) {
	r.otherCall++
}
func (r *captureRegistrar) OnEvent(bot.EventType, bot.Handler, ...bot.Option) { r.otherCall++ }
func (r *captureRegistrar) Use(bot.Middleware)                                { r.otherCall++ }
func (r *captureRegistrar) OnAll(h bot.Handler, opts ...bot.Option) {
	r.all = append(r.all, h)
	r.allOpts = append(r.allOpts, opts)
}

func TestMetadataIsolatesCallerPermissions(t *testing.T) {
	fake := &fakePlugin{}
	perms := []bot.Permission{bot.PermSendMessage}
	cfg := baseConfig()
	cfg.Permissions = perms
	p := newTestPlugin(t, cfg, fake)

	// 调用方后续修改自己的配置切片，不得改写已构造插件的权限声明。
	perms[0] = bot.PermAll
	if got := p.Metadata().Permissions; len(got) != 1 || got[0] != bot.PermSendMessage {
		t.Fatalf("Metadata().Permissions = %v，期望不受调用方修改影响", got)
	}
	if got := fake.snapshotInit().GetPermissions(); len(got) != 1 || got[0] != string(bot.PermSendMessage) {
		t.Fatalf("InitRequest.permissions = %v，期望不受调用方修改影响", got)
	}
}

func TestSetupRegistersSingleCatchAllRule(t *testing.T) {
	fake := &fakePlugin{}
	p := newTestPlugin(t, baseConfig(), fake)

	reg := &captureRegistrar{}
	if err := p.Setup(context.Background(), reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if len(reg.all) != 1 {
		t.Fatalf("OnAll 调用次数 = %d，期望 1", len(reg.all))
	}
	if reg.otherCall != 0 {
		t.Fatalf("Setup 不应注册其它规则或中间件，实际调用 %d 次", reg.otherCall)
	}
	// 不带 Option 意味着优先级保持默认 0：核心侧规则优先级不被外部插件挤占。
	if len(reg.allOpts[0]) != 0 {
		t.Fatalf("OnAll 应不带 Option，实际 %d 个", len(reg.allOpts[0]))
	}
	if err := p.Setup(context.Background(), nil); err == nil {
		t.Fatal("Registrar 为 nil 时应返回错误")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestHandleEventConvertsEvent(t *testing.T) {
	fake := &fakePlugin{}
	p := newTestPlugin(t, baseConfig(), fake)
	reg := &captureRegistrar{}
	if err := p.Setup(context.Background(), reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	ts := time.Date(2026, 9, 23, 10, 11, 12, 345*int(time.Millisecond), time.UTC)
	ev := &bot.Event{
		ID:       "evt-1",
		Type:     bot.EventMessage,
		Platform: "mock",
		BotID:    "bot-a",
		Time:     ts,
		Message: &bot.Message{
			ID:   "m-1",
			Kind: bot.MessageGroup,
			Segments: []bot.Segment{
				{Type: bot.SegText, Data: map[string]any{bot.KeyText: "/ext hi"}},
				{Type: bot.SegImage, Data: map[string]any{bot.KeyURL: "https://example.com/a.png"}},
			},
		},
		Sender:  &bot.User{ID: "u-1", Name: "张三"},
		Channel: &bot.Channel{ID: "room-1", Name: "房间", Kind: bot.MessageGroup},
		Command: &bot.Command{Name: "ext", Args: []string{"hi"}, Raw: "hi"},
		Raw:     map[string]any{"seq": float64(7)},
	}

	if err := reg.all[0](context.Background(), ev, nil); err != nil {
		t.Fatalf("Handler: %v", err)
	}

	events := fake.snapshotEvents()
	if len(events) != 1 {
		t.Fatalf("收到事件数 = %d，期望 1", len(events))
	}
	got := events[0]
	if got.GetId() != "evt-1" || got.GetType() != "message" || got.GetPlatform() != "mock" || got.GetBotId() != "bot-a" {
		t.Fatalf("事件头部 = %+v", got)
	}
	// 时间必须按 UTC 毫秒传输，避免插件侧时区偏移。
	if got.GetTimeUnixMs() != ts.UnixMilli() {
		t.Fatalf("time_unix_ms = %d，期望 %d", got.GetTimeUnixMs(), ts.UnixMilli())
	}
	segs := got.GetMessage().GetSegments()
	if len(segs) != 2 {
		t.Fatalf("段数 = %d，期望 2", len(segs))
	}
	var text struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(segs[0].GetDataJson()), &text); err != nil {
		t.Fatalf("data_json %q 解析失败: %v", segs[0].GetDataJson(), err)
	}
	if segs[0].GetType() != string(bot.SegText) || text.Text != "/ext hi" {
		t.Fatalf("文本段 = (%q, %+v)", segs[0].GetType(), text)
	}
	if segs[1].GetType() != string(bot.SegImage) || !strings.Contains(segs[1].GetDataJson(), "a.png") {
		t.Fatalf("图片段 = (%q, %q)", segs[1].GetType(), segs[1].GetDataJson())
	}
	if cmd := got.GetCommand(); cmd.GetName() != "ext" || cmd.GetRaw() != "hi" || len(cmd.GetArgs()) != 1 {
		t.Fatalf("command = %+v", cmd)
	}
	if got.GetSender().GetId() != "u-1" || got.GetChannel().GetId() != "room-1" || got.GetChannel().GetKind() != "group" {
		t.Fatalf("sender/channel = %+v / %+v", got.GetSender(), got.GetChannel())
	}
}

func TestHandleEventResults(t *testing.T) {
	rpcErr := status.Error(codes.Unavailable, "插件掉线")

	cases := []struct {
		name      string
		resp      *pluginpb.HandleResult
		err       error
		wantErr   bool
		errSubstr string
	}{
		{name: "插件自报错误向上返回", resp: &pluginpb.HandleResult{Handled: true, Error: "发送失败"}, wantErr: true, errSubstr: "发送失败"},
		{name: "RPC 错误被包装", err: rpcErr, wantErr: true, errSubstr: "Unavailable"},
		{name: "未处理不算失败", resp: &pluginpb.HandleResult{Handled: false}, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePlugin{handleResp: tc.resp, handleErr: tc.err}
			p := newTestPlugin(t, baseConfig(), fake)
			reg := &captureRegistrar{}
			if err := p.Setup(context.Background(), reg); err != nil {
				t.Fatalf("Setup: %v", err)
			}

			err := reg.all[0](context.Background(), &bot.Event{ID: "evt-1", Type: bot.EventMessage}, nil)
			switch {
			case tc.wantErr && err == nil:
				t.Fatal("期望返回错误")
			case !tc.wantErr && err != nil:
				t.Fatalf("期望返回 nil，实际 %v", err)
			case tc.wantErr && !strings.Contains(err.Error(), tc.errSubstr):
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.errSubstr)
			case tc.wantErr && !strings.Contains(err.Error(), testPlugin):
				t.Fatalf("错误 = %v，期望包含插件名", err)
			}
			if len(fake.snapshotEvents()) != 1 {
				t.Fatal("事件未被投递给插件")
			}
		})
	}
}

func TestHandleEventTimesOutOnSlowPlugin(t *testing.T) {
	fake := &fakePlugin{handleWait: 2 * time.Second}
	cfg := baseConfig()
	cfg.Timeout = 20 * time.Millisecond
	p := newTestPlugin(t, cfg, fake)
	reg := &captureRegistrar{}
	if err := p.Setup(context.Background(), reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	start := time.Now()
	err := reg.all[0](context.Background(), &bot.Event{ID: "evt-slow", Type: bot.EventMessage}, nil)
	if err == nil {
		t.Fatal("插件超时应返回错误")
	}
	// 超时必须来自单次调用超时而非测试自身等待，因此要求显著早于插件处理耗时。
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("超时未生效，耗时 %v", elapsed)
	}
}

func TestHandleEventPropagatesUpstreamCancellation(t *testing.T) {
	fake := &fakePlugin{handleWait: 2 * time.Second}
	cfg := baseConfig()
	cfg.Timeout = time.Minute
	p := newTestPlugin(t, cfg, fake)
	reg := &captureRegistrar{}
	if err := p.Setup(context.Background(), reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := reg.all[0](ctx, &bot.Event{ID: "evt-cancel", Type: bot.EventMessage}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 context.Canceled", err)
	}
}

func TestStopNotifiesShutdownOnce(t *testing.T) {
	fake := &fakePlugin{}
	p := newTestPlugin(t, baseConfig(), fake)

	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("重复 Stop: %v", err)
	}
	if got := fake.shutdownCount(); got != 1 {
		t.Fatalf("Shutdown 调用次数 = %d，期望 1", got)
	}
}

func TestStopAttemptsShutdownWithCancelledContext(t *testing.T) {
	fake := &fakePlugin{}
	p := newTestPlugin(t, baseConfig(), fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 上游已取消也必须通知插件释放资源。
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop(已取消 ctx): %v", err)
	}
	if got := fake.shutdownCount(); got != 1 {
		t.Fatalf("Shutdown 调用次数 = %d，期望 1", got)
	}
}

func TestStopToleratesShutdownFailure(t *testing.T) {
	fake := &fakePlugin{shutdownEr: status.Error(codes.Unavailable, "插件已退出")}
	p := newTestPlugin(t, baseConfig(), fake)

	// Shutdown 失败只应记日志：此时插件多半已不可达，不应阻断核心关闭流程。
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := fake.shutdownCount(); got != 1 {
		t.Fatalf("Shutdown 调用次数 = %d，期望 1", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	fake := &fakePlugin{}
	p := newTestPlugin(t, baseConfig(), fake)

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	// 关闭之后 Handler 必须报错而不是静默成功，避免丢掉事件却无人察觉。
	reg := &captureRegistrar{}
	if err := p.Setup(context.Background(), reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := reg.all[0](context.Background(), &bot.Event{ID: "evt-1"}, nil); err == nil {
		t.Fatal("连接关闭后投递事件应返回错误")
	}
}

func TestCloseWithoutStopDoesNotShutdown(t *testing.T) {
	fake := &fakePlugin{}
	p := newTestPlugin(t, baseConfig(), fake)

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fake.shutdownCount(); got != 0 {
		t.Fatalf("Close 不应通知插件退出，Shutdown 调用次数 = %d", got)
	}
}

func TestCloseReleasesConnResources(t *testing.T) {
	fake := &fakePlugin{}
	conn := startBufconn(t, fake)
	p, err := NewWithConn(conn, baseConfig(), Deps{Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewWithConn: %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// ClientConn.Close 是同步的：它等待内部 addrConn/resolver/balancer 全部拆除后
	// 才返回，因此状态为 Shutdown 即证明连接相关 goroutine 已回收。
	if got := conn.GetState(); got != connectivity.Shutdown {
		t.Fatalf("连接状态 = %v，期望 Shutdown", got)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
}

// statusCodeIs 判断错误链上是否带有指定 gRPC 状态码。
func statusCodeIs(err error, code codes.Code) bool {
	var se interface{ GRPCStatus() *status.Status }
	if errors.As(err, &se) {
		return se.GRPCStatus().Code() == code
	}
	return false
}
