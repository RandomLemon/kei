package onebot

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/pkg/message"
)

// errPeerClosed 表示测试客户端读到了对端的关闭帧。
var errPeerClosed = errors.New("peer closed")

// wsHandshake 是一次反向 WebSocket 握手的结果。
type wsHandshake struct {
	// code 是 HTTP 状态码，101 表示握手成功。
	code int
	// header 是握手响应头。
	header textproto.MIMEHeader
	// client 仅在握手成功时非 nil。
	client *testWSClient
}

// testWSClient 是测试用的最小 WebSocket 客户端（RFC 6455 客户端方向）。
//
// 客户端方向的帧必须带掩码，服务端帧必须不带掩码，这里都如实照做，
// 以便验证适配器对协议细节的处理。
type testWSClient struct {
	t      *testing.T
	conn   net.Conn
	br     *bufio.Reader
	noPong bool

	mu    sync.Mutex
	pings int
}

// dialWS 对 base（形如 http://127.0.0.1:1234）发起一次反向 WebSocket 握手。
//
// key 为空时随机生成；hdr 中的键值会作为额外请求头带上。
func dialWS(t *testing.T, base, path, key string, hdr map[string]string) wsHandshake {
	t.Helper()

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("解析基地址 %q 失败: %v", base, err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 %s 失败: %v", u.Host, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if key == "" {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			t.Fatalf("生成 Sec-WebSocket-Key 失败: %v", err)
		}
		key = base64.StdEncoding.EncodeToString(raw[:])
	}

	var sb strings.Builder
	sb.WriteString("GET " + path + " HTTP/1.1\r\n")
	sb.WriteString("Host: " + u.Host + "\r\n")
	sb.WriteString("Upgrade: websocket\r\n")
	sb.WriteString("Connection: Upgrade\r\n")
	sb.WriteString("Sec-WebSocket-Version: 13\r\n")
	sb.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	for k, v := range hdr {
		sb.WriteString(k + ": " + v + "\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		t.Fatalf("写握手请求失败: %v", err)
	}

	br := bufio.NewReader(conn)
	tp := textproto.NewReader(br)
	statusLine, err := tp.ReadLine()
	if err != nil {
		t.Fatalf("读取握手状态行失败: %v", err)
	}
	header, err := tp.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("读取握手响应头失败: %v", err)
	}
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		t.Fatalf("握手状态行格式异常: %q", statusLine)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("握手状态码非法: %q", statusLine)
	}

	hs := wsHandshake{code: code, header: header}
	if code == http.StatusSwitchingProtocols {
		hs.client = &testWSClient{t: t, conn: conn, br: br}
	}
	return hs
}

// connectWS 建立一条反向 WebSocket 连接并在失败时终止测试。
func connectWS(t *testing.T, base string, hdr map[string]string) *testWSClient {
	t.Helper()
	hs := dialWS(t, base, defaultWSPath, "", hdr)
	if hs.code != http.StatusSwitchingProtocols {
		t.Fatalf("反向 WebSocket 握手失败: 状态码 %d", hs.code)
	}
	return hs.client
}

// writeFrame 写一个客户端帧（带掩码）；fin 为 false 时是分片消息的首帧。
func (c *testWSClient) writeFrame(opcode byte, payload []byte, fin bool) {
	c.t.Helper()

	frame := make([]byte, 0, len(payload)+14)
	if fin {
		frame = append(frame, 0x80|opcode)
	} else {
		frame = append(frame, opcode)
	}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n <= 0xFFFF:
		frame = append(frame, 0x80|126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		frame = append(frame, ext[:]...)
	}

	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		c.t.Fatalf("生成掩码失败: %v", err)
	}
	frame = append(frame, key[:]...)
	for i, b := range payload {
		frame = append(frame, b^key[i%4])
	}

	if _, err := c.conn.Write(frame); err != nil {
		c.t.Fatalf("写帧失败: %v", err)
	}
}

// writeText 写一条文本消息。
func (c *testWSClient) writeText(body string) { c.writeFrame(wsOpText, []byte(body), true) }

