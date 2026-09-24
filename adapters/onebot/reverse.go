package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// defaultPingInterval 是反向 WebSocket 的默认心跳间隔。
const defaultPingInterval = 30 * time.Second

// OneBot 反向 WebSocket 客户端在 X-Client-Role 中声明的角色。
const (
	// wsRoleUniversal 表示连接既接收事件也承载 API 调用。
	wsRoleUniversal = "universal"
	// wsRoleEvent 表示连接只接收事件，不能承载 API 调用。
	wsRoleEvent = "event"
	// wsRoleAPI 表示连接只承载 API 调用。
	wsRoleAPI = "api"
)

// wsHub 管理一个适配器实例上的全部反向 WebSocket 连接。
//
// OneBot 反向 WebSocket 由平台侧（OneBot 实现）主动连到本适配器：事件经连接
// 上行，API 调用经同一条连接下行。上行不需要 ACK，收到即可投递；下行靠 echo
// 字段把响应与请求对应起来，因此同一条连接上可以并行多次调用。
type wsHub struct {
	botID        string
	selfID       string
	pingInterval time.Duration
	log          *slog.Logger

	mu       sync.Mutex
	sink     bot.EventSink
	sessions []*wsSession // 按建立顺序排列，发送选路因此是确定的
	closed   bool
	wg       sync.WaitGroup
}

// newWSHub 创建一个连接管理器。pingInterval 为 0 时使用默认心跳间隔，
// 负值表示不发送心跳。
func newWSHub(botID, selfID string, pingInterval time.Duration, log *slog.Logger) *wsHub {
	if pingInterval == 0 {
		pingInterval = defaultPingInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &wsHub{botID: botID, selfID: selfID, pingInterval: pingInterval, log: log}
}

// setSink 注入事件出口。适配器 Start 时调用，之后不再变化。
func (h *wsHub) setSink(sink bot.EventSink) {
	h.mu.Lock()
	h.sink = sink
	h.mu.Unlock()
}

// currentSink 读取事件出口，未注入时返回 nil。
func (h *wsHub) currentSink() bot.EventSink {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sink
}

// register 登记一条新连接；适配器正在关闭时拒绝。
func (h *wsHub) register(conn *wsConn, selfID, role string) (*wsSession, error) {
	s := &wsSession{
		hub:     h,
		conn:    conn,
		selfID:  selfID,
		role:    role,
		pending: make(map[string]chan wsReply),
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, errors.New("onebot: 适配器正在关闭，拒绝新的反向 WebSocket 连接")
	}
	h.sessions = append(h.sessions, s)
	h.wg.Add(1)
	h.mu.Unlock()
	return s, nil
}

// unregister 摘除连接并释放等待中的调用。
func (h *wsHub) unregister(s *wsSession) {
	h.mu.Lock()
	for i, cur := range h.sessions {
		if cur == s {
			h.sessions = append(h.sessions[:i], h.sessions[i+1:]...)
			break
		}
	}
	h.mu.Unlock()

	s.fail(errWSClosed)
	h.wg.Done()
}

// pick 选出一条可承载 API 调用的连接，即最早建立的那条。
//
// 同一 bot 实例只会接受与 self_id 一致的连接（见 wsHandler），因此这里不需要
// 再按 self_id 选路；固定选最早建立的连接可让多连接时的行为可预测。
// 没有任何可承载 API 的连接时返回 nil，调用方回落到 HTTP API。
func (h *wsHub) pick() *wsSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.apiCapable() {
			return s
		}
	}
	return nil
}

// call 经一条反向 WebSocket 连接调用 OneBot API 并等待同 echo 的响应。
func (h *wsHub) call(ctx context.Context, s *wsSession, action string, params map[string]any) (*bot.SendResult, error) {
	echo, err := newEcho()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(apiRequest{Action: action, Params: params, Echo: echo})
	if err != nil {
		return nil, fmt.Errorf("onebot: 编码 %s 请求失败: %w", action, err)
	}

	ch, err := s.addPending(echo)
	if err != nil {
		return nil, err
	}
	defer s.dropPending(echo)

	if err := s.conn.writeMessage(ctx, wsOpText, body); err != nil {
		s.fail(fmt.Errorf("写 %s 请求失败: %w", action, err))
		return nil, fmt.Errorf("onebot: 反向 WebSocket 调用 %s 失败: %w", action, err)
	}

	select {
	case reply := <-ch:
		if reply.err != nil {
			return nil, fmt.Errorf("onebot: 反向 WebSocket 的 %s 调用失败: %w", action, reply.err)
		}
		res, err := decodeAPIResponse(action, reply.body)
		if err != nil {
			return nil, err
		}
		h.log.Debug("onebot: 发送成功", "name", h.botID, "action", action,
			"transport", "ws", "message_id", res.MessageID)
		return res, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("onebot: 等待反向 WebSocket 的 %s 响应失败: %w", action, ctx.Err())
	}
}

