// Command example-adapter 是一个外部适配器示例：作为独立进程实现
// proto/adapter.proto 的 AdapterService，并在核心侧以 gRPC 客户端身份被 dial。
//
// 它演示外部适配器的两条数据流：
//
//	平台侧事件：HTTP 控制面 POST /inject -> BotService.EmitEvent -> 核心事件总线
//	发送：      核心 AdapterService.Send -> 本进程记录（真实适配器在此调用平台 API）
//
// 核心为每个 bot 调用一次 Start、一次 Stop，因此同一进程可以服务多个 bot 实例。
// 真实适配器把本文件里的「构造 bot.Event」与「记录发送」换成平台 SDK 调用即可。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/RandomLemon/kei/internal/grpcsrv"
	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

// 默认参数。
const (
	defaultListen   = "127.0.0.1:19071"
	defaultPlatform = "example"
	defaultControl  = "127.0.0.1:19081"

	shutdownTimeout = 5 * time.Second
	maxBodyBytes    = 1 << 20
)

// sendRecord 是一次发送请求的记录，供 /sent 查询。
type sendRecord struct {
	BotID     string            `json:"bot_id"`
	Target    *pluginpb.Target  `json:"target"`
	Message   *pluginpb.Message `json:"message"`
	ReplyTo   string            `json:"reply_to"`
	SentAt    time.Time         `json:"sent_at"`
	MessageID string            `json:"message_id"`
}

// instanceState 是一个 bot 实例的运行期状态，按 bot_id 隔离。
type instanceState struct {
	botID    string
	platform string
	settings map[string]any

	seq   atomic.Uint64
	mu    sync.Mutex
	sends []sendRecord
}

func (s *instanceState) record(rec sendRecord) {
	s.mu.Lock()
	s.sends = append(s.sends, rec)
	s.mu.Unlock()
}

func (s *instanceState) snapshot() []sendRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sendRecord(nil), s.sends...)
}

// service 实现 pluginpb.AdapterService。
type service struct {
	pluginpb.UnimplementedAdapterServiceServer

	log         *slog.Logger
	platform    string
	controlAddr string

	mu        sync.Mutex
	token     string
	coreAddr  string
	settings  map[string]any
	instances map[string]*instanceState
	shutdown  bool

	conn   *grpc.ClientConn
	client pluginpb.BotServiceClient
	http   *http.Server
}

func newService(logger *slog.Logger, platform, controlAddr string) *service {
	return &service{
		log:         logger,
		platform:    platform,
		controlAddr: controlAddr,
		instances:   make(map[string]*instanceState),
	}
}

// Init 接收核心下发的身份、令牌、核心地址与进程级配置。
//
// 此时核心的 BotService 可能尚未监听（适配器装配早于核心启动服务），因此这里只
// 保存地址并构造惰性连接，不做任何反向调用。
func (s *service) Init(_ context.Context, req *pluginpb.AdapterInitRequest) (*pluginpb.AdapterInitResponse, error) {
	if strings.TrimSpace(req.GetToken()) == "" {
		return &pluginpb.AdapterInitResponse{Ok: false, Error: "缺少 token"}, nil
	}
	if strings.TrimSpace(req.GetCoreAddr()) == "" {
		return &pluginpb.AdapterInitResponse{Ok: false, Error: "缺少 core_addr"}, nil
	}
	settings, err := decodeSettings(req.GetConfigJson())
	if err != nil {
		return &pluginpb.AdapterInitResponse{Ok: false, Error: err.Error()}, nil
	}

	s.mu.Lock()
	alreadyShutdown := s.shutdown
	s.mu.Unlock()
	if alreadyShutdown {
		// 已经收到过 Shutdown：核心若要重新接入必须重启适配器进程，
		// 否则会留下「Init 成功但任何实例都起不来」的迷惑状态。
		return &pluginpb.AdapterInitResponse{Ok: false, Error: "适配器已关闭，请重启进程"}, nil
	}

	conn, err := grpc.NewClient(target(req.GetCoreAddr()), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return &pluginpb.AdapterInitResponse{Ok: false, Error: fmt.Sprintf("连接核心失败: %v", err)}, nil
	}

	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.token = req.GetToken()
	s.coreAddr = req.GetCoreAddr()
	s.settings = settings
	s.conn = conn
	s.client = pluginpb.NewBotServiceClient(conn)
	platforms := req.GetInfo().GetPlatforms()
	s.mu.Unlock()

	s.log.Info("初始化完成",
		"core_addr", req.GetCoreAddr(),
		"granted_permissions", req.GetInfo().GetPermissions(),
		"settings", settings,
	)

	reported := s.platform
	if len(platforms) == 1 && platforms[0] != "" {
		reported = platforms[0]
	}
	return &pluginpb.AdapterInitResponse{
		Ok: true,
		Info: &pluginpb.AdapterInfo{
			Name:        req.GetInfo().GetName(),
			Version:     "v0.1.0",
			Author:      "core",
			Description: "外部适配器示例：/inject 注入事件、/sent 观察发送",
			Platforms:   []string{reported},
			Permissions: []string{string(bot.PermReceiveEvent), string(bot.PermNetwork), string(bot.PermNetListen)},
			Options:     []string{bot.OptListenAddr},
		},
	}, nil
}