// writeUnmasked 故意写一个不带掩码的文本帧，用于验证适配器的协议校验。
func (c *testWSClient) writeUnmasked(body string) {
	c.t.Helper()

	payload := []byte(body)
	frame := []byte{0x80 | wsOpText}
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

	if _, err := c.conn.Write(frame); err != nil {
		c.t.Fatalf("写未掩码帧失败: %v", err)
	}
}

// writeResponse 回写一次 API 调用的响应。
func (c *testWSClient) writeResponse(echo, messageID string) {
	c.t.Helper()
	c.writeText(fmt.Sprintf(`{"status":"ok","retcode":0,"data":{"message_id":%q},"echo":%q}`, messageID, echo))
}

// readFrame 读取一个服务端帧；超时由 timeout 控制。
func (c *testWSClient) readFrame(timeout time.Duration) (opcode byte, payload []byte, err error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		c.t.Fatalf("设置读超时失败: %v", err)
	}

	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return 0, nil, err
	}
	if head[0]&0x70 != 0 {
		c.t.Errorf("服务端帧不应设置 RSV 位: %#x", head[0])
	}
	opcode = head[0] & 0x0f
	if head[1]&0x80 != 0 {
		c.t.Error("服务端帧不应带掩码")
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

	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

// readFrameAutoPong 读取一个帧；收到 ping 时按 noPong 决定是否回 pong。
//
// 与 readMessage 不同，它每次只推进一个帧，便于测试逐个观察心跳等控制帧。
func (c *testWSClient) readFrameAutoPong(timeout time.Duration) (byte, []byte, error) {
	opcode, payload, err := c.readFrame(timeout)
	if err != nil {
		return 0, nil, err
	}
	if opcode == wsOpPing {
		c.notePing()
		if !c.noPong {
			c.writeFrame(wsOpPong, payload, true)
		}
	}
	return opcode, payload, nil
}

// readMessage 读取一条数据消息，自动回 pong 并计数 ping。
//
// 读到关闭帧时返回 errPeerClosed，载荷是关闭原因。
func (c *testWSClient) readMessage(timeout time.Duration) (byte, []byte, error) {
	for {
		opcode, payload, err := c.readFrameAutoPong(timeout)
		if err != nil {
			return 0, nil, err
		}
		switch opcode {
		case wsOpPing, wsOpPong:
			continue
		case wsOpClose:
			return opcode, payload, errPeerClosed
		default:
			return opcode, payload, nil
		}
	}
}

// readJSON 读取一条文本消息并解析为 map。
func (c *testWSClient) readJSON(timeout time.Duration) map[string]any {
	c.t.Helper()
	opcode, payload, err := c.readMessage(timeout)
	if err != nil {
		c.t.Fatalf("读取消息失败: %v", err)
	}
	if opcode != wsOpText {
		c.t.Fatalf("opcode = %#x, 期望文本帧", opcode)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		c.t.Fatalf("解析消息失败: %v (%s)", err, payload)
	}
	return out
}

// notePing 记录一次 ping 并做非阻塞通知。
func (c *testWSClient) notePing() {
	c.mu.Lock()
	c.pings++
	c.mu.Unlock()
}

// pingCount 返回收到的 ping 数。
func (c *testWSClient) pingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pings
}

// close 关闭底层连接。
func (c *testWSClient) close() { _ = c.conn.Close() }

// sessionCount 返回适配器当前登记的反向 WebSocket 连接数。
func sessionCount(a *Adapter) int {
	a.hub.mu.Lock()
	defer a.hub.mu.Unlock()
	return len(a.hub.sessions)
}

// waitForSessions 等待连接数达到期望值。
func waitForSessions(t *testing.T, a *Adapter, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sessionCount(a) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待反向 WebSocket 连接数 = %d 超时，实际 %d", want, sessionCount(a))
}

// sendOutcome 是一次后台发送的结果。
type sendOutcome struct {
	res *bot.SendResult
	err error
}

// sendAsync 在后台发起一次发送，返回结果通道。
func sendAsync(a *Adapter, req *bot.SendRequest) <-chan sendOutcome {
	ch := make(chan sendOutcome, 1)
	go func() {
		res, err := a.Send(context.Background(), req)
		ch <- sendOutcome{res: res, err: err}
	}()
	return ch
}

