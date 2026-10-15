package adapterkit

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newQuietLogger 返回不输出任何内容的日志器。
func newQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewServerFillsDefaults(t *testing.T) {
	handler := http.NewServeMux()
	srv := NewServer(ServerOptions{Handler: handler, Logger: newQuietLogger()})

	if srv.Handler != handler {
		t.Errorf("Handler 未被透传")
	}
	if srv.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, 期望 %v", srv.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	}
	if srv.ReadTimeout != DefaultReadTimeout {
		t.Errorf("ReadTimeout = %v, 期望 %v", srv.ReadTimeout, DefaultReadTimeout)
	}
	if srv.WriteTimeout != DefaultWriteTimeout {
		t.Errorf("WriteTimeout = %v, 期望 %v", srv.WriteTimeout, DefaultWriteTimeout)
	}
	if srv.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v, 期望 %v", srv.IdleTimeout, DefaultIdleTimeout)
	}
	if srv.ErrorLog == nil {
		t.Error("ErrorLog 必须非 nil：net/http 内部错误要有结构化日志出口")
	}
}

func TestNewServerOverridesTimeouts(t *testing.T) {
	cases := []struct {
		name           string
		opts           ServerOptions
		wantReadHeader time.Duration
		wantRead       time.Duration
		wantWrite      time.Duration
		wantIdle       time.Duration
	}{
		{
			name: "四项全部覆盖",
			opts: ServerOptions{
				ReadHeaderTimeout: time.Second,
				ReadTimeout:       2 * time.Second,
				WriteTimeout:      3 * time.Second,
				IdleTimeout:       4 * time.Second,
			},
			wantReadHeader: time.Second,
			wantRead:       2 * time.Second,
			wantWrite:      3 * time.Second,
			wantIdle:       4 * time.Second,
		},
		{
			name:           "只覆盖一项，其余取默认",
			opts:           ServerOptions{ReadTimeout: 7 * time.Second},
			wantReadHeader: DefaultReadHeaderTimeout,
			wantRead:       7 * time.Second,
			wantWrite:      DefaultWriteTimeout,
			wantIdle:       DefaultIdleTimeout,
		},
		{
			name:           "负值等同于未配置",
			opts:           ServerOptions{ReadHeaderTimeout: -1, ReadTimeout: -1, WriteTimeout: -1, IdleTimeout: -1},
			wantReadHeader: DefaultReadHeaderTimeout,
			wantRead:       DefaultReadTimeout,
			wantWrite:      DefaultWriteTimeout,
			wantIdle:       DefaultIdleTimeout,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Logger = newQuietLogger()
			srv := NewServer(tc.opts)
			if srv.ReadHeaderTimeout != tc.wantReadHeader {
				t.Errorf("ReadHeaderTimeout = %v, 期望 %v", srv.ReadHeaderTimeout, tc.wantReadHeader)
			}
			if srv.ReadTimeout != tc.wantRead {
				t.Errorf("ReadTimeout = %v, 期望 %v", srv.ReadTimeout, tc.wantRead)
			}
			if srv.WriteTimeout != tc.wantWrite {
				t.Errorf("WriteTimeout = %v, 期望 %v", srv.WriteTimeout, tc.wantWrite)
			}
			if srv.IdleTimeout != tc.wantIdle {
				t.Errorf("IdleTimeout = %v, 期望 %v", srv.IdleTimeout, tc.wantIdle)
			}
		})
	}
}

func TestNewServerLoggerDefaults(t *testing.T) {
	srv := NewServer(ServerOptions{})
	if srv.ErrorLog == nil {
		t.Fatal("Logger 为 nil 时 ErrorLog 仍必须非 nil")
	}
}

func TestNewServerErrorLogUsesWarnLevel(t *testing.T) {
	var buf bytes.Buffer
	srv := NewServer(ServerOptions{
		Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})

	srv.ErrorLog.Print("http 内部错误")

	out := buf.String()
	if !strings.Contains(out, "http 内部错误") {
		t.Errorf("ErrorLog 未写入日志器，输出 = %q", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("ErrorLog 必须按 warn 级别输出，输出 = %q", out)
	}
}
