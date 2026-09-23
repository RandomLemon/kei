// Package feishu 实现飞书（Lark）平台适配器。
//
// 适配器通过事件订阅 HTTP 回调接收事件：先校验签名与 Verification Token，
// 必要时解密加密事件，再快速 ACK 并把事件异步投递给 bot.EventSink；
// 发送侧使用 tenant_access_token 调用开放平台 IM 接口。
//
// 本包只依赖标准库与 pkg/bot。
package feishu

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

// 平台名与默认配置。
const (
	// PlatformName 是适配器对外声明的平台名。
	PlatformName = "feishu"
	// defaultPath 是回调路径默认值。
	defaultPath = "/feishu/event"
	// defaultBaseURL 是飞书开放平台默认地址。
	defaultBaseURL = "https://open.feishu.cn"
	// defaultHTTPTimeout 是默认 HTTP 客户端超时。
	defaultHTTPTimeout = 10 * time.Second
	// eventQueueCapacity 是事件异步投递队列容量，队列满时丢弃事件并告警。
	eventQueueCapacity = 1024
	// eventWorkers 是异步投递的 worker 数量。
	eventWorkers = 2
	// shutdownTimeout 是退出时 http.Server.Shutdown 的最长等待时间。
	shutdownTimeout = 5 * time.Second
	// drainTimeout 是退出前投递队列剩余事件的时间预算。
	drainTimeout = 5 * time.Second
	// maxBodyBytes 限制回调 body 大小，避免超大请求耗尽内存。
	maxBodyBytes = 4 << 20
)

// Options 是飞书适配器的配置。
type Options struct {
	// Name 是 bot 名称，写入 bot.Event.BotID；必填。
	Name string
	// AppID 是飞书应用 App ID；必填。
	AppID string
	// AppSecret 是飞书应用 App Secret；必填。
	AppSecret string
	// VerificationToken 是事件订阅的 Verification Token；必填。
	VerificationToken string
	// EncryptKey 是事件加密 key；可选，非空时校验签名并解密加密事件。
	EncryptKey string
	// ListenAddr 是回调监听地址，例如 "127.0.0.1:8080"；必填。
	ListenAddr string
	// Path 是回调路径；默认 "/feishu/event"。
	Path string
	// BaseURL 是开放平台地址；默认 "https://open.feishu.cn"。
	BaseURL string
	// HTTPClient 是调用开放平台使用的客户端；nil 时使用 10s 超时的默认客户端。
	HTTPClient *http.Client
	// Logger 是日志器；nil 时使用 slog.Default()。
	Logger *slog.Logger
}

// Adapter 是飞书平台适配器，实现 bot.Adapter。
type Adapter struct {
	opts   Options
	log    *slog.Logger
	client *http.Client

	// queue 是事件异步投递队列；容量固定，满时丢弃事件，不阻塞回调。
	queue chan *bot.Event

	// mu 保护监听状态；RWMutex 允许并发读取 Addr。
	mu      sync.RWMutex
	server  *http.Server
	ln      net.Listener
	addr    string
	cancel  context.CancelFunc
	started bool
	// wg 跟踪异步投递 worker，退出前必须等待其结束，避免 goroutine 泄漏。
	wg sync.WaitGroup

	// tokMu 保护 tenant_access_token 缓存；tokRefresh 串行化刷新（single-flight）。
	tokMu      sync.RWMutex
	tokRefresh sync.Mutex
	tok        string
	tokExpire  time.Time

	// seq 为缺少平台事件 ID 时生成兜底 ID 使用。
	seq atomic.Uint64
}

// 错误哨兵。
var (
	// errNilSink 表示 Start 未收到事件投递出口。
	errNilSink = errors.New("feishu: sink 不能为空")
)

// 编译期确认 Adapter 满足 bot.Adapter。
var _ bot.Adapter = (*Adapter)(nil)