// sendResult 等待一次后台发送的结果。
func sendResult(t *testing.T, ch <-chan sendOutcome) (*bot.SendResult, error) {
	t.Helper()
	select {
	case out := <-ch:
		return out.res, out.err
	case <-time.After(3 * time.Second):
		t.Fatal("等待 Send 返回超时")
		return nil, nil
	}
}

func TestReverseWSHandshakeAndAccept(t *testing.T) {
	h := startHarness(t, Options{})

	// RFC 6455 §1.3 的样例：该 key 必须得到该 Accept 值。
	hs := dialWS(t, h.base, defaultWSPath, "dGhlIHNhbXBsZSBub25jZQ==", nil)
	if hs.code != http.StatusSwitchingProtocols {
		t.Fatalf("状态码 = %d, 期望 101", hs.code)
	}
	if got := hs.header.Get("Sec-Websocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("Sec-WebSocket-Accept = %q, 期望 RFC 样例值", got)
	}
	if got := hs.header.Get("Upgrade"); !strings.EqualFold(got, "websocket") {
		t.Errorf("Upgrade = %q, 期望 websocket", got)
	}
	hs.client.close()
}

func TestReverseWSRejectsInvalidRequests(t *testing.T) {
	h := startHarness(t, Options{})

	t.Run("非升级请求返回 400", func(t *testing.T) {
		resp, err := testClient.Get(h.base + defaultWSPath)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
		}
	})

	t.Run("非 GET 方法返回 405", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, h.base+defaultWSPath, nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		resp, err := testClient.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("状态码 = %d, 期望 405", resp.StatusCode)
		}
	})

	t.Run("子路径返回 404", func(t *testing.T) {
		hs := dialWS(t, h.base, defaultWSPath+"/extra", "", nil)
		if hs.code != http.StatusNotFound {
			t.Errorf("状态码 = %d, 期望 404", hs.code)
		}
	})

	t.Run("自定义 WSPath", func(t *testing.T) {
		custom := startHarness(t, Options{WSPath: "/custom/ws"})
		if hs := dialWS(t, custom.base, "/custom/ws", "", nil); hs.code != http.StatusSwitchingProtocols {
			t.Errorf("自定义路径状态码 = %d, 期望 101", hs.code)
		} else {
			hs.client.close()
		}
		if hs := dialWS(t, custom.base, defaultWSPath, "", nil); hs.code != http.StatusNotFound {
			t.Errorf("默认路径状态码 = %d, 期望 404", hs.code)
		}
	})
}

func TestReverseWSAuth(t *testing.T) {
	h := startHarness(t, Options{Secret: "s3cret"})

	cases := []struct {
		name   string
		path   string
		header map[string]string
		want   int
	}{
		{name: "无令牌", path: defaultWSPath, want: http.StatusUnauthorized},
		{name: "令牌错误", path: defaultWSPath + "?access_token=nope", want: http.StatusUnauthorized},
		{name: "query 令牌", path: defaultWSPath + "?access_token=s3cret", want: http.StatusSwitchingProtocols},
		{name: "Authorization 头", path: defaultWSPath, header: map[string]string{"Authorization": "Bearer s3cret"}, want: http.StatusSwitchingProtocols},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hs := dialWS(t, h.base, tc.path, "", tc.header)
			if hs.code != tc.want {
				t.Fatalf("状态码 = %d, 期望 %d", hs.code, tc.want)
			}
			if hs.client != nil {
				hs.client.close()
			}
		})
	}
}

