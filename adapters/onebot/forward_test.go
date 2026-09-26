package onebot

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/pkg/message"
)

// testWSServer 是测试用的最小 WebSocket 服务端（RFC 6455 服务端方向），
// 与 reverse_test.go 的 testWSClient 对称。
//
// 它刻意不复用生产 wsConn：掩码方向上的错误如果两端共用同一份实现，会互相掩盖。
// Sec-WebSocket-Accept 也在本文件独立计算。
type testWSServer struct {
	t     *testing.T
	ln    net.Listener
	conns chan *testWSServerConn
}

func newTestWSServer(t *testing.T) *testWSServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	s := &testWSServer{t: t, ln: ln, conns: make(chan *testWSServerConn, 8)}
	go s.acceptLoop()
	t.Cleanup(func() {
		_ = ln.Close()
	})
	return s
}

// URL 返回测试服务端的 WebSocket 地址。
func (s *testWSServer) URL() string { return "ws://" + s.ln.Addr().String() + "/onebot/ws" }

func (s *testWSServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handshake(conn)
	}
}

// handshake 读入一次升级请求、回 101，并把连接交给 accept 的调用方。
func (s *testWSServer) handshake(conn net.Conn) {
	br := bufio.NewReader(conn)
	tp := textproto.NewReader(br)
	reqLine, err := tp.ReadLine()
	if err != nil {
		_ = conn.Close()
		return
	}
	header, err := tp.ReadMIMEHeader()
	if err != nil {
		_ = conn.Close()
		return
	}

	accept := testAcceptKey(header.Get("Sec-WebSocket-Key"))
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := io.WriteString(conn, resp); err != nil {
		_ = conn.Close()
		return
	}
	s.conns <- &testWSServerConn{t: s.t, conn: conn, br: br, reqLine: reqLine, header: http.Header(header)}
}

// accept 等待 kei 建立一条正向连接。
func (s *testWSServer) accept(t *testing.T) *testWSServerConn {
	t.Helper()
	select {
	case c := <-s.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("等待 kei 的正向 WebSocket 连接超时")
		return nil
	}
}

// testAcceptKey 独立实现 Sec-WebSocket-Accept，避免与生产实现同错同对。
func testAcceptKey(key string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(key) + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// testWSServerConn 是一条被测试服务端接管的连接。
type testWSServerConn struct {
	t       *testing.T
	conn    net.Conn
	br      *bufio.Reader
	reqLine string
	header  http.Header
}

// readFrame 读取一个客户端帧并强制要求它带掩码（RFC 6455 §5.1）。
func (c *testWSServerConn) readFrame(timeout time.Duration) (opcode byte, payload []byte, err error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		c.t.Fatalf("设置读超时失败: %v", err)
	}

	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return 0, nil, err
	}
	if head[0]&0x70 != 0 {
		c.t.Errorf("客户端帧不应设置 RSV 位: %#x", head[0])
	}
	opcode = head[0] & 0x0f
	if head[1]&0x80 == 0 {
		c.t.Fatalf("客户端帧必须带掩码: %#x", head[1])
	}

	length := int64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}

	var key [4]byte
	if _, err := io.ReadFull(c.br, key[:]); err != nil {
		return 0, nil, err
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return opcode, payload, nil
}

// readMessage 读取一条数据消息，自动回 pong；关闭帧返回 errPeerClosed。
func (c *testWSServerConn) readMessage(timeout time.Duration) (byte, []byte, error) {
	for {
		opcode, payload, err := c.readFrame(timeout)
		if err != nil {
			return 0, nil, err
		}
		switch opcode {
		case wsOpPing:
			if err := c.writeFrame(wsOpPong, payload); err != nil {
				return 0, nil, err
			}
		case wsOpPong:
			continue
		case wsOpClose:
			return opcode, payload, errPeerClosed
		default:
			return opcode, payload, nil
		}
	}
}

