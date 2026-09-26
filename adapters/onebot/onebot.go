// Package onebot 实现 OneBot v11 适配器，覆盖规范里的四种接入方式，由
// Options.Mode 选择（一次只启用一种，缺省 reverse_ws）：
//
//   - forward_http / reverse_http（同义，HTTP 双向）：事件通过 OneBot 实现的 HTTP
//     上报（POST JSON）进入本适配器，发送通过 OneBot 的 HTTP API 调用。
//   - reverse_ws（缺省）：OneBot 实现主动连接本适配器的 WSPath，事件经连接上行，
//     发送优先经同一连接下发（按 echo 匹配响应）。
//   - forward_ws：本适配器主动连接 OneBot 实现的 WSURL，事件与发送同样共用这条
//     连接，断线后自动重连，不监听任何本地端口。
//
// 与所选 mode 无关的配置键不报错，而是忽略并记一条 warning，因此同一份配置可以
// 保留全部方式的键、只切换 Mode。发送回落规则与 mode 无关：只要配置了非空的
// APIURL，它就是一条可用的出站 HTTP API 通道，优先级固定为
// 「可用 WebSocket 连接 → APIURL → 报错」。不依赖任何第三方库：正向与反向
// WebSocket 共用的 RFC 6455 实现（含服务端与客户端两个方向）见 ws.go，
// 连接管理与反向入口见 reverse.go，正向连接与重连见 forward.go。
package onebot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// platformName 是 OneBot 平台在统一事件中的平台名，也是 Adapter.Name() 的返回值。
const platformName = "onebot"

// Options.Mode 的取值；forward_http 与 reverse_http 同义（HTTP 双向）。
const (
	modeForwardHTTP = "forward_http"
	modeReverseHTTP = "reverse_http"
	modeForwardWS   = "forward_ws"
	modeReverseWS   = "reverse_ws"
	// defaultMode 是 Options.Mode 为空时的接入方式。
	defaultMode = modeReverseWS
)

// topology 是接入方式归一后的拓扑。
type topology uint8

const (
	topoHTTP topology = iota
	topoForwardWS
	topoReverseWS
)

// parseMode 归一 Options.Mode：空值取 defaultMode；大小写与首尾空白不敏感；
// 无法识别时返回错误并列出全部合法取值。
func parseMode(raw string) (topology, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", modeReverseWS:
		return topoReverseWS, nil
	case modeForwardHTTP, modeReverseHTTP:
		return topoHTTP, nil
	case modeForwardWS:
		return topoForwardWS, nil
	default:
		return 0, fmt.Errorf("onebot: Options.Mode 非法: %q（可选 %s / %s / %s / %s）",
			raw, modeForwardHTTP, modeReverseHTTP, modeForwardWS, modeReverseWS)
	}
}

// modeName 返回归一后的 mode 字面值（仅用于日志），空值取 defaultMode。
func modeName(raw string) string {
	if s := strings.ToLower(strings.TrimSpace(raw)); s != "" {
		return s
	}
	return defaultMode
}

// validateWSURL 校验正向 WebSocket 地址：必须是 ws:// 或 wss:// 且带主机名。
func validateWSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("onebot: Options.WSURL 非法: %w", err)
	}
	switch u.Scheme {
	case "ws", "wss":
	default:
		return fmt.Errorf("onebot: Options.WSURL 只支持 ws:// 与 wss://，得到 %q", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("onebot: Options.WSURL 缺少主机名: %q", raw)
	}
	return nil
}

// OneBot v11 的发送动作名。
const (
	actionSendGroup   = "send_group_msg"
	actionSendPrivate = "send_private_msg"
)