func TestReverseWSReceivesEvents(t *testing.T) {
	h := startHarness(t, Options{SelfID: "10000"})
	client := connectWS(t, h.base, map[string]string{"X-Self-ID": "10000", "X-Client-Role": "Universal"})
	waitForSessions(t, h.adapter, 1)

	// 1) 整帧投递：事件字段应与 HTTP 上报路径完全一致，且不产生响应帧。
	client.writeText(groupArrayEvent)
	ev := h.sink.next(t)
	if ev.ID != "12345" || ev.Type != bot.EventMessage {
		t.Errorf("ID/Type = %q/%q, 期望 12345/message", ev.ID, ev.Type)
	}
	if ev.Platform != platformName || ev.BotID != "bot1" {
		t.Errorf("Platform/BotID = %q/%q, 期望 %q/bot1", ev.Platform, ev.BotID, platformName)
	}
	if ev.Channel == nil || ev.Channel.ID != "30001" || ev.Channel.Kind != bot.MessageGroup {
		t.Errorf("Channel = %+v, 期望 group 30001", ev.Channel)
	}
	if ev.Message == nil || ev.Message.ID != "12345" || len(ev.Message.Segments) != 6 {
		t.Fatalf("Message = %+v", ev.Message)
	}

	// 2) 分片消息：分两帧发送，必须被重组为同一条事件。
	body := []byte(privateCQEvent)
	client.writeFrame(wsOpText, body[:20], false)
	client.writeFrame(wsOpContinuation, body[20:], true)
	ev2 := h.sink.next(t)
	if ev2.ID != "12346" {
		t.Errorf("分片事件 ID = %q, 期望 12346", ev2.ID)
	}
	if ev2.Message == nil || len(ev2.Message.Segments) != 1 || ev2.Message.Segments[0].Data[bot.KeyText] != "你好 world" {
		t.Errorf("分片事件消息 = %+v", ev2.Message)
	}

	// 3) 重复投递同一事件：Event.ID 必须稳定（上游据此去重）。
	client.writeText(groupArrayEvent)
	if ev3 := h.sink.next(t); ev3.ID != ev.ID {
		t.Errorf("重复事件 ID = %q, 期望 %q", ev3.ID, ev.ID)
	}

	// 4) ping 必须被 pong 回应，且连接保持可用。
	client.writeFrame(wsOpPing, []byte("hb"), true)
	opcode, payload, err := client.readFrame(2 * time.Second)
	if err != nil {
		t.Fatalf("等待 pong 失败: %v", err)
	}
	if opcode != wsOpPong || string(payload) != "hb" {
		t.Errorf("响应帧 = %#x/%q, 期望 pong/hb", opcode, payload)
	}
	client.writeText(privateCQEvent)
	h.sink.next(t)

	// 5) 二进制帧同样按事件处理（兼容按二进制发送 JSON 的实现）。
	client.writeFrame(wsOpBinary, body, true)
	if ev5 := h.sink.next(t); ev5.ID != "12346" {
		t.Errorf("二进制帧事件 ID = %q, 期望 12346", ev5.ID)
	}
}

func TestReverseWSSend(t *testing.T) {
	// 不配置 api_url：发送只能走反向 WebSocket。
	h := startHarness(t, Options{})
	client := connectWS(t, h.base, map[string]string{"X-Self-ID": "10000"})
	waitForSessions(t, h.adapter, 1)

	ch := sendAsync(h.adapter, &bot.SendRequest{
		Target: bot.Target{ChannelID: "30001"},
		Message: &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
			message.Markdown("# 标题"),
			message.Text(" 正文"),
			message.Card(map[string]any{"k": "v"}),
		}},
	})

	req := client.readJSON(2 * time.Second)
	if req["action"] != actionSendGroup {
		t.Errorf("action = %v, 期望 %s", req["action"], actionSendGroup)
	}
	echo, _ := req["echo"].(string)
	if len(echo) != 36 {
		t.Errorf("echo = %q, 期望 UUID 形式", echo)
	}
	params, _ := req["params"].(map[string]any)
	if params["group_id"] != "30001" {
		t.Errorf("group_id = %v, 期望 30001", params["group_id"])
	}
	segs := messageParams(t, &apiCall{params: params})
	want := []onebotSegment{
		{Type: "text", Data: map[string]any{"text": "# 标题"}},
		{Type: "text", Data: map[string]any{"text": " 正文"}},
		{Type: "text", Data: map[string]any{"text": `[card] {"k":"v"}`}},
	}
	if len(segs) != len(want) {
		t.Fatalf("段数 = %d, 期望 %d: %+v", len(segs), len(want), segs)
	}
	for i := range want {
		if segs[i].Type != want[i].Type || segs[i].Data["text"] != want[i].Data["text"] {
			t.Errorf("第 %d 段 = %+v, 期望 %+v", i, segs[i], want[i])
		}
	}

	client.writeResponse(echo, "ws-1")
	res, err := sendResult(t, ch)
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.MessageID != "ws-1" {
		t.Errorf("MessageID = %q, 期望 ws-1", res.MessageID)
	}
	if raw, ok := res.Raw.(json.RawMessage); !ok || !strings.Contains(string(raw), `"ws-1"`) {
		t.Errorf("Raw = %v, 期望原始响应体", res.Raw)
	}
}

