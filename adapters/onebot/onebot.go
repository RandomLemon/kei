// Package onebot 实现 OneBot v11 的 HTTP 双向适配器。
//
// 事件通过 OneBot 实现的 HTTP 上报（POST JSON）推送给本适配器，
// 发送消息则通过 OneBot 的 HTTP API（POST {"action","params","echo"}）调用。
// 不依赖任何第三方库，也不涉及 WebSocket 接入。
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

// OneBot v11 的发送动作名。
const (
	actionSendGroup   = "send_group_msg"
	actionSendPrivate = "send_private_msg"
)

const (
	// defaultPath 是事件上报的默认路径。
	defaultPath = "/onebot/event"
	// defaultTimeout 是默认 HTTP 客户端的超时时间。
	defaultTimeout = 10 * time.Second
	// shutdownTimeout 是 Start 退出时等待未完成请求的时限。
	shutdownTimeout = 5 * time.Second
	// maxBodyBytes 限制单个请求/响应体大小，避免异常实现打爆内存。
	maxBodyBytes = 1 << 20
	// errorBodyLimit 是错误信息中回显响应体的字节上限。
	errorBodyLimit = 256
)

// Options 是 OneBot HTTP 双向适配器的配置。
type Options struct {
	// Name 是 bot 名称，作为 bot.Event.BotID；必填。
	Name string
	// APIURL 是 OneBot HTTP API 根地址，如 http://127.0.0.1:3000；必填。
	APIURL string
	// ListenAddr 是事件上报的监听地址，如 127.0.0.1:8080；必填。
	// 传 ":0" 时由系统分配端口，可通过 Addr 查询实际地址。
	ListenAddr string
	// Path 是上报路径，默认 "/onebot/event"，必须以 "/" 开头。
	Path string
	// Secret 是上报校验令牌，可选；非空时要求
	// Authorization: Bearer <secret> 或 query 参数 access_token 与之匹配。
	Secret string
	// AccessToken 是调用 HTTP API 时附带的 Bearer 令牌，可选。
	AccessToken string
	// SelfID 是机器人自身 ID，可选；事件缺少 self_id 时用于生成事件 ID。
	SelfID string
	// HTTPClient 是调用 API 使用的客户端，为 nil 时使用 10s 超时的默认客户端。
	HTTPClient *http.Client
	// Logger 是日志器，为 nil 时使用 slog.Default()。
	Logger *slog.Logger
}

// Adapter 是 OneBot v11 的 HTTP 双向适配器。
//
// 一个实例对应 `Options.Name` 指定的一个 bot：事件从 ListenAddr 上的 HTTP
// 服务进入，发送请求发往 APIURL。Adapter 可安全地被多个 goroutine 并发调用。
type Adapter struct {
	name        string
	apiURL      string
	listenAddr  string
	path        string
	secret      string
	accessToken string
	selfID      string
	client      *http.Client
	log         *slog.Logger
	caps        bot.Capabilities

	mu     sync.Mutex
	srv    *http.Server
	addr   string
	closed bool
}

// 编译期断言：Adapter 必须实现 bot.Adapter。
var _ bot.Adapter = (*Adapter)(nil)

// New 创建 OneBot 适配器并校验必填项。
func New(opts Options) (*Adapter, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("onebot: Options.Name 不能为空")
	}
	if strings.TrimSpace(opts.APIURL) == "" {
		return nil, errors.New("onebot: Options.APIURL 不能为空")
	}
	if _, err := url.Parse(opts.APIURL); err != nil {
		return nil, fmt.Errorf("onebot: Options.APIURL 非法: %w", err)
	}
	if strings.TrimSpace(opts.ListenAddr) == "" {
		return nil, errors.New("onebot: Options.ListenAddr 不能为空")
	}

	path := opts.Path
	if path == "" {
		path = defaultPath
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("onebot: Options.Path 必须以 / 开头，得到 %q", path)
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Adapter{
		name:        opts.Name,
		apiURL:      opts.APIURL,
		listenAddr:  opts.ListenAddr,
		path:        path,
		secret:      opts.Secret,
		accessToken: opts.AccessToken,
		selfID:      opts.SelfID,
		client:      client,
		log:         logger,
		caps: bot.Capabilities{
			Text:    true,
			Image:   true,
			At:      true,
			File:    true,
			Reply:   true,
			Private: true,
			Group:   true,
		},
	}, nil
}

