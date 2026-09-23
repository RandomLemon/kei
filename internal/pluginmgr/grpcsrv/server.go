// Package grpcsrv 实现外部插件 gRPC 协议中「核心侧」的服务端。
//
// 本包只实现 proto/plugin.proto 中的 BotService（供外部插件反向调用核心），
// 不实现 PluginService（那是外部插件进程自身的服务，由插件加载器作为客户端调用）。
//
// 数据流：
//
//	外部插件 --BotService.SendMessage--> 本包 --bot.BotAPI.Send--> 适配器 --> 平台
package grpcsrv

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

// TokenInfo 描述一个插件令牌对应的身份与权限。
type TokenInfo struct {
	// Plugin 是 token 对应的插件名，也是其在 Configs 中的键。
	Plugin string
	// Permissions 是该插件被授予的权限。
	Permissions []bot.Permission
}

// Options 是 Server 的构造参数。
type Options struct {
	// Addr 是监听地址，支持 ":0" 表示由系统分配端口，必填。
	Addr string
	// Tokens 是 token 到插件信息的映射，非空 token 才能通过认证。
	Tokens map[string]TokenInfo
	// Bot 是 SendMessage 的落地实现，必填。
	Bot bot.BotAPI
	// Configs 是插件名到配置的映射，供 GetConfig 读取，可为 nil。
	Configs map[string]*bot.Config
	// Logger 是日志器，nil 时使用 slog.Default()。
	Logger *slog.Logger
	// TLS 非 nil 时启用 TLS；mTLS 由调用方在 tls.Config 中配置 ClientAuth。
	TLS *tls.Config
}

// Server 是 BotService 的 gRPC 服务端。
//
// Server 持有并管理自己的 grpc.Server；Start 非阻塞，Stop 幂等。
// Server 可安全地被多个 goroutine 并发调用。
type Server struct {
	addr     string
	tokens   map[string]TokenInfo
	configs  map[string]*bot.Config
	botAPI   bot.BotAPI
	logger   *slog.Logger
	tlsConf  *tls.Config
	listener net.Listener

	grpc *grpc.Server

	mu      sync.Mutex
	ln      net.Listener
	started bool
	stopped bool
	stopOne sync.Once
	stopErr error
}

// New 构造 Server，校验 Options 并完成服务注册。
//
// Addr 为空或 Bot 为 nil 时返回错误；Tokens 与 Configs 会被复制，
// 调用方后续修改原映射不影响已构造的 Server。
func New(opts Options) (*Server, error) {
	return newServer(opts, nil)
}

// newServer 是 New 的内部实现。ln 非 nil 时直接复用该监听器（供 bufconn 测试注入），
// 此时忽略 opts.Addr；否则 Start 时按 opts.Addr 监听。
func newServer(opts Options, ln net.Listener) (*Server, error) {
	if opts.Addr == "" && ln == nil {
		return nil, errors.New("pluginmgr/grpcsrv: Options.Addr 不能为空")
	}
	if opts.Bot == nil {
		return nil, errors.New("pluginmgr/grpcsrv: Options.Bot 不能为空")
	}

	tokens := make(map[string]TokenInfo, len(opts.Tokens))
	for k, v := range opts.Tokens {
		v.Permissions = append([]bot.Permission(nil), v.Permissions...)
		tokens[k] = v
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	configs := make(map[string]*bot.Config, len(opts.Configs))
	for k, v := range opts.Configs {
		configs[k] = v
	}

	s := &Server{
		addr:     opts.Addr,
		tokens:   tokens,
		configs:  configs,
		botAPI:   opts.Bot,
		logger:   logger,
		tlsConf:  opts.TLS,
		listener: ln,
	}

	var serverOpts []grpc.ServerOption
	if opts.TLS != nil {
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(opts.TLS)))
	}
	s.grpc = grpc.NewServer(serverOpts...)
	pluginpb.RegisterBotServiceServer(s.grpc, &botService{srv: s})
	return s, nil
}

