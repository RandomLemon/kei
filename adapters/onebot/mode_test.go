package onebot

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestModeValidation(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantSub string
	}{
		{
			name:    "未知 mode",
			opts:    Options{Name: "bot1", Mode: "webhook"},
			wantSub: "Options.Mode 非法",
		},
		{
			name:    "forward_http 缺少 api_url",
			opts:    Options{Name: "bot1", Mode: modeForwardHTTP, ListenAddr: "127.0.0.1:0"},
			wantSub: "需要配置 api_url",
		},
		{
			name:    "forward_http 缺少 listen_addr",
			opts:    Options{Name: "bot1", Mode: modeForwardHTTP, APIURL: "http://127.0.0.1:3000"},
			wantSub: "需要配置 listen_addr",
		},
		{
			name:    "reverse_ws 缺少 listen_addr",
			opts:    Options{Name: "bot1", Mode: modeReverseWS},
			wantSub: "需要配置 listen_addr",
		},
		{
			name:    "forward_ws 缺少 ws_url",
			opts:    Options{Name: "bot1", Mode: modeForwardWS},
			wantSub: "需要配置 ws_url",
		},
		{
			name:    "forward_ws 的 ws_url 协议非法",
			opts:    Options{Name: "bot1", Mode: modeForwardWS, WSURL: "http://127.0.0.1:1"},
			wantSub: "只支持 ws:// 与 wss://",
		},
		{
			name:    "forward_ws 的 ws_url 缺主机名",
			opts:    Options{Name: "bot1", Mode: modeForwardWS, WSURL: "ws:///onebot"},
			wantSub: "缺少主机名",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.opts)
			if err == nil {
				t.Fatalf("期望返回错误，实际为 nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息 = %q, 期望包含 %q", err.Error(), tc.wantSub)
			}
		})
	}

	t.Run("forward_http 与 reverse_http 归一为同一拓扑", func(t *testing.T) {
		fwd, err := New(Options{Name: "bot1", Mode: modeForwardHTTP, APIURL: "http://127.0.0.1:1", ListenAddr: "127.0.0.1:0"})
		if err != nil {
			t.Fatalf("forward_http New 失败: %v", err)
		}
		rev, err := New(Options{Name: "bot1", Mode: modeReverseHTTP, APIURL: "http://127.0.0.1:1", ListenAddr: "127.0.0.1:0"})
		if err != nil {
			t.Fatalf("reverse_http New 失败: %v", err)
		}
		if fwd.topo != topoHTTP || rev.topo != topoHTTP {
			t.Fatalf("topo = %v/%v, 期望都是 topoHTTP", fwd.topo, rev.topo)
		}
		if !fwd.listensHTTP() || fwd.listensWS() || !rev.listensHTTP() || rev.listensWS() {
			t.Fatalf("HTTP 拓扑应只提供 HTTP 入口: fwd=%v/%v rev=%v/%v",
				fwd.listensHTTP(), fwd.listensWS(), rev.listensHTTP(), rev.listensWS())
		}
	})

	t.Run("mode 大小写与首尾空白不敏感", func(t *testing.T) {
		a, err := New(Options{Name: "bot1", Mode: "  FORWARD_WS ", WSURL: "ws://127.0.0.1:6700"})
		if err != nil {
			t.Fatalf("New 失败: %v", err)
		}
		if a.topo != topoForwardWS || a.mode != modeForwardWS {
			t.Fatalf("topo/mode = %v/%q, 期望 topoForwardWS/%q", a.topo, a.mode, modeForwardWS)
		}
	})

	t.Run("未配置 mode 时回落到 reverse_ws", func(t *testing.T) {
		a, err := New(Options{Name: "bot1", ListenAddr: "127.0.0.1:0"})
		if err != nil {
			t.Fatalf("New 失败: %v", err)
		}
		if a.mode != defaultMode || a.topo != topoReverseWS {
			t.Fatalf("mode/topo = %q/%v, 期望 %q/topoReverseWS", a.mode, a.topo, defaultMode)
		}
	})

	t.Run("path 只在实际使用它的 mode 下校验", func(t *testing.T) {
		if _, err := New(Options{Name: "bot1", Mode: modeForwardWS, WSURL: "ws://127.0.0.1:6700", Path: "no-slash", Logger: quietLogger()}); err != nil {
			t.Fatalf("forward_ws 下非法 path 应被忽略，实际报错: %v", err)
		}
		_, err := New(Options{Name: "bot1", Mode: modeForwardHTTP, APIURL: "http://127.0.0.1:1", ListenAddr: "127.0.0.1:0", Path: "no-slash"})
		if err == nil || !strings.Contains(err.Error(), "必须以 / 开头") {
			t.Fatalf("forward_http 下非法 path 应报错，实际: %v", err)
		}
	})
}

