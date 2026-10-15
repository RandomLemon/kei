package adapterkit

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// serve 在随机端口上启动 srv，返回其基础 URL。
func serve(t *testing.T, srv *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String()
}

func TestShutdownNilServer(t *testing.T) {
	if err := Shutdown(context.Background(), nil); err != nil {
		t.Errorf("srv 为 nil 时应返回 nil，实际 = %v", err)
	}
}

func TestShutdownReleasesListener(t *testing.T) {
	srv := NewServer(ServerOptions{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
		Logger: newQuietLogger(),
	})
	base := serve(t, srv)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base)
	if err != nil {
		t.Fatalf("关停前请求失败: %v", err)
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Shutdown(ctx, srv); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
	if resp, err := client.Get(base); err == nil {
		_ = resp.Body.Close()
		t.Error("Shutdown 返回后监听端口仍接受连接")
	}

	// 重复关闭是幂等的目标状态：不因为已关闭而返回错误。
	if err := Shutdown(context.Background(), srv); err != nil {
		t.Errorf("重复 Shutdown 应返回 nil，实际 = %v", err)
	}
}

func TestShutdownBoundedByContext(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := NewServer(ServerOptions{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release
			w.WriteHeader(http.StatusNoContent)
		}),
		Logger: newQuietLogger(),
	})
	base := serve(t, srv)

	go func() {
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(base)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("在途请求未开始")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := Shutdown(ctx, srv); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("预算耗尽应返回 ctx 错误，实际 = %v", err)
	}

	close(release)
	if err := Shutdown(context.Background(), srv); err != nil {
		t.Errorf("在途请求结束后 Shutdown 应返回 nil，实际 = %v", err)
	}
}
