package adapterkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// sinkRecorder 记录投递的事件；order 非 nil 时同时记录投递发生的顺序，
// 供断言「先 ACK 后投递」使用。
type sinkRecorder struct {
	mu     sync.Mutex
	order  *[]string
	events []*bot.Event
	err    error
}

func (s *sinkRecorder) Emit(_ context.Context, ev *bot.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.order != nil {
		*s.order = append(*s.order, "emit")
	}
	s.events = append(s.events, ev)
	return s.err
}

func (s *sinkRecorder) snapshot() []*bot.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*bot.Event(nil), s.events...)
}

// orderingWriter 记录响应被写出（WriteHeader）的顺序。
type orderingWriter struct {
	*httptest.ResponseRecorder
	order *[]string
}

func (w orderingWriter) WriteHeader(code int) {
	*w.order = append(*w.order, "response")
	w.ResponseRecorder.WriteHeader(code)
}

// okParse 是总是成功的解析钩子，返回固定 ID 的事件。
func okParse(_ context.Context, _ []byte) (*bot.Event, error) {
	return &bot.Event{ID: "ev-1"}, nil
}

func TestHandlerStatusesAndLogs(t *testing.T) {
	errBoom := errors.New("boom")
	cases := []struct {
		name       string
		method     string
		body       string
		maxBody    int64
		authorize  func(*http.Request) bool
		parse      func(context.Context, []byte) (*bot.Event, error)
		emitErr    error
		wantStatus int
		wantAllow  string
		wantLog    string
		wantEmit   int
	}{
		{
			name:       "非 POST 返回 405 且不记日志",
			method:     http.MethodGet,
			authorize:  func(*http.Request) bool { return true },
			parse:      okParse,
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  http.MethodPost,
		},
		{
			name:       "鉴权失败返回 401",
			method:     http.MethodPost,
			authorize:  func(*http.Request) bool { return false },
			parse:      okParse,
			wantStatus: http.StatusUnauthorized,
			wantLog:    "上报校验失败",
		},
		{
			name:       "请求体超限返回 400",
			method:     http.MethodPost,
			body:       strings.Repeat("x", 32),
			maxBody:    8,
			authorize:  func(*http.Request) bool { return true },
			parse:      okParse,
			wantStatus: http.StatusBadRequest,
			wantLog:    "读取上报请求体失败",
		},
		{
			name:       "解析失败返回 400",
			method:     http.MethodPost,
			authorize:  func(*http.Request) bool { return true },
			parse:      func(context.Context, []byte) (*bot.Event, error) { return nil, errBoom },
			wantStatus: http.StatusBadRequest,
			wantLog:    "解析上报事件失败",
		},
		{
			name:       "未配置解析钩子返回 400",
			method:     http.MethodPost,
			wantStatus: http.StatusBadRequest,
			wantLog:    "解析上报事件失败",
		},
		{
			name:       "成功返回 204 并投递一次",
			method:     http.MethodPost,
			authorize:  func(*http.Request) bool { return true },
			parse:      okParse,
			wantStatus: http.StatusNoContent,
			wantEmit:   1,
		},
		{
			name:       "Emit 失败仍保持 204",
			method:     http.MethodPost,
			authorize:  func(*http.Request) bool { return true },
			parse:      okParse,
			emitErr:    errBoom,
			wantStatus: http.StatusNoContent,
			wantLog:    "投递事件失败",
			wantEmit:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			opts := EventSourceOptions{
				Name:         "kit",
				MaxBodyBytes: tc.maxBody,
				Authorize:    tc.authorize,
				Parse:        tc.parse,
				Logger:       slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
			}
			sink := &sinkRecorder{err: tc.emitErr}

			var order []string
			rec := orderingWriter{ResponseRecorder: httptest.NewRecorder(), order: &order}
			req := httptest.NewRequest(tc.method, "/event", strings.NewReader(tc.body))
			opts.Handler(sink).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("状态码 = %d, 期望 %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Result().Header.Get("Allow"); got != tc.wantAllow {
				t.Errorf("Allow = %q, 期望 %q", got, tc.wantAllow)
			}
			events := sink.snapshot()
			if len(events) != tc.wantEmit {
				t.Errorf("投递次数 = %d, 期望 %d", len(events), tc.wantEmit)
			}

			logged := buf.String()
			if tc.wantLog == "" {
				if logged != "" {
					t.Errorf("不应有日志，实际 = %q", logged)
				}
				return
			}
			if !strings.Contains(logged, "kit: "+tc.wantLog) {
				t.Errorf("日志应含 %q，实际 = %q", "kit: "+tc.wantLog, logged)
			}
			if !strings.Contains(logged, "level=WARN") {
				t.Errorf("日志级别应为 WARN，实际 = %q", logged)
			}
		})
	}
}

func TestHandlerAcknowledgesBeforeEmit(t *testing.T) {
	var order []string
	sink := &sinkRecorder{order: &order}
	rec := orderingWriter{ResponseRecorder: httptest.NewRecorder(), order: &order}
	opts := EventSourceOptions{Name: "kit", Parse: okParse, Logger: newQuietLogger()}

	opts.Handler(sink).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/event", strings.NewReader("{}")))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204", rec.Code)
	}
	if got := strings.Join(order, ", "); got != "response, emit" {
		t.Errorf("调用顺序 = %q, 期望「response, emit」（先 ACK 后投递）", got)
	}
}

// TestHandlerThroughServer 走真实 HTTP 连接，验证 Flusher 分支与端到端状态码。
func TestHandlerThroughServer(t *testing.T) {
	sink := &sinkRecorder{}
	opts := EventSourceOptions{
		Name:      "kit",
		Authorize: TokenAuthorizer("s3cr3t"),
		Parse:     okParse,
		Logger:    newQuietLogger(),
	}
	srv := httptest.NewServer(opts.Handler(sink))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer s3cr3t")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("204 响应体应为空，实际 = %q", body)
	}
	if events := sink.snapshot(); len(events) != 1 || events[0].ID != "ev-1" {
		t.Errorf("投递事件 = %v, 期望一条 ID 为 ev-1 的事件", events)
	}
}

func TestTokenAuthorizer(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		header string
		query  string
		want   bool
	}{
		{name: "Bearer 匹配", token: "s3cr3t", header: "Bearer s3cr3t", want: true},
		{name: "Bearer 大小写不敏感", token: "s3cr3t", header: "bearer s3cr3t", want: true},
		{name: "Bearer 错误", token: "s3cr3t", header: "Bearer nope", want: false},
		{name: "query 匹配", token: "s3cr3t", query: "s3cr3t", want: true},
		{name: "query 错误", token: "s3cr3t", query: "nope", want: false},
		{name: "两者都缺失", token: "s3cr3t", want: false},
		{name: "Bearer 错但 query 对", token: "s3cr3t", header: "Bearer nope", query: "s3cr3t", want: true},
		{name: "无 Bearer 前缀", token: "s3cr3t", header: "s3cr3t", want: false},
		{name: "令牌为空时不校验（无凭据）", token: "", want: true},
		{name: "令牌为空时不校验（含任意凭据）", token: "", header: "Bearer whatever", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := "/event"
			if tc.query != "" {
				url += "?access_token=" + tc.query
			}
			req := httptest.NewRequest(http.MethodPost, url, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if got := TokenAuthorizer(tc.token)(req); got != tc.want {
				t.Errorf("TokenAuthorizer(%q) = %v, 期望 %v", tc.token, got, tc.want)
			}
		})
	}
}
