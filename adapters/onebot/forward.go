package onebot

import (
	"context"
	"net/http"
	"time"
)

// 本文件实现正向 WebSocket：kei 作为客户端主动连接 OneBot 实现的 WebSocket 服务端，
// 事件与 API 调用共用这条连接（与反向 WebSocket 完全对称，只是拨号方向相反）。

const (
	// forwardBackoffBase 是重连的初始间隔。
	forwardBackoffBase = time.Second
	// forwardBackoffMax 是重连间隔的上限。
	forwardBackoffMax = 30 * time.Second
)

// forwardConfig 是正向 WebSocket 的连接参数。
type forwardConfig struct {
	// url 是 OneBot 实现的 WebSocket 地址，形如 ws://127.0.0.1:6700。
	url string
	// accessToken 非空时以 Authorization: Bearer <token> 随握手请求发送。
	accessToken string
}

// startForward 启动正向 WebSocket 连接循环；适配器已在关闭时不做任何事。
//
// 与 register 使用同样的加锁模式：先把 goroutine 计入 wg 再释放锁，保证 shutdown
// 之后不会再起新的 goroutine。
func (h *wsHub) startForward(ctx context.Context, cfg forwardConfig) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.wg.Add(1)
	h.mu.Unlock()

	go func() {
		defer h.wg.Done()
		h.forwardLoop(ctx, cfg)
	}()
}

// stopped 报告正向连接循环是否应当退出：ctx 结束或 hub 已关闭都算。
func (h *wsHub) stopped(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// forwardLoop 反复拨号直到 ctx 结束或 hub 关闭，失败时按指数退避重试。
func (h *wsHub) forwardLoop(ctx context.Context, cfg forwardConfig) {
	backoff := forwardBackoffBase
	for !h.stopped(ctx) {
		if h.forwardOnce(ctx, cfg) {
			backoff = forwardBackoffBase // 成功建立过连接：重置退避
		} else {
			backoff = min(backoff*2, forwardBackoffMax) // 仅拨号失败时增长
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
	}
}

// forwardOnce 建立一条正向连接并在其上服务，直到连接结束。
//
// 返回值表示本轮是否成功建立过连接（已建立即返回 true，即使随后断开），
// 供 forwardLoop 决定是否重置退避。
func (h *wsHub) forwardOnce(ctx context.Context, cfg forwardConfig) bool {
	header := make(http.Header)
	if cfg.accessToken != "" {
		header.Set("Authorization", "Bearer "+cfg.accessToken)
	}

	conn, err := dialWebSocket(ctx, cfg.url, header)
	if err != nil {
		if ctx.Err() == nil {
			h.log.Warn("onebot: 正向 WebSocket 连接失败",
				"name", h.botID, "url", cfg.url, "err", err)
		}
		return false
	}

	s, err := h.register(conn, h.selfID, wsRoleUniversal)
	if err != nil {
		// hub 已关闭：放弃这条连接，下一轮循环由 stopped 退出。
		_ = conn.closeWith(wsCloseGoingAway, nil)
		return true
	}

	h.log.Info("onebot: 正向 WebSocket 已连接", "name", h.botID, "url", cfg.url)
	err = s.serve(ctx)
	h.unregister(s)
	h.log.Info("onebot: 正向 WebSocket 已断开", "name", h.botID, "url", cfg.url, "err", err)
	return true
}

// sleepCtx 等待 d，ctx 结束时提前返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