// New 创建飞书适配器，校验必填配置并填充默认值。
func New(opts Options) (*Adapter, error) {
	switch {
	case opts.Name == "":
		return nil, errors.New("feishu: Name 不能为空")
	case opts.AppID == "":
		return nil, errors.New("feishu: AppID 不能为空")
	case opts.AppSecret == "":
		return nil, errors.New("feishu: AppSecret 不能为空")
	case opts.VerificationToken == "":
		return nil, errors.New("feishu: VerificationToken 不能为空")
	case opts.ListenAddr == "":
		return nil, errors.New("feishu: ListenAddr 不能为空")
	}
	if opts.Path == "" {
		opts.Path = defaultPath
	}
	if !strings.HasPrefix(opts.Path, "/") {
		return nil, errors.New(`feishu: Path 必须以 "/" 开头`)
	}
	if opts.BaseURL == "" {
		opts.BaseURL = defaultBaseURL
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Adapter{
		opts:   opts,
		log:    opts.Logger,
		client: opts.HTTPClient,
		queue:  make(chan *bot.Event, eventQueueCapacity),
	}, nil
}

// Name 返回平台名 "feishu"。
func (a *Adapter) Name() string { return PlatformName }

// Addr 返回当前回调监听地址；未启动或已停止时为空串。
func (a *Adapter) Addr() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.addr
}

// Capabilities 返回飞书支持的消息能力。
func (a *Adapter) Capabilities() bot.Capabilities {
	return bot.Capabilities{
		Text:     true,
		Markdown: true,
		Image:    true,
		At:       true,
		Card:     true,
		File:     true,
		Reply:    true,
		Private:  true,
		Group:    true,
	}
}

// Start 启动回调监听并阻塞，直到 ctx 结束、Stop 被调用或服务异常退出。
//
// 启动前完成 net.Listen，因此 Start 返回前调用 Addr 即可拿到监听地址；
// 退出前会 Shutdown 服务器、停止投递 worker 并尽可能投递完队列中剩余事件。
func (a *Adapter) Start(ctx context.Context, sink bot.EventSink) error {
	if sink == nil {
		return errNilSink
	}
	if ctx == nil {
		ctx = context.Background()
	}

	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return errors.New("feishu: 适配器已启动")
	}
	ln, err := net.Listen("tcp", a.opts.ListenAddr)
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("feishu: 监听回调地址失败: %w", err)
	}
	workCtx, cancel := context.WithCancel(ctx)
	srv := &http.Server{
		Handler:           a.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	a.ln, a.addr, a.cancel, a.server, a.started = ln, ln.Addr().String(), cancel, srv, true
	a.mu.Unlock()

	a.wg.Add(eventWorkers)
	for range eventWorkers {
		go a.worker(workCtx, sink)
	}

	serveErr := make(chan error, 1)
	go func() {
		serr := srv.Serve(ln)
		if serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			serveErr <- serr
			return
		}
		serveErr <- nil
	}()

	a.log.Info("飞书适配器启动", "name", a.opts.Name, "addr", a.addr, "path", a.opts.Path)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serveErr:
		if runErr != nil {
			a.log.Error("飞书适配器服务异常退出", "name", a.opts.Name, "err", runErr)
		}
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelStop()
	if err := srv.Shutdown(stopCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("feishu: 关闭回调监听失败: %w", err)
	}

	a.mu.Lock()
	stop := a.cancel
	a.server, a.cancel, a.ln, a.addr, a.started = nil, nil, nil, "", false
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
	a.wg.Wait()
	a.log.Info("飞书适配器已停止", "name", a.opts.Name)
	return runErr
}

// Stop 停止回调监听并等待投递 worker 退出；可重复调用且幂等。
func (a *Adapter) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.RLock()
	srv, cancel := a.server, a.cancel
	a.mu.RUnlock()
	if srv == nil && cancel == nil {
		// 未启动或已停止：幂等返回。
		return nil
	}
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("feishu: 停止回调监听失败: %w", err)
		}
	}
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handler 构造回调路由。
func (a *Adapter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(a.opts.Path, a.handleEvent)
	return mux
}