// Addr 返回实际监听地址。
//
// Start 之前返回空串；Start 之后返回真实地址（":0" 会解析为具体端口）。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Start 在后台 goroutine 中开始服务，非阻塞。
//
// 监听器与后端 grpc.Server 在返回前已就绪，因此 Start 成功返回后 Addr() 必定可用。
// 重复调用返回错误；启动后必须调用 Stop 释放监听器与 goroutine。
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("pluginmgr/grpcsrv: Server 已停止，不可重新启动")
	}
	if s.started {
		return errors.New("pluginmgr/grpcsrv: Server 已启动")
	}

	ln := s.listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", s.addr)
		if err != nil {
			return fmt.Errorf("pluginmgr/grpcsrv: 监听 %s: %w", s.addr, err)
		}
	}
	s.ln = ln
	s.started = true

	s.logger.Info("gRPC 插件服务端已启动",
		"addr", ln.Addr().String(),
		"tls", s.tlsConf != nil,
		"tokens", len(s.tokens),
	)

	go func() {
		// Serve 在 Stop/GracefulStop 触发后返回，并自行关闭监听 socket；
		// 该 goroutine 因此必然退出，不会泄漏。
		if err := s.grpc.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.logger.Error("gRPC 插件服务端异常退出", "err", err)
		}
	}()
	return nil
}

// Stop 优雅停止服务，可重复调用且并发安全。
//
// ctx 超时后立即强制停止，避免 GracefulStop 被未完成的 RPC 无限拖住；
// 此时返回 ctx.Err() 作为提示，但服务确实已停止。
func (s *Server) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.stopOne.Do(func() {
		s.mu.Lock()
		started := s.started
		s.stopped = true
		s.mu.Unlock()

		// 未曾 Start 的 Server 没有后台 goroutine；若已注入监听器则回收它，
		// 保证 Stop 之后不会残留可连接的 socket。
		if !started {
			if s.listener != nil {
				if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					s.stopErr = err
				}
			}
			return
		}
		if ctx.Err() != nil {
			s.grpc.Stop()
			s.stopErr = ctx.Err()
			return
		}

		done := make(chan struct{})
		go func() {
			s.grpc.GracefulStop()
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			s.grpc.Stop()
			<-done
			s.stopErr = ctx.Err()
		}
		// GracefulStop/Stop 已关闭全部监听 socket，Serve goroutine 随之退出，
		// 本包无需也不能重复关闭监听器。
	})
	return s.stopErr
}

// botService 是 BotService 的服务实现，复用 Server 上的令牌与依赖。
type botService struct {
	pluginpb.UnimplementedBotServiceServer
	srv *Server
}

// authenticate 校验 token 并返回对应插件信息；未知或空 token 返回 Unauthenticated。
func (s *Server) authenticate(token string) (TokenInfo, error) {
	info, ok := s.tokens[token]
	if !ok {
		return TokenInfo{}, status.Error(codes.Unauthenticated, "未认证的插件令牌")
	}
	return info, nil
}

// requestSender 是 bot.BotAPI 的可选扩展：支持发出带引用回复的完整发送请求。
//
// 引擎（internal/engine）实现了该方法；外部插件加载器只需传入引擎实例即可获得
// reply_to 能力。bot.BotAPI 未实现该接口时自动退化为 bot.BotAPI.Send。
type requestSender interface {
	// SendRequest 发送一条完整的发送请求（含引用回复）。
	SendRequest(ctx context.Context, req *bot.SendRequest) (*bot.SendResult, error)
}

