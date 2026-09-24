package external

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

// fakeService 是内存版 AdapterService，用于核心侧客户端的单元测试。
type fakeService struct {
	pluginpb.UnimplementedAdapterServiceServer

	mu       sync.Mutex
	initReqs []*pluginpb.AdapterInitRequest
	info     *pluginpb.AdapterInfo
	refuse   bool

	startReqs     []*pluginpb.AdapterInstance
	startErr      error
	startCaps     *pluginpb.Capabilities
	startPlatform string

	sendReqs  []*pluginpb.AdapterSendRequest
	sendMsgID string
	sendErr   string

	stops     []string
	shutdowns int
}

func (s *fakeService) Init(_ context.Context, req *pluginpb.AdapterInitRequest) (*pluginpb.AdapterInitResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initReqs = append(s.initReqs, req)
	if s.refuse {
		return &pluginpb.AdapterInitResponse{Ok: false, Error: "拒绝初始化"}, nil
	}
	if s.info != nil {
		return &pluginpb.AdapterInitResponse{Ok: true, Info: s.info}, nil
	}
	return &pluginpb.AdapterInitResponse{Ok: true, Info: defaultInfo()}, nil
}

func (s *fakeService) Start(_ context.Context, req *pluginpb.AdapterInstance) (*pluginpb.AdapterStartResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startReqs = append(s.startReqs, req)
	if s.startErr != nil {
		return nil, s.startErr
	}
	caps := s.startCaps
	if caps == nil {
		caps = &pluginpb.Capabilities{Text: true, Markdown: true}
	}
	return &pluginpb.AdapterStartResponse{Ok: true, Capabilities: caps, Platform: s.startPlatform}, nil
}

func (s *fakeService) Stop(_ context.Context, req *pluginpb.AdapterStopRequest) (*pluginpb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = append(s.stops, req.GetBotId())
	return &pluginpb.Empty{}, nil
}

func (s *fakeService) Send(_ context.Context, req *pluginpb.AdapterSendRequest) (*pluginpb.SendResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendReqs = append(s.sendReqs, req)
	if s.sendErr != "" {
		return &pluginpb.SendResponse{Error: s.sendErr}, nil
	}
	id := s.sendMsgID
	if id == "" {
		id = "ext-1"
	}
	return &pluginpb.SendResponse{MessageId: id}, nil
}

func (s *fakeService) Shutdown(context.Context, *pluginpb.Empty) (*pluginpb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdowns++
	return &pluginpb.Empty{}, nil
}

func (s *fakeService) counts() (init, start, send, stop, shutdown int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.initReqs), len(s.startReqs), len(s.sendReqs), len(s.stops), s.shutdowns
}

// defaultInfo 是适配器默认上报的元信息。
func defaultInfo() *pluginpb.AdapterInfo {
	return &pluginpb.AdapterInfo{
		Name:        "myim",
		Version:     "v0.1.0",
		Author:      "third-party",
		Description: "MyIM 适配器",
		Platforms:   []string{"myim"},
		Permissions: []string{string(bot.PermReceiveEvent), string(bot.PermNetwork), string(bot.PermNetListen)},
		Options:     []string{"listen_addr"},
	}
}

// startService 在本地端口上启动 fakeService，返回监听地址。
func startService(t *testing.T, svc *fakeService) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pluginpb.RegisterAdapterServiceServer(srv, svc)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