// handleMessage 处理连接上收到的一条消息。
//
// 带 post_type 的按事件投递（反向 WebSocket 不需要 ACK），带 echo 的按 API 响应
// 派发；两者都不是时只记日志，不影响连接。
func (h *wsHub) handleMessage(ctx context.Context, s *wsSession, raw []byte) {
	var probe struct {
		PostType string `json:"post_type"`
		Echo     string `json:"echo"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		h.log.Warn("onebot: 反向 WebSocket 消息不是合法 JSON", "name", h.botID, "err", err)
		return
	}

	if probe.PostType == "" {
		if probe.Echo != "" && s.deliver(probe.Echo, raw) {
			return
		}
		h.log.Warn("onebot: 反向 WebSocket 收到无法处理的消息",
			"name", h.botID, "echo", probe.Echo)
		return
	}

	ev, err := convertEvent(raw, h.botID, firstNonEmpty(s.selfID, h.selfID), h.log)
	if err != nil {
		h.log.Warn("onebot: 解析反向 WebSocket 事件失败", "name", h.botID, "err", err)
		return
	}
	sink := h.currentSink()
	if sink == nil {
		h.log.Warn("onebot: 适配器尚未启动，丢弃反向 WebSocket 事件", "name", h.botID, "id", ev.ID)
		return
	}
	if err := sink.Emit(ctx, ev); err != nil {
		h.log.Warn("onebot: 投递反向 WebSocket 事件失败", "name", h.botID, "id", ev.ID, "err", err)
	}
}

// shutdown 关闭全部连接并等待读循环退出。
//
// 反向 WebSocket 是 hijacked 连接，http.Server.Shutdown 既不关闭也不等待它们，
// 必须在这里显式收尾。重复调用安全。
func (h *wsHub) shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	sessions := append([]*wsSession(nil), h.sessions...)
	h.mu.Unlock()

	for _, s := range sessions {
		_ = s.conn.closeWith(wsCloseGoingAway, nil)
	}

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// 硬关底层连接后读循环必定退出，再给一小段时间收尾。
		for _, s := range sessions {
			_ = s.conn.close()
		}
		select {
		case <-done:
			return nil
		case <-time.After(time.Second):
			return fmt.Errorf("onebot: 等待 %d 条反向 WebSocket 连接退出超时", len(sessions))
		}
	}
}

// wsReply 是一次 API 调用的结果：要么是对端回传的响应体，要么是连接失败原因。
type wsReply struct {
	body []byte
	err  error
}

// wsSession 是一条反向 WebSocket 连接。
type wsSession struct {
	hub    *wsHub
	conn   *wsConn
	selfID string
	role   string

	mu      sync.Mutex
	pending map[string]chan wsReply
	done    bool
}

// apiCapable 表示该连接能否承载 API 调用；role=event 的连接只接收事件。
func (s *wsSession) apiCapable() bool { return s.role != wsRoleEvent }

// serve 读循环：投递事件、派发 API 响应，直到连接结束。
//
// 心跳间隔为正时启用读超时：连续两个心跳周期没有收到任何帧即判定连接失效，
// 避免半开连接长期占住发送选路。
func (s *wsSession) serve(ctx context.Context) error {
	readTimeout := time.Duration(0)
	if interval := s.hub.pingInterval; interval > 0 {
		readTimeout = wsReadTimeout(interval)

		stop := make(chan struct{})
		defer close(stop)
		go s.keepAlive(interval, stop)
	}

	for {
		_, payload, err := s.conn.readMessage(readTimeout)
		if err != nil {
			return err
		}
		s.hub.handleMessage(ctx, s, payload)
	}
}

// keepAlive 定期发送 ping 帧；对端必须回 pong，读超时据此生效。
func (s *wsSession) keepAlive(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := s.conn.writeFrame(wsOpPing, nil, time.Now().Add(wsWriteTimeout)); err != nil {
				_ = s.conn.close()
				return
			}
		}
	}
}

// wsReadTimeout 根据心跳间隔推出读超时。
func wsReadTimeout(interval time.Duration) time.Duration { return 2 * interval }

// addPending 为一次 API 调用登记响应等待通道。
func (s *wsSession) addPending(echo string) (chan wsReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil, errors.New("onebot: 反向 WebSocket 连接已断开")
	}
	ch := make(chan wsReply, 1)
	s.pending[echo] = ch
	return ch, nil
}

// dropPending 移除响应等待通道；已经被派发过或连接已失效时是空操作。
func (s *wsSession) dropPending(echo string) {
	s.mu.Lock()
	delete(s.pending, echo)
	s.mu.Unlock()
}

// deliver 把响应交给对应 echo 的等待方，没有等待方时返回 false。
func (s *wsSession) deliver(echo string, body []byte) bool {
	s.mu.Lock()
	ch, ok := s.pending[echo]
	delete(s.pending, echo)
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- wsReply{body: body}:
	default:
	}
	return true
}

// fail 标记连接失效：关闭底层连接并唤醒所有等待中的调用。
func (s *wsSession) fail(err error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	pending := s.pending
	s.pending = make(map[string]chan wsReply)
	s.mu.Unlock()

	_ = s.conn.close()
	for _, ch := range pending {
		select {
		case ch <- wsReply{err: err}:
		default:
		}
	}
}

// normalizeWSRole 校验并归一 X-Client-Role；未知角色按 OneBot 默认的 universal 处理。
func normalizeWSRole(raw string) (role string, known bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", wsRoleUniversal:
		return wsRoleUniversal, true
	case wsRoleEvent:
		return wsRoleEvent, true
	case wsRoleAPI:
		return wsRoleAPI, true
	default:
		return wsRoleUniversal, false
	}
}

// wsHandler 返回反向 WebSocket 的接入处理函数。
//
// 处理流程：校验方法与鉴权 → 校验升级头与 X-Self-ID → 升级并接管连接 → 读循环。
// 与 HTTP 上报不同，OneBot 反向 WebSocket 不需要任何响应帧，没有 ACK 超时问题；
// 事件在收到后立即投递（sink.Emit 按约定必须快速返回）。
func (a *Adapter) wsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "only GET is supported", http.StatusMethodNotAllowed)
			return
		}
		if !a.authorized(r) {
			a.log.Warn("onebot: 反向 WebSocket 鉴权失败", "name", a.name, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := validateWebSocketUpgrade(r); err != nil {
			a.log.Warn("onebot: 反向 WebSocket 升级请求非法",
				"name", a.name, "remote", r.RemoteAddr, "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		headerSelfID := strings.TrimSpace(r.Header.Get("X-Self-ID"))
		if a.selfID != "" && headerSelfID != "" && headerSelfID != a.selfID {
			a.log.Warn("onebot: 反向 WebSocket 的 X-Self-ID 与配置不符",
				"name", a.name, "header", headerSelfID, "configured", a.selfID)
			http.Error(w, "self_id mismatch", http.StatusForbidden)
			return
		}
		role, known := normalizeWSRole(r.Header.Get("X-Client-Role"))
		if !known {
			a.log.Warn("onebot: 未知的 X-Client-Role，按 universal 处理",
				"name", a.name, "role", r.Header.Get("X-Client-Role"))
		}

		conn, err := hijackWebSocket(w, r)
		if err != nil {
			a.log.Warn("onebot: 反向 WebSocket 握手失败", "name", a.name, "remote", r.RemoteAddr, "err", err)
			return
		}

		session, err := a.hub.register(conn, firstNonEmpty(headerSelfID, a.selfID), role)
		if err != nil {
			_ = conn.closeWith(wsCloseGoingAway, nil)
			a.log.Warn("onebot: 拒绝反向 WebSocket 连接", "name", a.name, "remote", r.RemoteAddr, "err", err)
			return
		}

		a.log.Info("onebot: 反向 WebSocket 已连接",
			"name", a.name, "remote", r.RemoteAddr, "self_id", session.selfID, "role", role)
		err = session.serve(r.Context())
		a.hub.unregister(session)
		a.log.Info("onebot: 反向 WebSocket 已断开",
			"name", a.name, "remote", r.RemoteAddr, "self_id", session.selfID, "err", err)
	}
}