// readAPIRequest 读取一条文本消息并解析为 OneBot API 请求。
func (c *testWSServerConn) readAPIRequest(timeout time.Duration) apiRequest {
	c.t.Helper()
	opcode, payload, err := c.readMessage(timeout)
	if err != nil {
		c.t.Fatalf("读取 API 请求失败: %v", err)
	}
	if opcode != wsOpText {
		c.t.Fatalf("opcode = %#x, 期望文本帧", opcode)
	}
	var req apiRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		c.t.Fatalf("解析 API 请求失败: %v (%s)", err, payload)
	}
	return req
}

// writeFrame 写一个不带掩码的服务端帧。
func (c *testWSServerConn) writeFrame(opcode byte, payload []byte) error {
	frame := make([]byte, 0, len(payload)+10)
	frame = append(frame, 0x80|opcode)
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, byte(n))
	case n <= 0xFFFF:
		frame = append(frame, 126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		frame = append(frame, ext[:]...)
	}
	frame = append(frame, payload...)
	if err := c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	_, err := c.conn.Write(frame)
	return err
}

// writeText 写一条文本消息。
func (c *testWSServerConn) writeText(body string) {
	c.t.Helper()
	if err := c.writeFrame(wsOpText, []byte(body)); err != nil {
		c.t.Fatalf("写文本帧失败: %v", err)
	}
}

// writeResponse 回写一次 API 调用的响应（不带掩码）。
func (c *testWSServerConn) writeResponse(echo, messageID string) {
	c.t.Helper()
	c.writeText(fmt.Sprintf(`{"status":"ok","retcode":0,"data":{"message_id":%q},"echo":%q}`, messageID, echo))
}

// close 关闭底层连接。
func (c *testWSServerConn) close() { _ = c.conn.Close() }

// forwardHarness 是一次正向 WebSocket 测试运行实例。
type forwardHarness struct {
	adapter *Adapter
	sink    *sinkRecorder
	cancel  context.CancelFunc
	done    chan error
	drain   sync.Once
	drained chan struct{}
}

// startForward 以 forward_ws 启动适配器并返回运行句柄。
func startForward(t *testing.T, opts Options) *forwardHarness {
	t.Helper()
	if opts.Name == "" {
		opts.Name = "bot1"
	}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}

	a, err := New(opts)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sink := newSinkRecorder()
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx, sink) }()

	h := &forwardHarness{adapter: a, sink: sink, cancel: cancel, done: done, drained: make(chan struct{})}
	t.Cleanup(func() {
		cancel()
		_ = a.Stop(context.Background())
		select {
		case <-h.drained:
		default:
			if err := h.waitDone(t); err != nil {
				t.Errorf("Start 返回错误: %v", err)
			}
		}
	})
	return h
}

// waitDone 等待 Start 退出并返回它的错误；重复调用返回同一结果。
func (h *forwardHarness) waitDone(t *testing.T) error {
	t.Helper()
	var err error
	h.drain.Do(func() {
		close(h.drained)
		select {
		case err = <-h.done:
		case <-time.After(5 * time.Second):
			t.Errorf("Start 未在超时内退出")
		}
	})
	return err
}

func TestForwardWSDialHandshake(t *testing.T) {
	srv := newTestWSServer(t)
	h := startForward(t, Options{
		Mode:        modeForwardWS,
		WSURL:       srv.URL(),
		AccessToken: "t",
	})

	conn := srv.accept(t)
	if !strings.HasPrefix(conn.reqLine, "GET /onebot/ws ") {
		t.Errorf("请求行 = %q, 期望 GET /onebot/ws", conn.reqLine)
	}
	if got := conn.header.Get("Upgrade"); !strings.EqualFold(got, "websocket") {
		t.Errorf("Upgrade = %q, 期望 websocket", got)
	}
	if !headerHasToken(conn.header.Get("Connection"), "upgrade") {
		t.Errorf("Connection = %q, 期望含 upgrade", conn.header.Get("Connection"))
	}
	if conn.header.Get("Sec-WebSocket-Key") == "" {
		t.Error("Sec-WebSocket-Key 不能为空")
	}
	if got := conn.header.Get("Sec-WebSocket-Version"); got != "13" {
		t.Errorf("Sec-WebSocket-Version = %q, 期望 13", got)
	}
	if got := conn.header.Get("Authorization"); got != "Bearer t" {
		t.Errorf("Authorization = %q, 期望 Bearer t", got)
	}

	waitForSessions(t, h.adapter, 1)
	if got := h.adapter.Addr(); got != "" {
		t.Errorf("forward_ws 下 Addr() = %q, 期望空字符串", got)
	}
}