func TestReverseWSPrefersWebSocketOverHTTP(t *testing.T) {
	api, call := newFakeAPI(t, `{"status":"ok","retcode":0,"data":{"message_id":"http-1"}}`, http.StatusOK)
	h := startHarness(t, Options{APIURL: api.URL})
	client := connectWS(t, h.base, nil)
	waitForSessions(t, h.adapter, 1)

	ch := sendAsync(h.adapter, &bot.SendRequest{
		Target:  bot.Target{UserID: "20001"},
		Message: message.Plain(bot.MessagePrivate, "hi"),
	})
	req := client.readJSON(2 * time.Second)
	if req["action"] != actionSendPrivate {
		t.Errorf("action = %v, 期望 %s", req["action"], actionSendPrivate)
	}
	client.writeResponse(req["echo"].(string), "ws-2")

	res, err := sendResult(t, ch)
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.MessageID != "ws-2" {
		t.Errorf("MessageID = %q, 期望走反向 WebSocket 得到的 ws-2", res.MessageID)
	}
	if call.action != "" {
		t.Errorf("有反向 WebSocket 连接时不应调用 HTTP API，实际调用 %q", call.action)
	}
}

func TestReverseWSMatchesConcurrentResponses(t *testing.T) {
	h := startHarness(t, Options{})
	client := connectWS(t, h.base, nil)
	waitForSessions(t, h.adapter, 1)

	group := sendAsync(h.adapter, &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
		Message: message.Plain(bot.MessageGroup, "g"),
	})
	private := sendAsync(h.adapter, &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessagePrivate, UserID: "20001"},
		Message: message.Plain(bot.MessagePrivate, "p"),
	})

	// 两帧请求都到达后，按相反顺序回响应，验证按 echo 精确对应。
	seen := make(map[string]string, 2)
	for i := 0; i < 2; i++ {
		req := client.readJSON(2 * time.Second)
		action, _ := req["action"].(string)
		seen[action] = req["echo"].(string)
	}
	for action, echo := range map[string]string{
		actionSendPrivate: "private-1",
		actionSendGroup:   "group-1",
	} {
		client.writeResponse(seen[action], echo)
	}

	if res, err := sendResult(t, group); err != nil || res.MessageID != "group-1" {
		t.Errorf("群聊结果 = %+v, err = %v, 期望 group-1", res, err)
	}
	if res, err := sendResult(t, private); err != nil || res.MessageID != "private-1" {
		t.Errorf("私聊结果 = %+v, err = %v, 期望 private-1", res, err)
	}
}

func TestReverseWSEventRoleFallsBackToHTTP(t *testing.T) {
	api, call := newFakeAPI(t, `{"status":"ok","retcode":0,"data":{"message_id":"http-1"}}`, http.StatusOK)
	h := startHarness(t, Options{APIURL: api.URL})
	client := connectWS(t, h.base, map[string]string{"X-Client-Role": "Event"})
	waitForSessions(t, h.adapter, 1)

	res, err := h.adapter.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
		Message: message.Plain(bot.MessageGroup, "hi"),
	})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if call.action != actionSendGroup || res.MessageID != "http-1" {
		t.Errorf("action/MessageID = %q/%q, 期望走 HTTP API", call.action, res.MessageID)
	}

	// 事件角色连接只接收事件，不应收到任何 API 请求帧。
	if opcode, payload, err := client.readMessage(200 * time.Millisecond); err == nil {
		t.Errorf("role=event 的连接收到了下行帧 %#x: %s", opcode, payload)
	}
}

func TestReverseWSNoConnectionAndNoAPIURL(t *testing.T) {
	h := startHarness(t, Options{})
	_, err := h.adapter.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
		Message: message.Plain(bot.MessageGroup, "hi"),
	})
	if err == nil {
		t.Fatal("既无反向 WebSocket 连接也未配置 api_url 时应返回错误")
	}
	if !strings.Contains(err.Error(), "api_url") {
		t.Errorf("错误信息应指出缺少 api_url: %v", err)
	}
}