const (
	// defaultPath 是事件上报的默认路径。
	defaultPath = "/onebot/event"
	// defaultWSPath 是反向 WebSocket 的默认接入路径。
	defaultWSPath = "/onebot/ws"
	// defaultTimeout 是默认 HTTP 客户端的超时时间。
	defaultTimeout = 10 * time.Second
	// shutdownTimeout 是 Start 退出时等待在途请求与反向 WebSocket 读循环退出的时限。
	shutdownTimeout = 5 * time.Second
	// maxBodyBytes 限制单个请求/响应体大小，避免异常实现打爆内存。
	maxBodyBytes = 1 << 20
	// errorBodyLimit 是错误信息中回显响应体的字节上限。
	errorBodyLimit = 256
)

// Options 是 OneBot 适配器的配置。
type Options struct {
	// Name 是 bot 名称，作为 bot.Event.BotID；必填。
	Name string
	// Mode 是接入方式，取值 forward_http、reverse_http、forward_ws、reverse_ws；
	// 空值（未配置）为 reverse_ws。
	//
	//   - forward_http / reverse_http（同义）：HTTP 双向。本进程在 ListenAddr+Path
	//     接收 OneBot 的 HTTP 上报，并调用 APIURL 的 HTTP API 发送；两者都必填。
	//   - reverse_ws：OneBot 实现主动连入 ListenAddr 上的 WSPath，事件与发送共用
	//     这条连接；ListenAddr 必填。
	//   - forward_ws：本适配器主动连接 WSURL（ws:// 或 wss://），事件与发送共用这条
	//     连接，不监听任何端口；WSURL 必填，断线后按 1s→30s 指数退避自动重连。
	//
	// 与所选方式无关的键会被忽略并记 warning，因此同一份配置可以保留全部方式的键、
	// 只切换 Mode。
	Mode string
	// WSURL 是 OneBot 实现正向 WebSocket 的地址，如 ws://127.0.0.1:6700；
	// 仅在 Mode 为 forward_ws 时使用，必填。指向 OneBot 的 `/` 端点（或反向
	// WebSocket 等价端点）才能同时收发；若指向 `/event` 这类只推事件的端点，
	// 发送会等待响应直到 context 超时。
	WSURL string
	// APIURL 是 OneBot HTTP API 根地址，如 http://127.0.0.1:3000。
	// 在 forward_http / reverse_http 下必填；其它 mode 下可选，非空时作为发送的
	// HTTP API 回落通道（优先级：可用 WebSocket 连接 → APIURL → 报错）。
	APIURL string
	// ListenAddr 是事件入站的监听地址，如 127.0.0.1:8080。
	// 在 forward_http / reverse_http 与 reverse_ws 下必填（两种入口各自使用它），
	// 在 forward_ws 下不使用。传 ":0" 时由系统分配端口，可通过 Addr 查询实际地址。
	ListenAddr string
	// Path 是 HTTP 上报路径，默认 "/onebot/event"，必须以 "/" 开头；
	// 仅在 Mode 为 forward_http / reverse_http 时生效。
	Path string
	// WSPath 是反向 WebSocket 的接入路径，默认 "/onebot/ws"，必须以 "/" 开头；
	// 仅在 Mode 为 reverse_ws 时生效。OneBot 实现把反向 WebSocket 地址配置为
	// ws://<ListenAddr><WSPath>，可用 ?access_token= 或以 Authorization 头鉴权。
	WSPath string
	// PingInterval 是 WebSocket 的心跳间隔：为 0 时使用默认值 30s，
	// 负值表示不发送心跳（此时无法发现半开连接）。正向与反向 WebSocket 都适用。
	PingInterval time.Duration
	// Secret 是入站校验令牌，可选；非空时要求 HTTP 上报与反向 WebSocket 的
	// Authorization: Bearer <secret> 或 query 参数 access_token 与之匹配。
	// 正向 WebSocket 不涉及入站校验，该键在 forward_ws 下被忽略。
	Secret string
	// AccessToken 是调用 HTTP API 与正向 WebSocket 握手时附带的 Bearer 令牌，可选。
	AccessToken string
	// SelfID 是机器人自身 ID，可选；事件缺少 self_id 时用于生成事件 ID，
	// 非空时还要求反向 WebSocket 的 X-Self-ID 与之一致：不一致的连接直接拒绝，
	// 以免别的账号的事件进入本 bot。
	SelfID string
	// HTTPClient 是调用 API 使用的客户端，为 nil 时使用 10s 超时的默认客户端。
	HTTPClient *http.Client
	// Logger 是日志器，为 nil 时使用 slog.Default()。
	Logger *slog.Logger
}

