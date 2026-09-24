package adaptermgr

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

// fakeAdapterService 是外部适配器通道回归测试用的最小 AdapterService。
//
// 它存在的意义是：Build 成功返回后必须保持连接可用（早期实现在返回时误关了
// 连接，导致引擎启动阶段的下发 Start 失败）。
type fakeAdapterService struct {
	pluginpb.UnimplementedAdapterServiceServer

	mu        sync.Mutex
	started   []string
	sends     int
	stops     int
	shutdowns int
}

func (s *fakeAdapterService) Init(context.Context, *pluginpb.AdapterInitRequest) (*pluginpb.AdapterInitResponse, error) {
	return &pluginpb.AdapterInitResponse{Ok: true, Info: &pluginpb.AdapterInfo{
		Name:        "ext",
		Platforms:   []string{"ext"},
		Permissions: []string{string(bot.PermReceiveEvent), string(bot.PermNetListen)},
		Options:     []string{bot.OptListenAddr},
	}}, nil
}

func (s *fakeAdapterService) Start(_ context.Context, req *pluginpb.AdapterInstance) (*pluginpb.AdapterStartResponse, error) {
	s.mu.Lock()
	s.started = append(s.started, req.GetBotId())
	s.mu.Unlock()
	return &pluginpb.AdapterStartResponse{Ok: true, Capabilities: &pluginpb.Capabilities{Text: true}, Platform: req.GetPlatform()}, nil
}

func (s *fakeAdapterService) Stop(context.Context, *pluginpb.AdapterStopRequest) (*pluginpb.Empty, error) {
	s.mu.Lock()
	s.stops++
	s.mu.Unlock()
	return &pluginpb.Empty{}, nil
}

func (s *fakeAdapterService) Send(context.Context, *pluginpb.AdapterSendRequest) (*pluginpb.SendResponse, error) {
	s.mu.Lock()
	s.sends++
	s.mu.Unlock()
	return &pluginpb.SendResponse{MessageId: "ext-1"}, nil
}

func (s *fakeAdapterService) Shutdown(context.Context, *pluginpb.Empty) (*pluginpb.Empty, error) {
	s.mu.Lock()
	s.shutdowns++
	s.mu.Unlock()
	return &pluginpb.Empty{}, nil
}

// startFakeAdapterService 启动 fakeAdapterService，返回监听地址。
func startFakeAdapterService(t *testing.T, svc *fakeAdapterService) string {
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

// TestBuildKeepsExternalChannelUsable 覆盖外部适配器装配后的连接所有权：
// Build 成功返回的连接必须仍然可用，Start/Send 必须能通过它下发。
func TestBuildKeepsExternalChannelUsable(t *testing.T) {
	svc := &fakeAdapterService{}
	addr := startFakeAdapterService(t, svc)

	cfg := &config.Config{
		Grpc: config.GrpcConfig{Addr: "127.0.0.1:19070"},
		Adapters: map[string]config.AdapterConfig{
			"ext": {GrpcAddr: addr, Token: "t", Permissions: []string{string(bot.PermReceiveEvent), string(bot.PermNetListen)}},
		},
		Bots: []config.BotConfig{{Name: "ext-main", Adapter: "ext", Settings: map[string]any{bot.OptListenAddr: "127.0.0.1:0"}}},
	}

	bindings, err := Build(context.Background(), cfg, Deps{Logger: nil, Storage: storage.NewMemory()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = bindings.Close() })

	list := bindings.List()
	if len(list) != 1 || !list[0].Info.External || list[0].Info.Metadata.Name != "ext" {
		t.Fatalf("绑定 = %+v", list)
	}
	if got := list[0].Info.Metadata.Permissions; len(got) != 2 ||
		got[0] != bot.PermReceiveEvent || got[1] != bot.PermNetListen {
		t.Fatalf("权限 = %v", got)
	}

	ad := list[0].Adapter
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ad.Start(ctx, nopEventSink{}) }()

	// Start 必须真正下发到适配器进程。
	deadline := time.Now().Add(3 * time.Second)
	for {
		svc.mu.Lock()
		started := len(svc.started)
		svc.mu.Unlock()
		if started == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Start 未下发到外部适配器（连接可能已被误关）")
		}
		time.Sleep(2 * time.Millisecond)
	}

	res, err := ad.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: "ext", BotID: "ext-main", ChannelID: "c1"},
		Message: &bot.Message{Segments: []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Send 应经外部通道成功: %v", err)
	}
	if !strings.HasPrefix(res.MessageID, "ext-") {
		t.Fatalf("消息 ID = %q", res.MessageID)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start 退出: %v", err)
	}
	if err := bindings.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.sends != 1 || svc.stops != 1 || svc.shutdowns != 1 {
		t.Fatalf("sends=%d stops=%d shutdowns=%d", svc.sends, svc.stops, svc.shutdowns)
	}
}

// TestBuildFailureReleasesPartialAssembly 覆盖装配失败的返回语义：
// 返回错误、不返回部分绑定，并且修正配置后可以重新装配成功（说明失败路径
// 没有把进程级状态留在不可恢复的状态）。
func TestBuildFailureReleasesPartialAssembly(t *testing.T) {
	svc := &fakeAdapterService{}
	addr := startFakeAdapterService(t, svc)

	cfg := &config.Config{
		Grpc: config.GrpcConfig{Addr: "127.0.0.1:19070"},
		Adapters: map[string]config.AdapterConfig{
			"ext": {GrpcAddr: addr, Token: "t", Permissions: []string{string(bot.PermReceiveEvent)}},
		},
		Bots: []config.BotConfig{
			{Name: "ext-main", Adapter: "ext", Settings: map[string]any{bot.OptListenAddr: "127.0.0.1:0"}},
			{Name: "ext-second", Adapter: "ext", Settings: map[string]any{bot.OptListenAddr: "127.0.0.1:0"}},
		},
	}
	// 仅授予 receive_event，因此 listen_addr 触发保留键校验失败。
	bindings, err := Build(context.Background(), cfg, Deps{Storage: storage.NewMemory()})
	if err == nil || !strings.Contains(err.Error(), string(bot.PermNetListen)) {
		t.Fatalf("保留键校验应让装配失败: %v", err)
	}
	if bindings != nil {
		t.Fatalf("失败时不应返回绑定: %+v", bindings.List())
	}

	cfg.Adapters["ext"] = config.AdapterConfig{
		GrpcAddr: addr, Token: "t",
		Permissions: []string{string(bot.PermReceiveEvent), string(bot.PermNetListen)},
	}
	bindings, err = Build(context.Background(), cfg, Deps{Storage: storage.NewMemory()})
	if err != nil {
		t.Fatalf("修正后重新装配: %v", err)
	}
	t.Cleanup(func() { _ = bindings.Close() })
	if len(bindings.List()) != 2 {
		t.Fatalf("绑定 = %+v", bindings.List())
	}
}

// nopEventSink 满足 bot.EventSink；外部适配器的事件不经 sink 上行。
type nopEventSink struct{}

func (nopEventSink) Emit(context.Context, *bot.Event) error { return nil }