// SendMessage 以插件身份发送消息。
//
// reply_to 非空表示引用回复：当 Bot 实现了 requestSender 时走完整的
// bot.SendRequest（可携带 ReplyTo），否则退化为 bot.Send，不视为错误。
// target.bot_id 用于在同平台多机器人部署中指定实例，两条发送路径都会透传。
//
// 失败语义：认证、权限、参数错误以 status error 返回；平台发送失败（发送接口报错）
// 走 SendResponse.Error，因为这是业务失败而非 RPC 失败，插件应在自己的逻辑中处理，
// 而不是把整个调用当作传输错误重试。
func (b *botService) SendMessage(ctx context.Context, req *pluginpb.SendRequest) (*pluginpb.SendResponse, error) {
	info, err := b.srv.authenticate(req.GetToken())
	if err != nil {
		return nil, err
	}
	meta := bot.Metadata{Permissions: info.Permissions}
	if !meta.HasPermission(bot.PermSendMessage) {
		return nil, status.Errorf(codes.PermissionDenied,
			"插件 %s 缺少 %s 权限", info.Plugin, bot.PermSendMessage)
	}

	msg, err := messageFromProto(req.GetMessage())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "消息格式非法: %v", err)
	}
	target := TargetFromProto(req.GetTarget())
	if target.ChannelID == "" && target.UserID == "" {
		return nil, status.Error(codes.InvalidArgument, "Target 必须指定 channel_id 或 user_id")
	}

	res, err := b.send(ctx, target, msg, req.GetReplyTo())
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// RPC 自身已被取消/超时，按传输失败上报，交由 gRPC 结束调用。
			return nil, status.FromContextError(ctxErr).Err()
		}
		b.srv.logger.Warn("插件发送消息失败",
			"plugin", info.Plugin,
			"platform", target.Platform,
			"channel_id", target.ChannelID,
			"user_id", target.UserID,
			"err", err,
		)
		return &pluginpb.SendResponse{Error: err.Error()}, nil
	}
	var messageID string
	if res != nil {
		messageID = res.MessageID
	}
	return &pluginpb.SendResponse{MessageId: messageID}, nil
}

// send 按 replyTo 是否为空选择发送路径。
//
// replyTo 为空时行为与历史版本完全一致（直接 bot.Send）；非空时优先使用
// 实现了 requestSender 的 Bot，以便把引用回复交给引擎统一处理。
func (b *botService) send(ctx context.Context, target bot.Target, msg *bot.Message, replyTo string) (*bot.SendResult, error) {
	if replyTo == "" {
		return b.srv.botAPI.Send(ctx, target, msg)
	}
	sender, ok := b.srv.botAPI.(requestSender)
	if !ok {
		// Bot 未提供扩展能力时退回普通发送：插件仍能收到消息，
		// 只是引用关系丢失，因此不视为错误。
		b.srv.logger.Debug("Bot 未实现 requestSender，reply_to 退化为普通发送",
			"platform", target.Platform,
			"reply_to", replyTo,
		)
		return b.srv.botAPI.Send(ctx, target, msg)
	}
	// bot.BotAPI.Send 已经接收带 BotID 的 bot.Target，SendRequest 路径再显式
	// 传一份 BotID，保证同平台多机器人部署下两条路径都能指定实例。
	return sender.SendRequest(ctx, &bot.SendRequest{
		BotID:   target.BotID,
		Target:  target,
		Message: msg,
		ReplyTo: replyTo,
	})
}

// GetConfig 读取插件自身的配置。
//
// key 为空表示读取整份配置；键或插件配置缺失时返回 found=false。
func (b *botService) GetConfig(_ context.Context, req *pluginpb.GetConfigRequest) (*pluginpb.GetConfigResponse, error) {
	info, err := b.srv.authenticate(req.GetToken())
	if err != nil {
		return nil, err
	}

	cfg := b.srv.configs[info.Plugin]
	key := req.GetKey()
	var value any
	if key == "" {
		raw := cfg.Raw()
		if raw == nil {
			return &pluginpb.GetConfigResponse{}, nil
		}
		value = raw
	} else {
		v, ok := cfg.Get(key)
		if !ok {
			return &pluginpb.GetConfigResponse{}, nil
		}
		value = v
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "配置序列化失败: %v", err)
	}
	return &pluginpb.GetConfigResponse{Found: true, ValueJson: string(encoded)}, nil
}