// Adapter 是 OneBot v11 适配器，接入方式由 Options.Mode 决定。
//
// 一个实例对应 `Options.Name` 指定的一个 bot：事件从 ListenAddr 上的服务进入
// （HTTP 上报或反向 WebSocket）或从主动连接的 WSURL 进入，发送优先走可用
// WebSocket 连接，否则回落 APIURL。Adapter 可安全地被多个 goroutine 并发调用。
type Adapter struct {
	name        string
	mode        string
	topo        topology
	apiURL      string
	wsURL       string
	listenAddr  string
	path        string
	wsPath      string
	secret      string
	accessToken string
	selfID      string
	client      *http.Client
	log         *slog.Logger
	caps        bot.Capabilities
	hub         *wsHub

	mu        sync.Mutex
	srv       *http.Server
	addr      string
	closed    bool
	fwdCancel context.CancelFunc
}

// 编译期断言：Adapter 必须实现 bot.Adapter。
var _ bot.Adapter = (*Adapter)(nil)

// listensHTTP 报告本进程是否提供 HTTP 上报入口。
func (a *Adapter) listensHTTP() bool { return a.topo == topoHTTP }

// listensWS 报告本进程是否提供反向 WebSocket 入口。
func (a *Adapter) listensWS() bool { return a.topo == topoReverseWS }

// listens 报告是否需要监听 ListenAddr。
func (a *Adapter) listens() bool { return a.listensHTTP() || a.listensWS() }

// dialsWS 报告是否需要主动连接 WSURL。
func (a *Adapter) dialsWS() bool { return a.topo == topoForwardWS }

// New 创建 OneBot 适配器并校验必填项。
func New(opts Options) (*Adapter, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("onebot: Options.Name 不能为空")
	}

	topo, err := parseMode(opts.Mode)
	if err != nil {
		return nil, err
	}

	apiURL := strings.TrimSpace(opts.APIURL)
	if apiURL != "" {
		if _, err := url.Parse(apiURL); err != nil {
			return nil, fmt.Errorf("onebot: Options.APIURL 非法: %w", err)
		}
	}
	wsURL := strings.TrimSpace(opts.WSURL)
	if wsURL != "" {
		if err := validateWSURL(wsURL); err != nil {
			return nil, err
		}
	}
	listenAddr := strings.TrimSpace(opts.ListenAddr)

	switch topo {
	case topoHTTP:
		if apiURL == "" {
			return nil, errors.New("onebot: mode forward_http/reverse_http 需要配置 api_url")
		}
		if listenAddr == "" {
			return nil, errors.New("onebot: mode forward_http/reverse_http 需要配置 listen_addr")
		}
	case topoReverseWS:
		if listenAddr == "" {
			return nil, errors.New("onebot: mode reverse_ws 需要配置 listen_addr")
		}
	case topoForwardWS:
		if wsURL == "" {
			return nil, errors.New("onebot: mode forward_ws 需要配置 ws_url")
		}
	}

	path := opts.Path
	if path == "" {
		path = defaultPath
	}
	if topo == topoHTTP && !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("onebot: Options.Path 必须以 / 开头，得到 %q", path)
	}

	wsPath := opts.WSPath
	if wsPath == "" {
		wsPath = defaultWSPath
	}
	if topo == topoReverseWS && !strings.HasPrefix(wsPath, "/") {
		return nil, fmt.Errorf("onebot: Options.WSPath 必须以 / 开头，得到 %q", wsPath)
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	a := &Adapter{
		name:        opts.Name,
		mode:        modeName(opts.Mode),
		topo:        topo,
		apiURL:      apiURL,
		wsURL:       wsURL,
		listenAddr:  listenAddr,
		path:        path,
		wsPath:      wsPath,
		secret:      opts.Secret,
		accessToken: opts.AccessToken,
		selfID:      opts.SelfID,
		client:      client,
		log:         logger,
		hub:         newWSHub(opts.Name, opts.SelfID, opts.PingInterval, logger),
		caps: bot.Capabilities{
			Text:    true,
			Image:   true,
			At:      true,
			File:    true,
			Reply:   true,
			Private: true,
			Group:   true,
		},
	}

	for _, key := range a.ignoredKeys(opts) {
		a.log.Warn("onebot: 配置键在当前 mode 下不生效，已忽略",
			"name", a.name, "mode", a.mode, "key", key)
	}
	if strings.TrimSpace(opts.Mode) == "" {
		a.log.Info("onebot: 未配置 mode，使用默认 reverse_ws", "name", a.name)
	}

	return a, nil
}