func TestDialWebSocketRejectsBadHandshake(t *testing.T) {
	// rawWSServer 用固定响应伪造一个握手失败的 WebSocket 服务端。
	rawWSServer := func(t *testing.T, respond func(key string) string) string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("监听失败: %v", err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			tp := textproto.NewReader(bufio.NewReader(conn))
			if _, err := tp.ReadLine(); err != nil {
				return
			}
			header, err := tp.ReadMIMEHeader()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, respond(header.Get("Sec-WebSocket-Key")))
		}()
		return "ws://" + ln.Addr().String() + "/onebot/ws"
	}

	cases := []struct {
		name    string
		respond func(key string) string
		wantSub string
	}{
		{
			name:    "状态码不是 101",
			respond: func(string) string { return "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n" },
			wantSub: "握手返回",
		},
		{
			name: "Sec-WebSocket-Accept 不匹配",
			respond: func(string) string {
				return "HTTP/1.1 101 Switching Protocols\r\n" +
					"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
					"Sec-WebSocket-Accept: " + testAcceptKey("wrong") + "\r\n\r\n"
			},
			wantSub: "Sec-WebSocket-Accept 不匹配",
		},
		{
			name: "缺少 Upgrade 头",
			respond: func(key string) string {
				return "HTTP/1.1 101 Switching Protocols\r\n" +
					"Connection: Upgrade\r\n" +
					"Sec-WebSocket-Accept: " + testAcceptKey(key) + "\r\n\r\n"
			},
			wantSub: "Upgrade",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := rawWSServer(t, tc.respond)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, err := dialWebSocket(ctx, url, nil)
			if err == nil {
				_ = conn.close()
				t.Fatal("期望握手失败，实际成功")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息 = %q, 期望包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestForwardWSReceivesEvents(t *testing.T) {
	srv := newTestWSServer(t)
	h := startForward(t, Options{Mode: modeForwardWS, WSURL: srv.URL()})
	conn := srv.accept(t)
	waitForSessions(t, h.adapter, 1)

	conn.writeText(groupArrayEvent)
	ev := h.sink.next(t)
	if ev.Platform != platformName || ev.BotID != "bot1" {
		t.Errorf("Platform/BotID = %q/%q, 期望 %q/bot1", ev.Platform, ev.BotID, platformName)
	}
	if ev.ID != "12345" || ev.Type != bot.EventMessage {
		t.Errorf("ID/Type = %q/%q, 期望 12345/message（与 HTTP 上报路径一致）", ev.ID, ev.Type)
	}
	if ev.Message == nil || len(ev.Message.Segments) != 6 {
		t.Fatalf("Message = %+v", ev.Message)
	}
}

func TestForwardWSSend(t *testing.T) {
	srv := newTestWSServer(t)
	h := startForward(t, Options{Mode: modeForwardWS, WSURL: srv.URL()})
	conn := srv.accept(t)
	waitForSessions(t, h.adapter, 1)

	ch := sendAsync(h.adapter, &bot.SendRequest{
		Target: bot.Target{ChannelID: "30001"},
		Message: &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
			message.Markdown("# 标题"),
			message.Text(" 正文"),
		}},
	})

	req := conn.readAPIRequest(3 * time.Second)
	if req.Action != actionSendGroup {
		t.Errorf("action = %q, 期望 %q", req.Action, actionSendGroup)
	}
	if got := req.Params["group_id"]; got != "30001" {
		t.Errorf("group_id = %v, 期望 30001", got)
	}
	if len(req.Echo) != 36 {
		t.Errorf("echo = %q, 期望 UUID 形式", req.Echo)
	}
	segs := messageParams(t, &apiCall{params: req.Params})
	want := []onebotSegment{
		{Type: "text", Data: map[string]any{"text": "# 标题"}},
		{Type: "text", Data: map[string]any{"text": " 正文"}},
	}
	if len(segs) != len(want) {
		t.Fatalf("段数 = %d, 期望 %d: %+v", len(segs), len(want), segs)
	}
	for i := range want {
		if segs[i].Type != want[i].Type || segs[i].Data["text"] != want[i].Data["text"] {
			t.Errorf("第 %d 段 = %+v, 期望 %+v", i, segs[i], want[i])
		}
	}

	conn.writeResponse(req.Echo, "12345")
	res, err := sendResult(t, ch)
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.MessageID != "12345" {
		t.Errorf("MessageID = %q, 期望 12345", res.MessageID)
	}
}

func TestForwardWSPrefersWSOverHTTP(t *testing.T) {
	api, call := newFakeAPI(t, `{"status":"ok","retcode":0,"data":{"message_id":"http-1"}}`, http.StatusOK)
	srv := newTestWSServer(t)
	h := startForward(t, Options{Mode: modeForwardWS, WSURL: srv.URL(), APIURL: api.URL})
	conn := srv.accept(t)
	waitForSessions(t, h.adapter, 1)

	ch := sendAsync(h.adapter, &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessagePrivate, UserID: "20001"},
		Message: message.Plain(bot.MessagePrivate, "hi"),
	})
	req := conn.readAPIRequest(3 * time.Second)
	if req.Action != actionSendPrivate {
		t.Errorf("action = %q, 期望 %q", req.Action, actionSendPrivate)
	}
	conn.writeResponse(req.Echo, "ws-1")

	res, err := sendResult(t, ch)
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.MessageID != "ws-1" {
		t.Errorf("MessageID = %q, 期望走正向 WebSocket 得到的 ws-1", res.MessageID)
	}
	if call.action != "" {
		t.Errorf("有 WebSocket 连接时不应调用 HTTP API，实际调用 %q", call.action)
	}
}

