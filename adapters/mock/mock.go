// Package mock 提供用于本地测试与事件回放的适配器。
//
// 它不连接任何真实平台：事件通过 Inject/InjectText 或 HTTP 控制面
// (POST /inject) 手动注入，发送的消息被记录在内存中供断言与观察
// (GET /sent)。集成测试与本地联调都使用它。
package mock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

const (
	defaultPlatform = "mock"
	defaultBotID    = "mock"
	maxBodyBytes    = 1 << 20
)

// Options 是 Mock 适配器的构造参数。
type Options struct {
	// Name 是 bot 名称，作为 bot.Event.BotID，默认 "mock"。
	Name string
	// Platform 是平台名，默认 "mock"。
	Platform string
	// ListenAddr 非空时启动 HTTP 控制面，例如 "127.0.0.1:0"。
	//
	// 控制面没有鉴权（任何能访问它的调用方都能注入事件、清空发送记录），
	// 因此默认只接受回环地址（127.0.0.1 / ::1 / localhost）；其它地址会导致
	// Start 失败，确需监听时设置 AllowRemoteControlPlane 显式放行。
	ListenAddr string
	// AllowRemoteControlPlane 允许控制面监听非回环地址。
	//
	// 仅在明确知道控制面暴露面（例如容器内网）时使用；默认关闭。
	AllowRemoteControlPlane bool
	// Logger 可为 nil。
	Logger *slog.Logger
	// Capabilities 为零值时表示全能力，便于验证降级路径之外的行为。
	Capabilities bot.Capabilities
}

// SendRecord 记录一次发送的完整信息。
type SendRecord struct {
	// Time 是发送时间。
	Time time.Time
	// Request 是发送请求。
	Request *bot.SendRequest
	// Result 是发送结果，失败时为 nil。
	Result *bot.SendResult
	// Err 是失败原因，成功时为空。
	Err string
}

// Adapter 是 Mock 适配器。
type Adapter struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	sink    bot.EventSink
	started bool
	stopped bool
	seq     atomic.Uint64
	records []*SendRecord
	srv     *http.Server
	ln      net.Listener
	addr    string
}

// 确保 Adapter 满足 bot.Adapter。
var _ bot.Adapter = (*Adapter)(nil)

// New 构造 Mock 适配器。
func New(opts Options) *Adapter {
	if opts.Name == "" {
		opts.Name = defaultBotID
	}
	if opts.Platform == "" {
		opts.Platform = defaultPlatform
	}
	opts.ListenAddr = strings.TrimSpace(opts.ListenAddr)
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Capabilities == (bot.Capabilities{}) {
		opts.Capabilities = bot.Capabilities{
			Text: true, Markdown: true, Image: true, At: true,
			Card: true, File: true, Reply: true, Private: true, Group: true,
		}
	}
	return &Adapter{opts: opts, log: opts.Logger}
}

// Name 返回平台名。
func (a *Adapter) Name() string { return a.opts.Platform }

// Capabilities 返回平台能力。
func (a *Adapter) Capabilities() bot.Capabilities { return a.opts.Capabilities }

// Addr 返回 HTTP 控制面实际监听地址，未启动时为 ""。
func (a *Adapter) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addr
}

// Start 记录事件出口，可选启动 HTTP 控制面，然后阻塞直到 ctx 结束。
//
// 只有控制面监听成功（或未配置控制面）后才置为运行状态：监听失败时
// Started 仍为 false，调用方可以换地址重试。控制面地址校验见 Options.ListenAddr。
func (a *Adapter) Start(ctx context.Context, sink bot.EventSink) error {
	if sink == nil {
		return errors.New("mock: nil sink")
	}
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return errors.New("mock: already started")
	}
	if err := a.checkControlPlaneAddr(); err != nil {
		a.mu.Unlock()
		return err
	}

	var (
		ln  net.Listener
		srv *http.Server
	)
	if a.opts.ListenAddr != "" {
		var err error
		ln, err = net.Listen("tcp", a.opts.ListenAddr)
		if err != nil {
			a.mu.Unlock()
			return fmt.Errorf("mock: listen %s: %w", a.opts.ListenAddr, err)
		}
		srv = &http.Server{
			Handler:           a.handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		a.ln = ln
		a.srv = srv
		a.addr = ln.Addr().String()
	}

	a.started = true
	a.stopped = false
	a.sink = sink
	addr := a.addr
	a.mu.Unlock()

	if srv != nil {
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.log.Error("mock control plane stopped", "error", err)
			}
		}()
		a.log.Info("mock control plane listening", "addr", addr)
	}

	<-ctx.Done()
	return nil
}

// checkControlPlaneAddr 校验控制面监听地址：默认只允许回环地址。
//
// 控制面无鉴权，注入事件可触发命令执行，因此非回环地址必须显式 opt-in。
func (a *Adapter) checkControlPlaneAddr() error {
	if a.opts.ListenAddr == "" || a.opts.AllowRemoteControlPlane {
		return nil
	}
	if listenAddrIsLoopback(a.opts.ListenAddr) {
		return nil
	}
	return fmt.Errorf("mock: 控制面监听地址 %q 不是回环地址（控制面无鉴权，可注入事件）；确需监听请设置 allow_remote_control_plane", a.opts.ListenAddr)
}

// listenAddrIsLoopback 判断监听地址是否限定在回环接口上。
//
// 空主机名（如 ":18080"）表示监听所有接口，与具体网卡地址一样按非回环处理；
// 主机名只认 "localhost"，其它名字即使解析到回环也不放行（避免依赖解析结果）。
func listenAddrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Stop 关闭 HTTP 控制面，幂等。
func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	srv := a.srv
	a.stopped = true
	a.mu.Unlock()

	if srv == nil {
		return nil
	}
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("mock: shutdown: %w", err)
	}
	return nil
}