// ignoredKeys 返回当前 mode 下不生效、但在配置里显式写了的键。
//
// 忽略而非报错，是为了让同一份配置文件保留全部接入方式的键、只切换 mode。
func (a *Adapter) ignoredKeys(opts Options) []string {
	var keys []string
	add := func(key string, present bool) {
		if present {
			keys = append(keys, key)
		}
	}
	switch a.topo {
	case topoHTTP:
		add("ws_url", strings.TrimSpace(opts.WSURL) != "")
		add("ws_path", strings.TrimSpace(opts.WSPath) != "")
		add("ping_interval", opts.PingInterval != 0)
	case topoReverseWS:
		add("path", strings.TrimSpace(opts.Path) != "")
		add("ws_url", strings.TrimSpace(opts.WSURL) != "")
	case topoForwardWS:
		add("listen_addr", strings.TrimSpace(opts.ListenAddr) != "")
		add("path", strings.TrimSpace(opts.Path) != "")
		add("ws_path", strings.TrimSpace(opts.WSPath) != "")
		add("secret", strings.TrimSpace(opts.Secret) != "")
	}
	return keys
}

// WSPath 返回反向 WebSocket 的实际接入路径；仅在 Mode 为 reverse_ws 时生效。
func (a *Adapter) WSPath() string { return a.wsPath }

// Name 返回平台名，固定为 "onebot"。
func (a *Adapter) Name() string { return platformName }

// Capabilities 返回 OneBot 支持的消息能力。
//
// 不支持 Markdown 与卡片，因此这两类段会在发送前被 bot.Degrade 转成文本；
// OneBot 没有独立的表情段能力，表情随文本一起发送，故 Face 由 Text 支撑
// （见 bot.Capabilities.Supports）。
func (a *Adapter) Capabilities() bot.Capabilities { return a.caps }

// Addr 返回事件服务的实际监听地址。
//
// 只有 Start 完成监听后才有值；Options.ListenAddr 为 ":0" 时返回系统分配的真实地址。
// Mode 为 forward_ws 时不监听任何端口，恒返回空字符串。
func (a *Adapter) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addr
}