func TestReverseWSSelfIDMismatch(t *testing.T) {
	h := startHarness(t, Options{SelfID: "10000"})

	if hs := dialWS(t, h.base, defaultWSPath, "", map[string]string{"X-Self-ID": "10001"}); hs.code != http.StatusForbidden {
		t.Errorf("不匹配的 X-Self-ID 状态码 = %d, 期望 403", hs.code)
	}
	if hs := dialWS(t, h.base, defaultWSPath, "", map[string]string{"X-Self-ID": "10000"}); hs.code != http.StatusSwitchingProtocols {
		t.Errorf("匹配的 X-Self-ID 状态码 = %d, 期望 101", hs.code)
	} else {
		hs.client.close()
	}
	// 未带 X-Self-ID 时回落到配置值。
	if hs := dialWS(t, h.base, defaultWSPath, "", nil); hs.code != http.StatusSwitchingProtocols {
		t.Errorf("缺少 X-Self-ID 状态码 = %d, 期望 101", hs.code)
	} else {
		hs.client.close()
	}
}

func TestReverseWSMultipleConnections(t *testing.T) {
	h := startHarness(t, Options{})
	first := connectWS(t, h.base, map[string]string{"X-Self-ID": "10000"})
	second := connectWS(t, h.base, map[string]string{"X-Self-ID": "20000"})
	waitForSessions(t, h.adapter, 2)

	// 多连接时发送固定走最早建立的连接，行为可预测。
	ch := sendAsync(h.adapter, &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
		Message: message.Plain(bot.MessageGroup, "hi"),
	})
	req := first.readJSON(2 * time.Second)
	first.writeResponse(req["echo"].(string), "first")

	if res, err := sendResult(t, ch); err != nil || res.MessageID != "first" {
		t.Fatalf("Send 结果 = %+v, err = %v, 期望走最早建立的连接", res, err)
	}
	if opcode, payload, err := second.readMessage(200 * time.Millisecond); err == nil {
		t.Errorf("后建立的连接不应收到请求帧 %#x: %s", opcode, payload)
	}

	// 每条连接都能上行事件，self_id 不同的连接也一样。
	second.writeText(privateCQEvent)
	if ev := h.sink.next(t); ev.ID != "12346" {
		t.Errorf("事件 ID = %q, 期望 12346", ev.ID)
	}
	first.writeText(groupArrayEvent)
	if ev := h.sink.next(t); ev.ID != "12345" {
		t.Errorf("事件 ID = %q, 期望 12345", ev.ID)
	}
}

func TestReverseWSKeepAliveAndPruning(t *testing.T) {
	t.Run("对端回应 pong 时连接保持并可发送", func(t *testing.T) {
		h := startHarness(t, Options{PingInterval: 30 * time.Millisecond})
		client := connectWS(t, h.base, nil)
		waitForSessions(t, h.adapter, 1)

		deadline := time.Now().Add(2 * time.Second)
		for client.pingCount() == 0 && time.Now().Before(deadline) {
			// 心跳是服务端主动发出的：逐帧读取并回 pong，验证连接保持存活。
			op, payload, err := client.readFrameAutoPong(100 * time.Millisecond)
			if err != nil {
				t.Fatalf("等待心跳失败: %v", err)
			}
			if op != wsOpPing && op != wsOpPong {
				t.Fatalf("心跳期间收到非控制帧 %#x: %s", op, payload)
			}
		}
		if client.pingCount() == 0 {
			t.Fatal("未收到心跳 ping")
		}
		if sessionCount(h.adapter) != 1 {
			t.Errorf("心跳后连接数 = %d, 期望 1", sessionCount(h.adapter))
		}

		ch := sendAsync(h.adapter, &bot.SendRequest{
			Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
			Message: message.Plain(bot.MessageGroup, "hi"),
		})
		req := client.readJSON(2 * time.Second)
		client.writeResponse(req["echo"].(string), "alive")
		if res, err := sendResult(t, ch); err != nil || res.MessageID != "alive" {
			t.Fatalf("Send 结果 = %+v, err = %v", res, err)
		}
	})

	t.Run("对端不回 pong 时连接被回收", func(t *testing.T) {
		h := startHarness(t, Options{PingInterval: 30 * time.Millisecond})
		client := connectWS(t, h.base, nil)
		client.noPong = true
		waitForSessions(t, h.adapter, 1)

		deadline := time.Now().Add(3 * time.Second)
		for sessionCount(h.adapter) != 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := sessionCount(h.adapter); got != 0 {
			t.Fatalf("半开连接未被回收，连接数 = %d", got)
		}

		if _, err := h.adapter.Send(context.Background(), &bot.SendRequest{
			Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
			Message: message.Plain(bot.MessageGroup, "hi"),
		}); err == nil {
			t.Error("连接被回收后发送应失败")
		}
	})
}