// newTestClient 连接 fakeService 并完成 Init。
func newTestClient(t *testing.T, svc *fakeService, mutate func(*Config)) *Client {
	t.Helper()
	addr := startService(t, svc)
	cfg := Config{
		Name:        "myim",
		Addr:        addr,
		CoreAddr:    "127.0.0.1:19070",
		Token:       "change-me",
		Permissions: []bot.Permission{bot.PermReceiveEvent, bot.PermNetwork, bot.PermNetListen},
		Settings:    map[string]any{"listen_addr": "127.0.0.1:0"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	client, err := New(context.Background(), cfg, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestNewInitAndMetadata(t *testing.T) {
	svc := &fakeService{}
	client := newTestClient(t, svc, nil)

	init, _, _, _, _ := svc.counts()
	if init != 1 {
		t.Fatalf("Init 调用次数 = %d, want 1", init)
	}
	req := svc.initReqs[0]
	if req.GetToken() != "change-me" || req.GetCoreAddr() != "127.0.0.1:19070" {
		t.Fatalf("Init 请求 = %+v", req)
	}
	if got := req.GetInfo().GetName(); got != "myim" {
		t.Fatalf("Init 身份名 = %q", got)
	}
	if !strings.Contains(req.GetConfigJson(), "listen_addr") {
		t.Fatalf("进程级配置未下发: %q", req.GetConfigJson())
	}

	if got := client.Platform(); got != "myim" {
		t.Fatalf("Platform() = %q", got)
	}
	meta := client.Metadata()
	if meta.Name != "myim" || meta.Version != "v0.1.0" || meta.Author != "third-party" {
		t.Fatalf("Metadata() = %+v", meta)
	}
	// 权限取交集：声明与授予相同 → 全部保留；Options 来自适配器上报。
	if got := len(meta.Permissions); got != 3 {
		t.Fatalf("权限 = %v", meta.Permissions)
	}
	if len(meta.Options) != 1 || meta.Options[0] != "listen_addr" {
		t.Fatalf("Options = %v", meta.Options)
	}
}

func TestNewRestrictsPermissionsToGranted(t *testing.T) {
	svc := &fakeService{}
	client := newTestClient(t, svc, func(c *Config) {
		c.Permissions = []bot.Permission{bot.PermReceiveEvent}
	})

	meta := client.Metadata()
	if len(meta.Permissions) != 1 || meta.Permissions[0] != bot.PermReceiveEvent {
		t.Fatalf("权限应为授予集合: %v", meta.Permissions)
	}
	// 未授予 net_listen 时，实例配置里的保留键必须被拒。
	_, err := client.Instance("myim-main", bot.NewConfig(map[string]any{bot.OptListenAddr: "127.0.0.1:0"}))
	if err == nil || !strings.Contains(err.Error(), string(bot.PermNetListen)) {
		t.Fatalf("保留键校验: %v", err)
	}
	// 未授予 network 时不影响实例创建（进程内裁剪不适用于外部进程）。
	if _, err := client.Instance("myim-main", bot.NewConfig(nil)); err != nil {
		t.Fatalf("Instance: %v", err)
	}
}

func TestNewRejectsInvalidInit(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeService, *Config)
		want   string
	}{
		{
			name:   "适配器拒绝初始化",
			mutate: func(s *fakeService, _ *Config) { s.refuse = true },
			want:   "拒绝初始化",
		},
		{
			name: "上报名不一致",
			mutate: func(s *fakeService, _ *Config) {
				info := defaultInfo()
				info.Name = "other"
				s.info = info
			},
			want: "不一致",
		},
		{
			name: "未上报 Platforms",
			mutate: func(s *fakeService, _ *Config) {
				info := defaultInfo()
				info.Platforms = nil
				s.info = info
			},
			want: "Platforms",
		},
		{
			name: "多平台未指定",
			mutate: func(s *fakeService, _ *Config) {
				info := defaultInfo()
				info.Platforms = []string{"myim", "myim2"}
				s.info = info
			},
			want: "platform",
		},
		{
			name: "声明 PermAll",
			mutate: func(s *fakeService, _ *Config) {
				info := defaultInfo()
				info.Permissions = append(info.Permissions, string(bot.PermAll))
				s.info = info
			},
			want: "不得声明",
		},
		{
			name:   "地址为空",
			mutate: func(_ *fakeService, c *Config) { c.Addr = "" },
			want:   "Addr",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeService{}
			addr := startService(t, svc)
			cfg := Config{
				Name: "myim", Addr: addr, CoreAddr: "127.0.0.1:19070", Token: "t",
				Permissions: []bot.Permission{bot.PermReceiveEvent},
				Timeout:     time.Second,
			}
			tc.mutate(svc, &cfg)

			client, err := New(context.Background(), cfg, Deps{})
			if err == nil {
				_ = client.Close()
				t.Fatal("期望报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v, 期望包含 %q", err, tc.want)
			}
		})
	}
}

func TestExplicitPlatformMustBeReported(t *testing.T) {
	svc := &fakeService{}
	svc.info = defaultInfo()
	svc.info.Platforms = []string{"myim", "myim2"}

	client := newTestClient(t, svc, func(c *Config) { c.Platform = "myim2" })
	if got := client.Platform(); got != "myim2" {
		t.Fatalf("Platform() = %q", got)
	}

	// 配置了未被上报的平台 → 启动失败。
	svc2 := &fakeService{info: defaultInfo()}
	addr := startService(t, svc2)
	_, err := New(context.Background(), Config{
		Name: "myim", Addr: addr, CoreAddr: "127.0.0.1:19070", Token: "t", Platform: "nope",
	}, Deps{})
	if err == nil || !strings.Contains(err.Error(), "不含") {
		t.Fatalf("未上报的平台应报错: %v", err)
	}
}

func TestInstanceStartSendStop(t *testing.T) {
	svc := &fakeService{}
	client := newTestClient(t, svc, nil)

	inst, err := client.Instance("myim-main", bot.NewConfig(map[string]any{
		bot.OptListenAddr: "127.0.0.1:0",
		"api_base":        "https://im.example.com",
	}))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	if inst.Name() != "myim" {
		t.Fatalf("实例平台名 = %q", inst.Name())
	}
	if got := inst.Capabilities(); got.Text {
		t.Fatal("Start 之前不应有已声明的能力")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- inst.Start(ctx, nopSink{}) }()

	// 等待 Start RPC 生效：能力以应答为唯一权威来源，出现即表示应答已处理。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if inst.Capabilities().Text {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Start RPC 未在超时内生效")
		}
		time.Sleep(time.Millisecond)
	}

	svc.mu.Lock()
	startReq := svc.startReqs[0]
	svc.mu.Unlock()
	if startReq.GetBotId() != "myim-main" || startReq.GetPlatform() != "myim" {
		t.Fatalf("Start 请求 = %+v", startReq)
	}
	if !strings.Contains(startReq.GetConfigJson(), "api_base") {
		t.Fatalf("实例级配置未下发: %q", startReq.GetConfigJson())
	}

	caps := inst.Capabilities()
	if !caps.Text || !caps.Markdown || caps.Card {
		t.Fatalf("能力 = %+v", caps)
	}

	res, err := inst.Send(context.Background(), &bot.SendRequest{
		BotID:  "myim-main",
		Target: bot.Target{Platform: "myim", BotID: "myim-main", ChannelID: "c1", Kind: bot.MessageGroup},
		Message: &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi"}},
		}},
		ReplyTo: "m0",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID != "ext-1" {
		t.Fatalf("MessageID = %q", res.MessageID)
	}

	svc.mu.Lock()
	sendReq := svc.sendReqs[0]
	svc.mu.Unlock()
	if sendReq.GetBotId() != "myim-main" || sendReq.GetReplyTo() != "m0" {
		t.Fatalf("Send 请求 = %+v", sendReq)
	}
	if sendReq.GetTarget().GetChannelId() != "c1" {
		t.Fatalf("Send 目标 = %+v", sendReq.GetTarget())
	}
	if got := sendReq.GetMessage().GetSegments()[0].GetDataJson(); !strings.Contains(got, "hi") {
		t.Fatalf("Send 消息段 = %q", got)
	}

	// 平台业务失败按错误返回，交给引擎重试。
	svc.mu.Lock()
	svc.sendErr = "平台拒绝"
	svc.mu.Unlock()
	if _, err := inst.Send(context.Background(), &bot.SendRequest{Message: &bot.Message{}}); err == nil ||
		!strings.Contains(err.Error(), "平台拒绝") {
		t.Fatalf("业务失败应作为错误返回: %v", err)
	}

	// 引擎关闭路径：Stop 幂等。
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("重复 Stop: %v", err)
	}
	_, _, _, stops, _ := svc.counts()
	if stops != 1 {
		t.Fatalf("Stop 调用次数 = %d, want 1", stops)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start 退出: %v", err)
	}
}