// Start 启动事件入口并阻塞，直到 ctx 结束。
//
// 需要监听时（forward_http/reverse_http/reverse_ws）返回前一定已完成监听，因此
// 调用方在 Start 于后台运行时通过 Addr 轮询即可确定服务就绪。forward_ws 不监听
// 任何端口，只在后台维护到 WSURL 的连接。ctx 结束时关闭服务与连接并返回 nil；
// 若服务非正常退出（例如监听被外部关闭）则返回该错误。
func (a *Adapter) Start(ctx context.Context, sink bot.EventSink) error {
	if sink == nil {
		return errors.New("onebot: EventSink 不能为空")
	}

	// 正向连接循环用独立的 ctx：退出时必须先取消它再等 hub 收尾，否则重连前的
	// 退避睡眠会拖住 shutdown。
	fwdCtx, fwdCancel := context.WithCancel(ctx)
	defer func() {
		fwdCancel()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		// WebSocket 是 hijacked 连接（反向）或主动拨号（正向）：http.Server.Shutdown
		// 既不关闭也不等待它们，必须在这里显式收尾，否则连接与读循环会泄漏。
		if herr := a.hub.shutdown(shutdownCtx); herr != nil {
			a.log.Warn("onebot: WebSocket 收尾未完成", "name", a.name, "err", herr)
		}
	}()

	var (
		ln     net.Listener
		server *http.Server
	)
	if a.listens() {
		var err error
		ln, err = net.Listen("tcp", a.listenAddr)
		if err != nil {
			return fmt.Errorf("onebot: 监听 %s 失败: %w", a.listenAddr, err)
		}
		server = &http.Server{
			Handler:           a.routes(sink),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
		}
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		if ln != nil {
			_ = ln.Close()
		}
		return errors.New("onebot: 适配器已停止，不能再次启动")
	}
	a.srv = server // 无监听时为 nil，Stop 已按 nil 处理
	a.fwdCancel = fwdCancel
	if ln != nil {
		a.addr = ln.Addr().String()
	}
	a.mu.Unlock()
	a.hub.setSink(sink)

	if a.dialsWS() {
		a.hub.startForward(fwdCtx, forwardConfig{url: a.wsURL, accessToken: a.accessToken})
	}

	if server == nil {
		// 只有正向 WebSocket：不监听任何端口，但仍需阻塞到 ctx 结束。
		a.log.Info("onebot: 未监听本地端口，仅正向 WebSocket",
			"name", a.name, "mode", a.mode, "url", a.wsURL)
		<-ctx.Done()
		a.log.Info("onebot: 事件服务已退出", "name", a.name, "mode", a.mode)
		return nil
	}

	attrs := []any{"name", a.name, "mode", a.mode, "addr", a.addr}
	if a.listensHTTP() {
		attrs = append(attrs, "path", a.path)
	}
	if a.listensWS() {
		attrs = append(attrs, "ws_path", a.wsPath)
	}
	a.log.Info("onebot: 事件服务已启动", attrs...)

	errCh := make(chan error, 1)
	go func() {
		if serr := server.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
			return
		}
		errCh <- nil
	}()

	var serveErr error
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		serveErr = server.Shutdown(shutdownCtx)
		cancel()
		if serveErr != nil && errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		<-errCh
	case serveErr = <-errCh:
	}

	a.log.Info("onebot: 事件服务已退出", "name", a.name, "mode", a.mode, "addr", a.addr)
	if serveErr != nil {
		return fmt.Errorf("onebot: 事件服务退出: %w", serveErr)
	}
	return nil
}

// Stop 关闭事件服务与全部 WebSocket 连接；可重复调用，重复调用返回 nil。
func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	srv := a.srv
	cancel := a.fwdCancel
	a.mu.Unlock()

	// 先停正向连接循环，避免它在下一次重连前重新登记连接。
	if cancel != nil {
		cancel()
	}

	// WebSocket 连接不受 http.Server 管理，必须单独关闭并等待读循环退出。
	wsErr := a.hub.shutdown(ctx)

	if srv == nil {
		return wsErr
	}

	err := srv.Shutdown(ctx)
	if err != nil && errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("onebot: 关闭事件服务失败: %w", err)
	}
	return wsErr
}