// Started 返回适配器是否已进入运行状态，供测试等待引擎就绪。
func (a *Adapter) Started() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started && !a.stopped
}

// Send 记录发送请求并返回构造的消息 ID，不产生任何网络调用。
func (a *Adapter) Send(ctx context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("mock: nil send request")
	}
	res := &bot.SendResult{MessageID: fmt.Sprintf("mock-%d", a.seq.Add(1))}

	a.mu.Lock()
	a.records = append(a.records, &SendRecord{
		Time:    time.Now().UTC(),
		Request: req,
		Result:  res,
	})
	a.mu.Unlock()
	return res, nil
}

// Inject 注入一个事件，缺失字段会补默认值。
func (a *Adapter) Inject(ctx context.Context, ev *bot.Event) error {
	if ev == nil {
		return errors.New("mock: nil event")
	}
	a.mu.Lock()
	sink := a.sink
	stopped := a.stopped || sink == nil
	botID := a.opts.Name
	platform := a.opts.Platform
	a.mu.Unlock()

	if stopped {
		return errors.New("mock: adapter is not running")
	}
	a.fillDefaults(ev, botID, platform)
	return sink.Emit(ctx, ev)
}

// InjectText 注入一条仅含文本的消息事件，并返回构造出的事件。
func (a *Adapter) InjectText(ctx context.Context, text string) (*bot.Event, error) {
	ev := a.newEvent(text, "", "", "", bot.MessageGroup)
	if err := a.Inject(ctx, ev); err != nil {
		return nil, err
	}
	return ev, nil
}

// Sent 返回已发送请求的快照。
func (a *Adapter) Sent() []*bot.SendRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*bot.SendRequest, 0, len(a.records))
	for _, r := range a.records {
		out = append(out, r.Request)
	}
	return out
}

// Records 返回发送记录快照。
func (a *Adapter) Records() []*SendRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*SendRecord(nil), a.records...)
}

// Reset 清空已发送记录。
func (a *Adapter) Reset() {
	a.mu.Lock()
	a.records = nil
	a.mu.Unlock()
}

// WaitSent 阻塞等待至少 n 条发送记录，超时返回 false。
func (a *Adapter) WaitSent(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		a.mu.Lock()
		count := len(a.records)
		a.mu.Unlock()
		if count >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (a *Adapter) fillDefaults(ev *bot.Event, botID, platform string) {
	if ev.Platform == "" {
		ev.Platform = platform
	}
	if ev.BotID == "" {
		ev.BotID = botID
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if ev.Type == "" {
		ev.Type = bot.EventMessage
	}
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("%s-%d", platform, time.Now().UnixNano())
	}
	if ev.Sender == nil {
		ev.Sender = &bot.User{ID: "mock-user", Name: "Mock User"}
	}
	if ev.Message == nil {
		ev.Message = &bot.Message{Kind: bot.MessagePrivate, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: ""}},
		}}
	}
	if ev.Message.Kind == "" {
		ev.Message.Kind = bot.MessagePrivate
	}
	if ev.Message.Kind == bot.MessageGroup && ev.Channel == nil {
		ev.Channel = &bot.Channel{ID: "mock-group", Name: "Mock Group", Kind: bot.MessageGroup}
	}
}

func (a *Adapter) newEvent(text, userID, userName, channelID string, kind bot.MessageKind) *bot.Event {
	if userID == "" {
		userID = "mock-user"
	}
	if userName == "" {
		userName = "Mock User"
	}
	ev := &bot.Event{
		Type:    bot.EventMessage,
		Sender:  &bot.User{ID: userID, Name: userName},
		Message: &bot.Message{Kind: kind, Segments: []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}}}},
	}
	if kind != bot.MessagePrivate {
		if channelID == "" {
			channelID = "mock-group"
		}
		ev.Channel = &bot.Channel{ID: channelID, Kind: kind}
	}
	return ev
}

// parseKind 把控制面提交的 kind 归一为受支持的会话类型。
//
// 空值默认群聊；未知取值直接报错，避免拼错的 kind 被静默当成群聊、
// 让集成测试验证到错误的代码路径。
func parseKind(raw string) (bot.MessageKind, error) {
	switch kind := bot.MessageKind(strings.TrimSpace(raw)); kind {
	case "":
		return bot.MessageGroup, nil
	case bot.MessagePrivate, bot.MessageGroup, bot.MessageChannel:
		return kind, nil
	default:
		return "", fmt.Errorf("mock: 未知的 kind %q，可用值：private/group/channel", raw)
	}
}

// injectRequest 是 HTTP 控制面接受的事件描述。
type injectRequest struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
	ChannelID string `json:"channel_id"`
	Kind      string `json:"kind"`
	Platform  string `json:"platform"`
	BotID     string `json:"bot_id"`
}

func (a *Adapter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /inject", a.handleInject)
	mux.HandleFunc("GET /sent", a.handleSent)
	mux.HandleFunc("DELETE /sent", a.handleSent)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	return mux
}

func (a *Adapter) handleInject(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req injectRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	kind, err := parseKind(req.Kind)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ev := a.newEvent(req.Text, req.UserID, req.UserName, req.ChannelID, kind)
	ev.ID = req.ID
	ev.Platform = req.Platform
	ev.BotID = req.BotID
	if req.Type != "" {
		ev.Type = bot.EventType(req.Type)
	}
	ev.Raw = json.RawMessage(body)

	if err := a.Inject(r.Context(), ev); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"event_id": ev.ID})
}

func (a *Adapter) handleSent(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		a.Reset()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.mu.Lock()
	records := append([]*SendRecord(nil), a.records...)
	a.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(records)
}