// Start 启动一个 bot 实例；同一 bot 重复调用是幂等的。
func (s *service) Start(_ context.Context, req *pluginpb.AdapterInstance) (*pluginpb.AdapterStartResponse, error) {
	if strings.TrimSpace(req.GetBotId()) == "" {
		return &pluginpb.AdapterStartResponse{Ok: false, Error: "缺少 bot_id"}, nil
	}
	settings, err := decodeSettings(req.GetConfigJson())
	if err != nil {
		return &pluginpb.AdapterStartResponse{Ok: false, Error: err.Error()}, nil
	}
	platform := req.GetPlatform()
	if platform == "" {
		platform = s.platform
	}

	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return &pluginpb.AdapterStartResponse{Ok: false, Error: "适配器已关闭"}, nil
	}
	if _, ok := s.instances[req.GetBotId()]; !ok {
		s.instances[req.GetBotId()] = &instanceState{botID: req.GetBotId(), platform: platform, settings: settings}
	}
	s.mu.Unlock()

	s.log.Info("实例已启动", "bot", req.GetBotId(), "platform", platform, "settings", settings)
	return &pluginpb.AdapterStartResponse{
		Ok:           true,
		Platform:     platform,
		Capabilities: &pluginpb.Capabilities{Text: true, Image: true, At: true, Reply: true, Private: true, Group: true},
	}, nil
}

// Stop 停止一个 bot 实例；未知实例与重复调用都不算错误。
func (s *service) Stop(_ context.Context, req *pluginpb.AdapterStopRequest) (*pluginpb.Empty, error) {
	s.mu.Lock()
	delete(s.instances, req.GetBotId())
	s.mu.Unlock()
	s.log.Info("实例已停止", "bot", req.GetBotId())
	return &pluginpb.Empty{}, nil
}

// Send 记录一次发送请求并返回消息 ID；未知实例按业务失败上报。
func (s *service) Send(_ context.Context, req *pluginpb.AdapterSendRequest) (*pluginpb.SendResponse, error) {
	inst, ok := s.instance(req.GetBotId())
	if !ok {
		return &pluginpb.SendResponse{Error: fmt.Sprintf("未知 bot %q", req.GetBotId())}, nil
	}
	id := fmt.Sprintf("example-%d", inst.seq.Add(1))
	inst.record(sendRecord{
		BotID:     req.GetBotId(),
		Target:    req.GetTarget(),
		Message:   req.GetMessage(),
		ReplyTo:   req.GetReplyTo(),
		SentAt:    time.Now().UTC(),
		MessageID: id,
	})
	s.log.Info("收到发送请求", "bot", req.GetBotId(), "message_id", id)
	return &pluginpb.SendResponse{MessageId: id}, nil
}

// Shutdown 停止控制面并释放连接。
func (s *service) Shutdown(ctx context.Context, _ *pluginpb.Empty) (*pluginpb.Empty, error) {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return &pluginpb.Empty{}, nil
	}
	s.shutdown = true
	httpSrv := s.http
	s.http = nil
	s.mu.Unlock()

	if httpSrv != nil {
		if err := httpSrv.Shutdown(ctx); err != nil {
			s.log.Warn("控制面停止未完成", "error", err)
		}
	}
	s.closeConn()
	s.log.Info("适配器已关闭")
	return &pluginpb.Empty{}, nil
}