// Log 把插件日志写入核心日志器，并绑定 plugin 字段。
//
// 未知 level 按 info 处理；fields_json 解析失败时降级为原始字符串字段，不报错。
func (b *botService) Log(ctx context.Context, req *pluginpb.LogRequest) (*pluginpb.Empty, error) {
	info, err := b.srv.authenticate(req.GetToken())
	if err != nil {
		return nil, err
	}

	level := slog.LevelInfo
	switch req.GetLevel() {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	logger := b.srv.logger.With("plugin", info.Plugin)
	switch fields := req.GetFieldsJson(); fields {
	case "":
		logger.Log(ctx, level, req.GetMessage())
	default:
		var attrs map[string]any
		if err := json.Unmarshal([]byte(fields), &attrs); err != nil {
			logger.Log(ctx, level, req.GetMessage(), "fields_raw", fields)
			break
		}
		logger.Log(ctx, level, req.GetMessage(), "fields", attrs)
	}
	return &pluginpb.Empty{}, nil
}

// EventToProto 把统一事件转换为 proto 事件，供外部插件加载器复用。
//
// Time 为零值时 time_unix_ms 为 0；Message/Sender/Channel/Command 为空时对应字段缺省。
// Raw 与消息段的 Data 以 JSON 字符串承载：nil 编码为空串，空映射编码为 "{}"，
// 因此两者经 EventFromProto 往返后仍可区分。
func EventToProto(ev *bot.Event) *pluginpb.Event {
	if ev == nil {
		return nil
	}
	out := &pluginpb.Event{
		Id:       ev.ID,
		Type:     string(ev.Type),
		Platform: ev.Platform,
		BotId:    ev.BotID,
		Message:  messageToProto(ev.Message),
		RawJson:  encodeJSON(ev.Raw),
	}
	if !ev.Time.IsZero() {
		out.TimeUnixMs = ev.Time.UTC().UnixMilli()
	}
	if ev.Sender != nil {
		out.Sender = &pluginpb.User{Id: ev.Sender.ID, Name: ev.Sender.Name, IsBot: ev.Sender.IsBot}
	}
	if ev.Channel != nil {
		out.Channel = &pluginpb.Channel{Id: ev.Channel.ID, Name: ev.Channel.Name, Kind: string(ev.Channel.Kind)}
	}
	if ev.Command != nil {
		out.Command = &pluginpb.Command{
			Name: ev.Command.Name,
			Raw:  ev.Command.Raw,
			Args: append([]string(nil), ev.Command.Args...),
		}
	}
	return out
}

// EventFromProto 把 proto 事件还原为统一事件，供外部插件加载器复用。
//
// raw_json 与 data_json 解析失败时对应字段留空/为 nil，不返回错误，
// 以免单条异常事件打断整条处理链路。
func EventFromProto(ev *pluginpb.Event) *bot.Event {
	if ev == nil {
		return nil
	}
	out := &bot.Event{
		ID:       ev.GetId(),
		Type:     bot.EventType(ev.GetType()),
		Platform: ev.GetPlatform(),
		BotID:    ev.GetBotId(),
		Message:  messageFromProtoLossy(ev.GetMessage()),
		Raw:      decodeJSON(ev.GetRawJson()),
	}
	if ms := ev.GetTimeUnixMs(); ms != 0 {
		out.Time = time.UnixMilli(ms).UTC()
	}
	if u := ev.GetSender(); u != nil {
		out.Sender = &bot.User{ID: u.GetId(), Name: u.GetName(), IsBot: u.GetIsBot()}
	}
	if c := ev.GetChannel(); c != nil {
		out.Channel = &bot.Channel{ID: c.GetId(), Name: c.GetName(), Kind: bot.MessageKind(c.GetKind())}
	}
	if c := ev.GetCommand(); c != nil {
		out.Command = &bot.Command{
			Name: c.GetName(),
			Raw:  c.GetRaw(),
			Args: append([]string(nil), c.GetArgs()...),
		}
	}
	return out
}

// messageToProto 转换消息段集合；msg 为 nil 时返回 nil。
func messageToProto(msg *bot.Message) *pluginpb.Message {
	if msg == nil {
		return nil
	}
	out := &pluginpb.Message{Id: msg.ID, Kind: string(msg.Kind)}
	if len(msg.Segments) > 0 {
		segs := make([]*pluginpb.Segment, 0, len(msg.Segments))
		for _, seg := range msg.Segments {
			segs = append(segs, &pluginpb.Segment{
				Type:     string(seg.Type),
				DataJson: encodeJSON(seg.Data),
			})
		}
		out.Segments = segs
	}
	return out
}

// messageFromProto 转换消息段集合，段数据类型非法时返回错误。
func messageFromProto(msg *pluginpb.Message) (*bot.Message, error) {
	if msg == nil {
		return nil, nil
	}
	out := &bot.Message{ID: msg.GetId(), Kind: bot.MessageKind(msg.GetKind())}
	if n := len(msg.GetSegments()); n > 0 {
		segs := make([]bot.Segment, 0, n)
		for _, seg := range msg.GetSegments() {
			data, err := segmentData(seg)
			if err != nil {
				return nil, err
			}
			segs = append(segs, bot.Segment{Type: bot.SegmentType(seg.GetType()), Data: data})
		}
		out.Segments = segs
	}
	return out, nil
}

// messageFromProtoLossy 是 messageFromProto 的容错版本，用于事件反序列化路径。
func messageFromProtoLossy(msg *pluginpb.Message) *bot.Message {
	out, err := messageFromProto(msg)
	if err != nil {
		return nil
	}
	return out
}

// segmentData 解析单个消息段的 data_json。
func segmentData(seg *pluginpb.Segment) (map[string]any, error) {
	raw := seg.GetDataJson()
	if raw == "" {
		return nil, nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("段 %q 的 data_json: %w", seg.GetType(), err)
	}
	return data, nil
}

// TargetToProto 把发送目标转换为 proto 目标，供外部插件加载器复用。
//
// t 为 nil 时返回 nil；BotID 为空表示由核心按平台自动选择机器人实例。
func TargetToProto(t *bot.Target) *pluginpb.Target {
	if t == nil {
		return nil
	}
	return &pluginpb.Target{
		Platform:  t.Platform,
		BotId:     t.BotID,
		ChannelId: t.ChannelID,
		UserId:    t.UserID,
		Kind:      string(t.Kind),
	}
}

// TargetFromProto 把 proto 目标还原为 bot.Target，供外部插件加载器复用。
//
// target 为 nil 时返回零值；BotID 为空表示由核心按平台自动选择机器人实例。
func TargetFromProto(t *pluginpb.Target) bot.Target {
	if t == nil {
		return bot.Target{}
	}
	return bot.Target{
		Platform:  t.GetPlatform(),
		BotID:     t.GetBotId(),
		ChannelID: t.GetChannelId(),
		UserID:    t.GetUserId(),
		Kind:      bot.MessageKind(t.GetKind()),
	}
}

// encodeJSON 把任意值编码为 JSON 字符串。
//
// nil 与无法编码的值返回空串；空映射(非 nil)编码为 "{}"，
// 以保证 nil 与空映射经 decodeJSON 往返后仍可区分。
func encodeJSON(v any) string {
	if v == nil {
		return ""
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// decodeJSON 解析 JSON 字符串；空串或解析失败返回 nil。
func decodeJSON(s string) any {
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	return v
}