// routes 构造事件入口的路由。
//
// 按拓扑只注册实际启用的入口，其它路径返回 404，避免把任意请求都当作事件处理。
// 路径模式匹配到子路径时（例如 /onebot/event/extra）回 404，而不是被宽松匹配吞掉。
func (a *Adapter) routes(sink bot.EventSink) http.Handler {
	mux := http.NewServeMux()
	exact := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != pattern {
				http.NotFound(w, r)
				return
			}
			h(w, r)
		})
	}
	if a.listensHTTP() {
		exact(a.path, a.eventHandler(sink))
	}
	if a.listensWS() {
		exact(a.wsPath, a.wsHandler())
	}
	return mux
}

// eventHandler 返回事件上报的 HTTP 处理函数。
//
// 处理流程：校验方法与鉴权 → 读取并解析 JSON → 立即回 204 → 再投递事件。
// 先响应后投递保证平台回调快速 ACK；Emit 在同 goroutine 内调用，因为
// bot.EventSink 约定实现方必须快速返回。
func (a *Adapter) eventHandler(sink bot.EventSink) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "only POST is supported", http.StatusMethodNotAllowed)
			return
		}
		if !a.authorized(r) {
			a.log.Warn("onebot: 上报校验失败", "remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			a.log.Warn("onebot: 读取上报请求体失败", "remote", r.RemoteAddr, "err", err)
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}

		ev, err := convertEvent(body, a.name, a.selfID, a.log)
		if err != nil {
			a.log.Warn("onebot: 解析上报事件失败", "remote", r.RemoteAddr, "err", err)
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusNoContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		if err := sink.Emit(r.Context(), ev); err != nil {
			a.log.Warn("onebot: 投递事件失败", "id", ev.ID, "err", err)
		}
	}
}

// authorized 校验上报请求的令牌。
//
// Secret 为空时不做校验；否则 Authorization: Bearer <secret> 与 query 参数
// access_token=<secret> 任一匹配即通过，比较使用常数时间算法。
func (a *Adapter) authorized(r *http.Request) bool {
	if a.secret == "" {
		return true
	}
	if token := bearerToken(r.Header.Get("Authorization")); token != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(a.secret)) == 1 {
		return true
	}
	if token := r.URL.Query().Get("access_token"); token != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(a.secret)) == 1 {
		return true
	}
	return false
}

// bearerToken 从 Authorization 头中提取 Bearer 令牌。
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// Send 把统一消息发送到目标会话。
//
// 目标类型为空时按 ChannelID/UserID 推断群聊或私聊；发送前用 bot.Degrade 按
// 平台能力降级消息段。存在可承载 API 的 WebSocket 连接（正向或反向）时优先经
// 连接下发（按 echo 匹配响应），否则回落到 HTTP API。
func (a *Adapter) Send(ctx context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	if req == nil {
		return nil, errors.New("onebot: 发送请求不能为空")
	}
	if req.Message == nil {
		return nil, errors.New("onebot: SendRequest.Message 不能为空")
	}

	action, targetID, err := a.resolveTarget(req.Target)
	if err != nil {
		return nil, err
	}

	params := map[string]any{"auto_escape": false}
	if action == actionSendGroup {
		params["group_id"] = targetID
	} else {
		params["user_id"] = targetID
	}
	params["message"] = buildArray(bot.Degrade(req.Message, a.caps), a.log)

	if session := a.hub.pick(); session != nil {
		return a.hub.call(ctx, session, action, params)
	}
	if a.apiURL == "" {
		return nil, errors.New("onebot: 没有可用的反向 WebSocket 连接，且未配置 api_url，无法发送")
	}
	return a.call(ctx, action, params)
}