// emit 把一条文本包装成统一事件并经核心 BotService.EmitEvent 投递。
func (s *service) emit(ctx context.Context, inst *instanceState, text string) (string, error) {
	s.mu.Lock()
	client, token := s.client, s.token
	s.mu.Unlock()
	if client == nil {
		return "", errors.New("尚未初始化，无法投递事件")
	}

	ev := &bot.Event{
		ID:       fmt.Sprintf("%s-%d", inst.platform, time.Now().UnixNano()),
		Type:     bot.EventMessage,
		Platform: inst.platform,
		BotID:    inst.botID,
		Time:     time.Now().UTC(),
		Sender:   &bot.User{ID: "example-user", Name: "Example User"},
		Channel:  &bot.Channel{ID: "example-group", Name: "Example Group", Kind: bot.MessageGroup},
		Message: &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
		}},
	}

	resp, err := client.EmitEvent(ctx, &pluginpb.EmitEventRequest{Token: token, Event: grpcsrv.EventToProto(ev)})
	if err != nil {
		return "", fmt.Errorf("EmitEvent: %w", err)
	}
	if !resp.GetOk() {
		return "", fmt.Errorf("核心拒绝事件: %s", resp.GetError())
	}
	return ev.ID, nil
}

// instance 返回指定 bot 的实例状态。
func (s *service) instance(botID string) (*instanceState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[botID]
	return inst, ok
}

// handler 构造 HTTP 控制面，仅用于演示与测试。
func (s *service) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/inject", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			BotID string `json:"bot_id"`
			Text  string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("请求体非法: %v", err), http.StatusBadRequest)
			return
		}
		inst, ok := s.instance(req.BotID)
		if !ok {
			http.Error(w, fmt.Sprintf("未知 bot %q", req.BotID), http.StatusBadRequest)
			return
		}
		id, err := s.emit(r.Context(), inst, req.Text)
		if err != nil {
			s.log.Warn("投递事件失败", "bot", req.BotID, "error", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"event_id": id})
	})

	mux.HandleFunc("/sent", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
			return
		}
		botID := r.URL.Query().Get("bot_id")
		var out []sendRecord
		s.mu.Lock()
		for id, inst := range s.instances {
			if botID != "" && botID != id {
				continue
			}
			out = append(out, inst.snapshot()...)
		}
		s.mu.Unlock()
		if out == nil {
			out = []sendRecord{}
		}
		writeJSON(w, http.StatusOK, out)
	})

	return mux
}

// startControl 启动 HTTP 控制面并返回实际监听地址。
func (s *service) startControl(logger *slog.Logger) (string, error) {
	ln, err := net.Listen("tcp", s.controlAddr)
	if err != nil {
		return "", fmt.Errorf("控制面监听 %s: %w", s.controlAddr, err)
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	s.mu.Lock()
	s.http = srv
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("控制面异常退出", "error", err)
		}
	}()
	return ln.Addr().String(), nil
}

// closeConn 关闭与核心的连接。
func (s *service) closeConn() {
	s.mu.Lock()
	conn := s.conn
	s.conn, s.client = nil, nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func main() {
	listen := flag.String("listen", defaultListen, "AdapterService 监听地址")
	platform := flag.String("platform", defaultPlatform, "上报的平台名")
	control := flag.String("control", defaultControl, "HTTP 控制面监听地址")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	svc := newService(logger, *platform, *control)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Error("监听失败", "addr", *listen, "error", err)
		os.Exit(1)
	}
	grpcSrv := grpc.NewServer()
	pluginpb.RegisterAdapterServiceServer(grpcSrv, svc)

	controlAddr, err := svc.startControl(logger)
	if err != nil {
		logger.Error("控制面启动失败", "error", err)
		os.Exit(1)
	}

	logger.Info("外部适配器示例已启动",
		"adapter_service", ln.Addr().String(),
		"control", controlAddr,
		"platform", *platform,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = grpcSrv.Serve(ln)
	}()

	<-ctx.Done()
	logger.Info("收到退出信号，开始关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// 与核心的 Shutdown 同一收尾路径：停止控制面并释放连接。
	if _, err := svc.Shutdown(shutdownCtx, &pluginpb.Empty{}); err != nil {
		logger.Warn("关闭失败", "error", err)
	}
	grpcSrv.GracefulStop()
	<-serveDone
	logger.Info("已退出")
}

// decodeSettings 解析核心下发的 JSON 配置；空串表示没有配置。
func decodeSettings(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("配置 JSON 非法: %w", err)
	}
	return out, nil
}

// target 把监听地址包装为 gRPC target 字符串。
func target(addr string) string {
	if strings.Contains(addr, "://") {
		return addr
	}
	return "passthrough:///" + addr
}

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