// worker 从队列取事件投递给 sink；ctx 结束后先投递完剩余事件再退出。
func (a *Adapter) worker(ctx context.Context, sink bot.EventSink) {
	defer a.wg.Done()
	for {
		select {
		case <-ctx.Done():
			drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			for {
				select {
				case ev := <-a.queue:
					if err := sink.Emit(drainCtx, ev); err != nil {
						a.log.Warn("飞书事件投递失败", "event_id", ev.ID, "err", err)
					}
				default:
					cancel()
					return
				}
			}
		case ev := <-a.queue:
			if err := sink.Emit(ctx, ev); err != nil {
				a.log.Warn("飞书事件投递失败", "event_id", ev.ID, "err", err)
			}
		}
	}
}

// enqueue 非阻塞投递事件；队列满时丢弃并告警，绝不阻塞回调。
func (a *Adapter) enqueue(ev *bot.Event) {
	select {
	case a.queue <- ev:
	default:
		a.log.Warn("飞书事件队列已满，丢弃事件", "event_id", ev.ID, "type", ev.Type)
	}
}

// handleEvent 处理飞书事件回调：校验 → ACK → 异步投递。
func (a *Adapter) handleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.log.Warn("飞书回调方法不被支持", "method", r.Method, "remote", r.RemoteAddr)
		a.writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Code: 1, Msg: "only POST is supported"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		a.log.Warn("读取飞书回调 body 失败", "remote", r.RemoteAddr, "err", err)
		a.writeJSON(w, http.StatusBadRequest, errorResponse{Code: 1, Msg: "read body failed"})
		return
	}

	// EncryptKey 非空时必须校验签名。
	timestamp := r.Header.Get("X-Lark-Request-Timestamp")
	nonce := r.Header.Get("X-Lark-Request-Nonce")
	if !verifySignature(a.opts.EncryptKey, timestamp, nonce, r.Header.Get("X-Lark-Signature"), body) {
		a.log.Warn("飞书回调签名校验失败", "remote", r.RemoteAddr, "timestamp", timestamp, "nonce", nonce)
		a.writeJSON(w, http.StatusUnauthorized, errorResponse{Code: 1, Msg: "invalid signature"})
		return
	}

	env, err := a.decodeEnvelope(body)
	if err != nil {
		a.log.Warn("解析飞书回调 body 失败", "remote", r.RemoteAddr, "err", err)
		a.writeJSON(w, http.StatusBadRequest, errorResponse{Code: 1, Msg: "invalid body"})
		return
	}

	// url_verification：用 body 里的 token 校验后原样回写 challenge。
	if env.Challenge != "" {
		if !secureEqual(env.Token, a.opts.VerificationToken) {
			a.log.Warn("飞书 challenge token 校验失败", "remote", r.RemoteAddr)
			a.writeJSON(w, http.StatusUnauthorized, errorResponse{Code: 1, Msg: "invalid token"})
			return
		}
		a.writeJSON(w, http.StatusOK, challengeResponse{Challenge: env.Challenge})
		return
	}

	token := env.Header.Token
	if token == "" {
		token = env.Token
	}
	if !secureEqual(token, a.opts.VerificationToken) {
		a.log.Warn("飞书回调 token 校验失败", "remote", r.RemoteAddr, "event_type", env.Header.EventType)
		a.writeJSON(w, http.StatusUnauthorized, errorResponse{Code: 1, Msg: "invalid token"})
		return
	}

	ev := a.convert(env)
	// 快速 ACK：先回包，再异步投递，绝不阻塞飞书回调。
	a.writeJSON(w, http.StatusOK, ackResponse{Code: 0})
	a.enqueue(ev)
	a.log.Debug("飞书事件已接收", "event_id", ev.ID, "event_type", env.Header.EventType, "type", ev.Type)
}

// writeJSON 回写 JSON 响应。
func (a *Adapter) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.log.Warn("回写飞书回调响应失败", "err", err)
	}
}

// ackResponse 是事件回调的 ACK 响应。
type ackResponse struct {
	// Code 为 0 表示接收成功。
	Code int `json:"code"`
}

// challengeResponse 是 url_verification 的响应。
type challengeResponse struct {
	// Challenge 原样回写飞书下发的校验串。
	Challenge string `json:"challenge"`
}

// errorResponse 是失败响应。
type errorResponse struct {
	// Code 非 0 表示失败。
	Code int `json:"code"`
	// Msg 是失败原因描述。
	Msg string `json:"msg"`
}