func TestForwardWSReconnect(t *testing.T) {
	srv := newTestWSServer(t)
	h := startForward(t, Options{Mode: modeForwardWS, WSURL: srv.URL()})

	first := srv.accept(t)
	waitForSessions(t, h.adapter, 1)

	// 服务端主动断开：适配器必须在退避后重连（基值 1s）。
	first.close()
	second := srv.accept(t)
	waitForSessions(t, h.adapter, 1)

	ch := sendAsync(h.adapter, &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
		Message: message.Plain(bot.MessageGroup, "hi"),
	})
	req := second.readAPIRequest(3 * time.Second)
	second.writeResponse(req.Echo, "after-reconnect")
	if res, err := sendResult(t, ch); err != nil || res.MessageID != "after-reconnect" {
		t.Fatalf("重连后 Send 结果 = %+v, err = %v, 期望 after-reconnect", res, err)
	}
}

func TestForwardWSStopClosesConnection(t *testing.T) {
	srv := newTestWSServer(t)
	h := startForward(t, Options{Mode: modeForwardWS, WSURL: srv.URL()})
	conn := srv.accept(t)
	waitForSessions(t, h.adapter, 1)

	h.cancel()
	if err := h.waitDone(t); err != nil {
		t.Errorf("Start 返回错误: %v", err)
	}
	if err := h.adapter.Stop(context.Background()); err != nil {
		t.Errorf("Stop 失败: %v", err)
	}
	if got := sessionCount(h.adapter); got != 0 {
		t.Errorf("Stop 后连接数 = %d, 期望 0", got)
	}

	if _, _, err := conn.readMessage(2 * time.Second); err == nil {
		t.Error("Stop 后测试服务端应读到连接关闭")
	}
}
