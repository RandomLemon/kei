package onebot

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 本文件是反向 WebSocket 所需的 RFC 6455 服务端最小实现。
//
// 只覆盖 OneBot 反向 WebSocket 用到的部分：握手校验、文本/二进制数据帧、
// 分片重组、ping/pong 与关闭握手；不做扩展协商（含压缩）、不实现客户端方向，
// 以维持本适配器零第三方依赖的约束（见包注释）。

// WebSocket 操作码（RFC 6455 §5.2）。
const (
	wsOpContinuation byte = 0x0
	wsOpText         byte = 0x1
	wsOpBinary       byte = 0x2
	wsOpClose        byte = 0x8
	wsOpPing         byte = 0x9
	wsOpPong         byte = 0xA
)

const (
	// wsHandshakeGUID 是 RFC 6455 §1.3 规定的握手固定值。
	wsHandshakeGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	// wsVersion 是本实现支持的 WebSocket 协议版本。
	wsVersion = "13"
	// wsWriteTimeout 是单次帧写入的超时上限。
	wsWriteTimeout = 10 * time.Second
	// wsMaxControlPayload 是控制帧允许的最大载荷长度（RFC 6455 §5.5）。
	wsMaxControlPayload = 125
)

// 关闭帧状态码（RFC 6455 §7.4.1）。
const (
	// wsCloseNormal 表示正常关闭。
	wsCloseNormal uint16 = 1000
	// wsCloseGoingAway 表示本端正在关闭（适配器退出）。
	wsCloseGoingAway uint16 = 1001
	// wsCloseTooLarge 表示收到的消息超过处理上限。
	wsCloseTooLarge uint16 = 1009
)

// errWSClosed 表示对端发起了关闭握手。
var errWSClosed = errors.New("onebot: 对端关闭了 WebSocket 连接")

// wsConn 是一条已完成握手的 WebSocket 连接（服务端方向）。
//
// 读必须由单个 goroutine 串行调用（readMessage）；写可由多个 goroutine 并发
// 调用（writeMu 串行化），因此同一条连接上可以并行等待多个 API 调用的响应。
// 握手后底层连接不再由 net/http 管理：必须显式关闭，且关闭前尽量发关闭帧。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu   sync.Mutex
	closeOnce sync.Once
}

// validateWebSocketUpgrade 校验升级请求的必要头。返回的错误可直接回给客户端。
func validateWebSocketUpgrade(r *http.Request) error {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return errors.New("缺少 Upgrade: websocket 头")
	}
	if !headerHasToken(r.Header.Get("Connection"), "upgrade") {
		return errors.New("缺少 Connection: Upgrade 头")
	}
	if v := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); v != wsVersion {
		return fmt.Errorf("不支持的 Sec-WebSocket-Version %q，需要 %s", v, wsVersion)
	}
	if strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key")) == "" {
		return errors.New("缺少 Sec-WebSocket-Key 头")
	}
	return nil
}

// headerHasToken 判断逗号分隔的头字段中是否包含指定 token（大小写不敏感）。
func headerHasToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// hijackWebSocket 回写 101 响应并接管底层连接。
//
// 调用前必须已通过 validateWebSocketUpgrade 与业务鉴权：一旦 Hijack 成功，
// 就无法再回常规 HTTP 错误，只能靠关闭连接告知对端。
func hijackWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("HTTP 响应器不支持 Hijack")
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("接管连接失败: %w", err)
	}
	// net/http 可能按 ReadTimeout/WriteTimeout 给连接设过超时，接管后必须清除，
	// 否则长连接会在若干秒后被误杀。
	if err := netConn.SetDeadline(time.Time{}); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("清除连接超时失败: %w", err)
	}

	// brw.Reader 可能已缓存对端在握手后立即发出的帧，必须继续使用它读取。
	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("写握手响应失败: %w", err)
	}
	if err := brw.Flush(); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("刷新握手响应失败: %w", err)
	}
	return &wsConn{conn: netConn, br: brw.Reader}, nil
}