func TestReverseWSProtocolErrors(t *testing.T) {
	t.Run("未掩码帧导致连接关闭", func(t *testing.T) {
		h := startHarness(t, Options{})
		client := connectWS(t, h.base, nil)
		waitForSessions(t, h.adapter, 1)

		client.writeUnmasked(groupArrayEvent)
		if _, _, err := client.readMessage(2 * time.Second); err == nil {
			t.Error("协议错误后连接应关闭")
		}
		waitForSessions(t, h.adapter, 0)
	})

	t.Run("未知 echo 的响应被忽略且连接存活", func(t *testing.T) {
		h := startHarness(t, Options{})
		client := connectWS(t, h.base, nil)
		waitForSessions(t, h.adapter, 1)

		client.writeResponse("unknown-echo", "x")
		ch := sendAsync(h.adapter, &bot.SendRequest{
			Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
			Message: message.Plain(bot.MessageGroup, "hi"),
		})
		req := client.readJSON(2 * time.Second)
		client.writeResponse(req["echo"].(string), "ok-1")
		if res, err := sendResult(t, ch); err != nil || res.MessageID != "ok-1" {
			t.Fatalf("Send 结果 = %+v, err = %v", res, err)
		}
	})

	t.Run("无法处理的消息只记日志", func(t *testing.T) {
		h := startHarness(t, Options{})
		client := connectWS(t, h.base, nil)
		waitForSessions(t, h.adapter, 1)

		client.writeText(`{"foo":"bar"}`)
		client.writeText(groupArrayEvent)
		if ev := h.sink.next(t); ev.ID != "12345" {
			t.Errorf("事件 ID = %q, 期望 12345", ev.ID)
		}
	})
}

func TestReverseWSStopClosesConnections(t *testing.T) {
	h := startHarness(t, Options{})
	client := connectWS(t, h.base, nil)
	waitForSessions(t, h.adapter, 1)

	if err := h.adapter.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if got := sessionCount(h.adapter); got != 0 {
		t.Errorf("Stop 后连接数 = %d, 期望 0", got)
	}

	opcode, payload, err := client.readMessage(2 * time.Second)
	if err == nil {
		t.Fatalf("Stop 后仍读到数据帧 %#x: %s", opcode, payload)
	}
	if errors.Is(err, errPeerClosed) {
		if len(payload) < 2 {
			t.Fatalf("关闭帧载荷过短: %v", payload)
		}
		if code := binary.BigEndian.Uint16(payload[:2]); code != wsCloseGoingAway {
			t.Errorf("关闭码 = %d, 期望 %d", code, wsCloseGoingAway)
		}
	}
}

func TestReverseWSClientDisconnectUnblocksSend(t *testing.T) {
	h := startHarness(t, Options{})
	client := connectWS(t, h.base, nil)
	waitForSessions(t, h.adapter, 1)

	ch := sendAsync(h.adapter, &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessageGroup, ChannelID: "30001"},
		Message: message.Plain(bot.MessageGroup, "hi"),
	})
	client.readJSON(2 * time.Second)

	// 对端在响应前断开：等待中的调用必须立刻失败，而不是等到 ctx 超时。
	client.close()
	_, err := sendResult(t, ch)
	if err == nil {
		t.Fatal("连接断开后 Send 应返回错误")
	}
	if !errors.Is(err, errWSClosed) && !strings.Contains(err.Error(), "WebSocket") {
		t.Errorf("错误信息应指出反向 WebSocket 失败: %v", err)
	}
	waitForSessions(t, h.adapter, 0)
}
