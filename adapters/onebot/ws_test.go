package onebot

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// deadlineProbeConn 记录是否有 SetWriteDeadline 落在另一个 Write 进行期间。
type deadlineProbeConn struct {
	net.Conn // 其余方法不会被 writeFrame 使用

	writing    atomic.Bool
	violations atomic.Int32
	started    chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (c *deadlineProbeConn) SetWriteDeadline(time.Time) error {
	if c.writing.Load() {
		c.violations.Add(1)
	}
	return nil
}

func (c *deadlineProbeConn) Write(p []byte) (int, error) {
	c.writing.Store(true)
	c.once.Do(func() { close(c.started) })
	<-c.release
	c.writing.Store(false)
	return len(p), nil
}

// TestWriteFrameDoesNotRaceWriteDeadline 验证并发写不会在实际写入期间改写
// 连接级写 deadline（writeMu 必须在 SetWriteDeadline 之前获取）。
func TestWriteFrameDoesNotRaceWriteDeadline(t *testing.T) {
	conn := &deadlineProbeConn{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	ws := &wsConn{conn: conn}

	results := make(chan error, 2)
	go func() {
		results <- ws.writeFrame(wsOpText, []byte("first"), time.Now().Add(time.Second))
	}()
	<-conn.started

	go func() {
		results <- ws.writeFrame(wsOpText, []byte("second"), time.Now().Add(time.Second))
	}()
	// 给第二个写方足够时间走到旧实现里锁外的 SetWriteDeadline。
	time.Sleep(20 * time.Millisecond)
	close(conn.release)

	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
	}
	if n := conn.violations.Load(); n != 0 {
		t.Fatalf("SetWriteDeadline 在其它 Write 进行期间被调用 %d 次", n)
	}
}