// Name 返回平台名，固定为 "onebot"。
func (a *Adapter) Name() string { return platformName }

// Capabilities 返回 OneBot 支持的消息能力。
//
// 不支持 Markdown 与卡片，因此这两类段会在发送前被 bot.Degrade 转成文本；
// OneBot 没有独立的表情段能力，表情随文本一起发送，故 Face 由 Text 支撑
// （见 bot.Capabilities.Supports）。
func (a *Adapter) Capabilities() bot.Capabilities { return a.caps }

// Addr 返回事件上报服务的实际监听地址。
//
// 只有 Start 完成监听后才有值；Options.ListenAddr 为 ":0" 时返回系统分配的真实地址。
func (a *Adapter) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addr
}

// Start 启动事件上报服务并阻塞，直到 ctx 结束。
//
// 返回前一定已完成监听，因此调用方在 Start 于后台运行时通过 Addr 轮询即可确定
// 服务就绪。ctx 结束时关闭 HTTP 服务，等待在途请求完成并返回 nil；
// 若 HTTP 服务非正常退出（例如监听被外部关闭）则返回该错误。
func (a *Adapter) Start(ctx context.Context, sink bot.EventSink) error {
	if sink == nil {
		return errors.New("onebot: EventSink 不能为空")
	}

	ln, err := net.Listen("tcp", a.listenAddr)
	if err != nil {
		return fmt.Errorf("onebot: 监听 %s 失败: %w", a.listenAddr, err)
	}

	server := &http.Server{
		Handler:           a.routes(sink),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = ln.Close()
		return errors.New("onebot: 适配器已停止，不能再次启动")
	}
	a.srv = server
	a.addr = ln.Addr().String()
	a.mu.Unlock()

	a.log.Info("onebot: 事件服务已启动", "name", a.name, "addr", a.addr, "path", a.path)

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

	a.log.Info("onebot: 事件服务已退出", "name", a.name, "addr", a.addr)
	if serveErr != nil {
		return fmt.Errorf("onebot: 事件服务退出: %w", serveErr)
	}
	return nil
}

// Stop 关闭事件上报服务；可重复调用，重复调用返回 nil。
func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	srv := a.srv
	a.mu.Unlock()

	if srv == nil {
		return nil
	}

	err := srv.Shutdown(ctx)
	if err != nil && errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("onebot: 关闭事件服务失败: %w", err)
	}
	return nil
}

// routes 构造事件上报的路由。
//
// 只注册 Options.Path 指定的路径，其它路径返回 404，避免把任意请求都当作事件处理。
// 路径模式匹配到子路径时（例如 /onebot/event/extra）回 404，而不是被宽松匹配吞掉。
func (a *Adapter) routes(sink bot.EventSink) http.Handler {
	mux := http.NewServeMux()
	handler := a.eventHandler(sink)
	mux.HandleFunc(a.path, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != a.path {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	})
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
// 平台能力降级消息段，再转换为 OneBot array 格式调用 HTTP API。
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

// apiResponse 是 OneBot HTTP API 的响应体。
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

	var out apiResponse
	if err := decodeJSON(respBody, &out); err != nil {
		return nil, fmt.Errorf("onebot: 解析 %s 响应失败: %w", action, err)
	}
	if out.Status != "ok" || out.RetCode != 0 {
		return nil, fmt.Errorf("onebot: %s 失败: status=%s retcode=%d wording=%s",
			action, out.Status, out.RetCode, firstNonEmpty(out.Wording, out.Msg))
	}

	a.log.Debug("onebot: 发送成功", "name", a.name, "action", action, "message_id", out.Data.MessageID.String())
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