func TestIgnoredKeys(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want []string
	}{
		{
			name: "forward_http 忽略 ws_url/ws_path/ping_interval",
			opts: Options{
				Name: "bot1", Mode: modeForwardHTTP,
				APIURL: "http://127.0.0.1:1", ListenAddr: "127.0.0.1:0",
				WSURL: "ws://127.0.0.1:6700", WSPath: "/custom/ws", PingInterval: 5 * time.Second,
				Logger: quietLogger(),
			},
			want: []string{"ping_interval", "ws_path", "ws_url"},
		},
		{
			name: "reverse_ws 忽略 path/ws_url",
			opts: Options{
				Name: "bot1", Mode: modeReverseWS, ListenAddr: "127.0.0.1:0",
				Path: "/custom/event", WSURL: "ws://127.0.0.1:6700",
				Logger: quietLogger(),
			},
			want: []string{"path", "ws_url"},
		},
		{
			name: "forward_ws 忽略 listen_addr/path/ws_path/secret",
			opts: Options{
				Name: "bot1", Mode: modeForwardWS, WSURL: "ws://127.0.0.1:6700",
				ListenAddr: "127.0.0.1:0", Path: "/onebot/event", WSPath: "/onebot/ws", Secret: "s",
				Logger: quietLogger(),
			},
			want: []string{"listen_addr", "path", "secret", "ws_path"},
		},
		{
			name: "只写该 mode 用到的键时没有忽略项",
			opts: Options{Name: "bot1", Mode: modeReverseWS, ListenAddr: "127.0.0.1:0", Logger: quietLogger()},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(tc.opts)
			if err != nil {
				t.Fatalf("New 失败: %v", err)
			}
			got := a.ignoredKeys(tc.opts)
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ignoredKeys = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

func TestModeRouting(t *testing.T) {
	t.Run("forward_http 只挂 HTTP 上报入口", func(t *testing.T) {
		h := startHarness(t, Options{Mode: modeForwardHTTP, APIURL: "http://127.0.0.1:1"})

		resp := postJSON(t, h.base+defaultPath, groupArrayEvent, nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("POST %s 状态码 = %d, 期望 204", defaultPath, resp.StatusCode)
		}
		if ev := h.sink.next(t); ev.ID != "12345" {
			t.Errorf("事件 ID = %q, 期望 12345", ev.ID)
		}

		wsResp, err := testClient.Get(h.base + defaultWSPath)
		if err != nil {
			t.Fatalf("GET %s 失败: %v", defaultWSPath, err)
		}
		defer func() { _ = wsResp.Body.Close() }()
		if wsResp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404（该 mode 不挂 ws_path）", defaultWSPath, wsResp.StatusCode)
		}
	})

	t.Run("reverse_ws 只挂反向 WebSocket 入口", func(t *testing.T) {
		h := startHarness(t, Options{})

		hs := dialWS(t, h.base, defaultWSPath, "", nil)
		if hs.code != http.StatusSwitchingProtocols {
			t.Fatalf("反向 WebSocket 握手状态码 = %d, 期望 101", hs.code)
		}
		hs.client.close()

		resp := postJSON(t, h.base+defaultPath, groupArrayEvent, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s 状态码 = %d, 期望 404（该 mode 不挂 path）", defaultPath, resp.StatusCode)
		}
	})

	t.Run("forward_ws 不监听任何端口", func(t *testing.T) {
		// 取一个当前空闲的端口作为被忽略的 listen_addr。
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("探测空闲端口失败: %v", err)
		}
		freeAddr := probe.Addr().String()
		_ = probe.Close()

		a, err := New(Options{
			Name: "bot1", Mode: modeForwardWS,
			WSURL:      "ws://127.0.0.1:1",
			ListenAddr: freeAddr,
			Logger:     quietLogger(),
		})
		if err != nil {
			t.Fatalf("New 失败: %v", err)
		}
		if a.listens() {
			t.Fatal("forward_ws 不应进入监听分支")
		}
		if got := a.Addr(); got != "" {
			t.Errorf("Start 之前 Addr() = %q, 期望空字符串", got)
		}

		ctx, cancel := context.WithCancel(context.Background())
		sink := newSinkRecorder()
		done := make(chan error, 1)
		go func() { done <- a.Start(ctx, sink) }()

		time.Sleep(200 * time.Millisecond)
		if a.mode != modeForwardWS {
			t.Errorf("mode = %q, 期望 %q", a.mode, modeForwardWS)
		}
		if got := a.Addr(); got != "" {
			t.Errorf("Start 之后 Addr() = %q, forward_ws 期望空字符串", got)
		}
		// kei 没有占用该端口：再次监听必须成功。
		ln, err := net.Listen("tcp", freeAddr)
		if err != nil {
			t.Fatalf("forward_ws 下 kei 不应占用 %s: %v", freeAddr, err)
		}
		_ = ln.Close()

		cancel()
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("Stop 失败: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Start 返回错误: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Start 未退出")
		}
	})
}