func TestReconnectRestartsInstances(t *testing.T) {
	svc := &fakeService{}
	client := newTestClient(t, svc, nil)

	inst, err := client.Instance("myim-main", bot.NewConfig(nil))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	if err := inst.(*instance).start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := client.reconnect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if _, starts, _, _, _ := svc.counts(); starts != 2 {
		t.Fatalf("重连后 Start 次数 = %d, want 2", starts)
	}

	// 适配器不可用时重连失败，实例被停用后发送返回明确错误。
	svc.mu.Lock()
	svc.startErr = errors.New("适配器不可用")
	svc.mu.Unlock()
	if err := client.reconnect(context.Background()); err == nil {
		t.Fatal("重连失败应返回错误")
	}

	client.setDisabled(true)
	if _, err := inst.Send(context.Background(), &bot.SendRequest{Message: &bot.Message{}}); err == nil ||
		!strings.Contains(err.Error(), "已停用") {
		t.Fatalf("停用实例应拒绝发送: %v", err)
	}
	client.setDisabled(false)
}

func TestCloseStopsInstancesAndShutsDownAdapter(t *testing.T) {
	svc := &fakeService{}
	addr := startService(t, svc)
	client, err := New(context.Background(), Config{
		Name: "myim", Addr: addr, CoreAddr: "127.0.0.1:19070", Token: "t",
		Permissions: []bot.Permission{bot.PermReceiveEvent},
	}, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inst, err := client.Instance("myim-main", bot.NewConfig(nil))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	if err := inst.(*instance).start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, _, stops, shutdowns := svc.counts()
	if stops != 1 || shutdowns != 1 {
		t.Fatalf("stops = %d, shutdowns = %d", stops, shutdowns)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	if _, err := client.Instance("myim-main", bot.NewConfig(nil)); err == nil {
		t.Fatal("关闭后不应再接受实例")
	}
}

// TestSupervisorReconnectsAfterAdapterCrash 用 bufconn 模拟适配器进程崩溃与重启：
// 核心必须重连、重启已启动的实例，并且进程本身不受影响。
func TestSupervisorReconnectsAfterAdapterCrash(t *testing.T) {
	svc := &fakeService{}

	var (
		lnMu     sync.Mutex
		listener *bufconn.Listener
		servers  []*grpc.Server
	)
	setListener := func(l *bufconn.Listener) {
		lnMu.Lock()
		listener = l
		lnMu.Unlock()
	}
	dialer := func(context.Context, string) (net.Conn, error) {
		lnMu.Lock()
		ln := listener
		lnMu.Unlock()
		if ln == nil {
			return nil, errors.New("适配器进程未运行")
		}
		return ln.Dial()
	}
	serve := func() (*bufconn.Listener, *grpc.Server) {
		ln := bufconn.Listen(1 << 20)
		srv := grpc.NewServer()
		pluginpb.RegisterAdapterServiceServer(srv, svc)
		go func() { _ = srv.Serve(ln) }()
		lnMu.Lock()
		servers = append(servers, srv)
		lnMu.Unlock()
		setListener(ln)
		return ln, srv
	}

	_, firstSrv := serve()
	t.Cleanup(func() {
		lnMu.Lock()
		defer lnMu.Unlock()
		for _, srv := range servers {
			srv.Stop()
		}
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client, err := newWithConn(context.Background(), conn, Config{
		Name: "myim", Addr: "bufnet", CoreAddr: "127.0.0.1:19070", Token: "t",
		Permissions: []bot.Permission{bot.PermReceiveEvent},
		Timeout:     time.Second,
	}, Deps{})
	if err != nil {
		t.Fatalf("newWithConn: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	inst, err := client.Instance("myim-main", bot.NewConfig(nil))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	proxy := inst.(*instance)
	if err := proxy.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.startSupervisor(ctx)

	// 适配器进程崩溃：服务端与连接一起消失，Send 失败，但核心进程继续存活。
	firstSrv.Stop()
	setListener(nil)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := proxy.Send(context.Background(), &bot.SendRequest{Message: &bot.Message{}}); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("适配器崩溃后 Send 仍成功")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 适配器进程重启（同一地址），巡检应重连并重新 Start 实例。
	serve()
	deadline = time.Now().Add(10 * time.Second)
	for {
		svc.mu.Lock()
		starts := len(svc.startReqs)
		svc.mu.Unlock()
		if starts >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("适配器重启后未重新 Start 实例（starts=%d）", starts)
		}
		time.Sleep(10 * time.Millisecond)
	}

	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := proxy.Send(context.Background(), &bot.SendRequest{Message: &bot.Message{}}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("重连后 Send 仍未恢复")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// nopSink 是满足 bot.EventSink 的空实现：外部适配器的事件不经 sink 上行。
type nopSink struct{}

func (nopSink) Emit(context.Context, *bot.Event) error { return nil }