// resolveTarget 根据目标推断发送动作（群聊/私聊）与目标 ID。
func (a *Adapter) resolveTarget(t bot.Target) (action, id string, err error) {
	kind := t.Kind
	if kind == "" {
		switch {
		case t.ChannelID != "":
			kind = bot.MessageGroup
		case t.UserID != "":
			kind = bot.MessagePrivate
		}
	}

	switch kind {
	case bot.MessageGroup, bot.MessageChannel:
		if t.ChannelID == "" {
			return "", "", errors.New("onebot: 群消息缺少 Target.ChannelID")
		}
		return actionSendGroup, t.ChannelID, nil
	case bot.MessagePrivate:
		if t.UserID == "" {
			return "", "", errors.New("onebot: 私聊消息缺少 Target.UserID")
		}
		return actionSendPrivate, t.UserID, nil
	default:
		return "", "", errors.New("onebot: 无法确定发送目标，请设置 Target.Kind 或 ChannelID/UserID")
	}
}

// apiRequest 是 OneBot HTTP API 的请求体。
type apiRequest struct {
	// Action 是动作名，例如 send_group_msg。
	Action string `json:"action"`
	// Params 是动作参数。
	Params map[string]any `json:"params"`
	// Echo 是调用方生成的唯一标识，便于把响应与请求对应起来。
	Echo string `json:"echo"`
}

// apiResponse 是 OneBot API 的响应体（HTTP 与反向 WebSocket 共用）。
type apiResponse struct {
	// Status 是执行状态，"ok" 表示成功。
	Status string `json:"status"`
	// RetCode 是返回码，0 表示成功。
	RetCode int `json:"retcode"`
	// Wording 是错误信息，部分实现返回 msg。
	Wording string `json:"wording"`
	// Msg 是错误信息的兼容字段。
	Msg string `json:"msg"`
	// Data 是动作返回数据。
	Data struct {
		// MessageID 是新消息的 ID，可能是数字或字符串。
		MessageID flexID `json:"message_id"`
	} `json:"data"`
}

// call 调用一次 OneBot HTTP API 并解析响应。
func (a *Adapter) call(ctx context.Context, action string, params map[string]any) (*bot.SendResult, error) {
	echo, err := newEcho()
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(apiRequest{Action: action, Params: params, Echo: echo})
	if err != nil {
		return nil, fmt.Errorf("onebot: 编码 %s 请求失败: %w", action, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("onebot: 构造 %s 请求失败: %w", action, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if a.accessToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.accessToken)
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("onebot: 调用 %s 失败: %w", action, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("onebot: 读取 %s 响应失败: %w", action, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("onebot: %s 返回 HTTP %d: %s", action, resp.StatusCode, truncate(string(respBody), errorBodyLimit))
	}

	res, err := decodeAPIResponse(action, respBody)
	if err != nil {
		return nil, err
	}
	a.log.Debug("onebot: 发送成功", "name", a.name, "action", action,
		"transport", "http", "message_id", res.MessageID)
	return res, nil
}

// decodeAPIResponse 解析一次 OneBot API 调用的响应体。
//
// HTTP 通道与反向 WebSocket 通道共用同一套成功判定：status 必须为 ok 且
// retcode 必须为 0，否则把 wording/msg 作为错误信息返回。respBody 归返回值所有。
func decodeAPIResponse(action string, respBody []byte) (*bot.SendResult, error) {
	var out apiResponse
	if err := decodeJSON(respBody, &out); err != nil {
		return nil, fmt.Errorf("onebot: 解析 %s 响应失败: %w", action, err)
	}
	if out.Status != "ok" || out.RetCode != 0 {
		return nil, fmt.Errorf("onebot: %s 失败: status=%s retcode=%d wording=%s",
			action, out.Status, out.RetCode, firstNonEmpty(out.Wording, out.Msg))
	}
	return &bot.SendResult{MessageID: out.Data.MessageID.String(), Raw: json.RawMessage(respBody)}, nil
}

// newEcho 生成一个符合 UUID v4 格式的唯一标识，作为请求的 echo 字段。
func newEcho() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("onebot: 生成 echo 失败: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	var buf [36]byte
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf[:]), nil
}

// truncate 截断过长的响应体用于错误信息，避免日志被单条响应撑爆。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