// wsAcceptKey 计算握手响应的 Sec-WebSocket-Accept。
//
// RFC 6455 规定使用 SHA-1，这里只为协议兼容，不用于任何安全目的。
func wsAcceptKey(key string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(key) + wsHandshakeGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// readMessage 读取一条完整消息，自动处理分片、ping/pong 与关闭握手。
//
// readTimeout 为正值时，每读到一个帧都会顺延读超时：任何帧（含对端的 pong）
// 都算存活证明，因此必须按帧而非按消息刷新，否则只有 pong 往返的连接会被误判失效。
// 返回的 opcode 为 wsOpText 或 wsOpBinary；对端关闭时返回 errWSClosed。
func (c *wsConn) readMessage(readTimeout time.Duration) (byte, []byte, error) {
	var (
		buf    []byte
		opcode byte
	)
	for {
		fin, op, payload, err := c.readFrame(readTimeout)
		if err != nil {
			return 0, nil, err
		}

		switch op {
		case wsOpPing:
			if err := c.writeFrame(wsOpPong, payload, time.Now().Add(wsWriteTimeout)); err != nil {
				return 0, nil, err
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			c.closeWith(wsCloseNormal, nil)
			return 0, nil, errWSClosed
		case wsOpText, wsOpBinary:
			if opcode != 0 {
				return 0, nil, errors.New("分片消息中出现了新的数据帧")
			}
			opcode = op
		case wsOpContinuation:
			if opcode == 0 {
				return 0, nil, errors.New("收到没有起始帧的续帧")
			}
		default:
			return 0, nil, fmt.Errorf("不支持的 opcode %#x", op)
		}

		buf = append(buf, payload...)
		if len(buf) > maxBodyBytes {
			c.closeWith(wsCloseTooLarge, nil)
			return 0, nil, fmt.Errorf("消息超过 %d 字节上限", maxBodyBytes)
		}
		if fin && opcode != 0 {
			return opcode, buf, nil
		}
	}
}

// readFrame 读取单个帧并解掩码。
//
// readTimeout 为正值时按帧设置读超时。客户端方向的帧必须带掩码（RFC 6455 §5.1），
// 未带掩码一律视为协议错误；载荷长度按帧头上的 7/16/64 位三段解析，
// 超过 maxBodyBytes 直接拒绝。
func (c *wsConn) readFrame(readTimeout time.Duration) (fin bool, opcode byte, payload []byte, err error) {
	if readTimeout > 0 {
		if err = c.conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return false, 0, nil, err
		}
	}

	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	if head[0]&0x70 != 0 {
		return false, 0, nil, errors.New("未协商的 RSV 位被置位")
	}
	fin = head[0]&0x80 != 0
	opcode = head[0] & 0x0f

	length := int64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u > 1<<62 {
			return false, 0, nil, errors.New("帧载荷长度非法")
		}
		length = int64(u)
	}
	if opcode >= wsOpClose && (!fin || length > wsMaxControlPayload) {
		return false, 0, nil, errors.New("控制帧必须不可分片且载荷不超过 125 字节")
	}
	if length > maxBodyBytes {
		return false, 0, nil, fmt.Errorf("帧载荷 %d 超过 %d 字节上限", length, maxBodyBytes)
	}
	if head[1]&0x80 == 0 {
		return false, 0, nil, errors.New("客户端帧未使用掩码")
	}

	var key [4]byte
	if _, err = io.ReadFull(c.br, key[:]); err != nil {
		return false, 0, nil, err
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return fin, opcode, payload, nil
}

// writeMessage 写一条数据消息，写超时取 ctx 截止时间与固定上限中更早的一个。
func (c *wsConn) writeMessage(ctx context.Context, opcode byte, payload []byte) error {
	deadline := time.Now().Add(wsWriteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return c.writeFrame(opcode, payload, deadline)
}

// writeFrame 写一个帧。服务端方向的帧不带掩码。
func (c *wsConn) writeFrame(opcode byte, payload []byte, deadline time.Time) error {
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

	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.conn.Write(frame)
	return err
}

// closeWith 发送关闭帧后关闭底层连接；可重复调用，只有第一次生效。
func (c *wsConn) closeWith(code uint16, reason []byte) error {
	var err error
	c.closeOnce.Do(func() {
		payload := make([]byte, 2, 2+len(reason))
		binary.BigEndian.PutUint16(payload, code)
		payload = append(payload, reason...)
		_ = c.writeFrame(wsOpClose, payload, time.Now().Add(wsWriteTimeout))
		err = c.conn.Close()
	})
	return err
}

// close 直接关闭底层连接，不发送关闭帧（读循环已发现连接异常时的收尾路径）。
func (c *wsConn) close() error {
	var err error
	c.closeOnce.Do(func() { err = c.conn.Close() })
	return err
}
