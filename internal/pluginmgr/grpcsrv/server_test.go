package grpcsrv

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

const (
	tokenOK       = "token-ok"
	tokenNoPerm   = "token-noperm"
	pluginOK      = "demo"
	pluginNoPerm  = "reader"
	platformGroup = "mock"
)

// sent 记录一次 BotAPI.Send 的入参，供断言使用。
type sent struct {
	target  bot.Target
	message *bot.Message
}

// fakeBot 是记录调用并可控返回值的 bot.BotAPI 测试替身。
type fakeBot struct {
	mu     sync.Mutex
	sent   []sent
	result *bot.SendResult
	err    error
	logger *slog.Logger
}

func (f *fakeBot) Send(_ context.Context, target bot.Target, msg *bot.Message) (*bot.SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sent{target: target, message: msg})
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeBot) Reply(context.Context, *bot.Event, *bot.Message) (*bot.SendResult, error) {
	return nil, errors.New("fakeBot: Reply 未实现")
}

func (f *fakeBot) Logger() *slog.Logger {
	if f.logger == nil {
		return slog.New(&captureHandler{sink: &sink{}})
	}
	return f.logger
}

func (f *fakeBot) Storage() bot.Storage { return nil }

// requestBot 在 fakeBot 之上实现 requestSender，用于验证 reply_to 透传路径。
type requestBot struct {
	fakeBot
	mu       sync.Mutex
	requests []*bot.SendRequest
}

func (r *requestBot) SendRequest(_ context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	if r.err != nil {
		return nil, r.err
	}
	return r.result, nil
}

func (r *requestBot) lastRequest(t *testing.T) *bot.SendRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) != 1 {
		t.Fatalf("SendRequest 调用次数 = %d，期望 1", len(r.requests))
	}
	return r.requests[0]
}

func (f *fakeBot) last(t *testing.T) sent {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 {
		t.Fatalf("Send 调用次数 = %d，期望 1", len(f.sent))
	}
	return f.sent[0]
}

// sink 收集 slog 记录，并发安全。
type sink struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (s *sink) add(r slog.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, r)
}

func (s *sink) all() []slog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slog.Record(nil), s.recs...)
}

// withMessage 过滤出正文等于 msg 的记录，用于剔除服务端自身的启动/退出日志。
func (s *sink) withMessage(msg string) []slog.Record {
	out := make([]slog.Record, 0, len(s.all()))
	for _, r := range s.all() {
		if r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

// captureHandler 是保留全部级别并记录属性的 slog.Handler。
type captureHandler struct {
	sink  *sink
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c := r.Clone()
	c.AddAttrs(h.attrs...)
	h.sink.add(c)
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &captureHandler{sink: h.sink, attrs: merged}
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// attrValue 在日志记录中查找指定属性。
func attrValue(r slog.Record, key string) (slog.Value, bool) {
	var (
		val   slog.Value
		found bool
	)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val, found = a.Value, true
			return false
		}
		return true
	})
	return val, found
}

// testEnv 是一个已启动的 bufconn 服务端与客户端。
type testEnv struct {
	client pluginpb.BotServiceClient
	bot    bot.BotAPI
	logs   *sink
	server *Server
}

// newTestEnv 通过 bufconn 启动服务端并返回客户端，全部资源在测试结束时回收。
func newTestEnv(t *testing.T, fake bot.BotAPI) *testEnv {
	t.Helper()

	logs := &sink{}
	ln := bufconn.Listen(1 << 20)
	srv, err := newServer(Options{
		Bot:  fake,
		Addr: "bufconn",
		Tokens: map[string]TokenInfo{
			tokenOK:     {Plugin: pluginOK, Permissions: []bot.Permission{bot.PermSendMessage, bot.PermStorage}},
			tokenNoPerm: {Plugin: pluginNoPerm, Permissions: []bot.Permission{bot.PermReadUser}},
		},
		Configs: map[string]*bot.Config{
			pluginOK: bot.NewConfig(map[string]any{"greeting": "hi", "retries": 3}),
		},
		Logger: slog.New(&captureHandler{sink: logs}),
	}, ln)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return ln.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &testEnv{client: pluginpb.NewBotServiceClient(conn), bot: fake, logs: logs, server: srv}
}

func TestSendMessageSucceeds(t *testing.T) {
	fake := &fakeBot{result: &bot.SendResult{MessageID: "mid-42"}}
	env := newTestEnv(t, fake)

	resp, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token: tokenOK,
		Target: &pluginpb.Target{
			Platform:  platformGroup,
			ChannelId: "room-1",
			Kind:      string(bot.MessageGroup),
		},
		Message: &pluginpb.Message{
			Id:   "m-1",
			Kind: string(bot.MessageGroup),
			Segments: []*pluginpb.Segment{
				{Type: string(bot.SegText), DataJson: `{"text":"hello"}`},
				{Type: string(bot.SegAt), DataJson: `{"user_id":"u-9","name":"小明"}`},
			},
		},
		ReplyTo: "m-0",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp.GetMessageId() != "mid-42" || resp.GetError() != "" {
		t.Fatalf("响应 = %+v，期望 message_id=mid-42 且无错误", resp)
	}

	got := fake.last(t)
	wantTarget := bot.Target{Platform: platformGroup, ChannelID: "room-1", Kind: bot.MessageGroup}
	if got.target != wantTarget {
		t.Fatalf("target = %+v，期望 %+v", got.target, wantTarget)
	}
	if got.message.ID != "m-1" || got.message.Kind != bot.MessageGroup {
		t.Fatalf("message 头 = %+v", got.message)
	}
	wantSegs := []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{"text": "hello"}},
		{Type: bot.SegAt, Data: map[string]any{"user_id": "u-9", "name": "小明"}},
	}
	if !reflect.DeepEqual(got.message.Segments, wantSegs) {
		t.Fatalf("segments = %#v，期望 %#v", got.message.Segments, wantSegs)
	}
}

func TestSendMessagePassesReplyToThroughRequestSender(t *testing.T) {
	fake := &requestBot{fakeBot: fakeBot{result: &bot.SendResult{MessageID: "mid-reply"}}}
	env := newTestEnv(t, fake)

	resp, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:  tokenOK,
		Target: &pluginpb.Target{Platform: platformGroup, ChannelId: "room-1", Kind: string(bot.MessageGroup)},
		Message: &pluginpb.Message{
			Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"收到"}`}},
		},
		ReplyTo: "m-quoted",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp.GetMessageId() != "mid-reply" {
		t.Fatalf("message_id = %q，期望 mid-reply", resp.GetMessageId())
	}

	got := fake.lastRequest(t)
	if got.ReplyTo != "m-quoted" {
		t.Fatalf("SendRequest.ReplyTo = %q，期望 m-quoted", got.ReplyTo)
	}
	if got.Target.ChannelID != "room-1" || got.Target.Platform != platformGroup {
		t.Fatalf("SendRequest.Target = %+v", got.Target)
	}
	if got.Message.PlainText() != "收到" {
		t.Fatalf("SendRequest.Message 文本 = %q，期望 收到", got.Message.PlainText())
	}
	// 走 requestSender 时不应再落到普通 Send。
	if len(fake.sent) != 0 {
		t.Fatalf("reply_to 非空时不应调用 bot.Send，实际调用 %d 次", len(fake.sent))
	}
}

func TestSendMessageFallsBackWhenBotLacksRequestSender(t *testing.T) {
	fake := &fakeBot{result: &bot.SendResult{MessageID: "mid-plain"}}
	env := newTestEnv(t, fake)

	resp, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenOK,
		Target:  &pluginpb.Target{Platform: platformGroup, BotId: "mock-b", ChannelId: "room-1"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"hi"}`}}},
		ReplyTo: "m-quoted",
	})
	if err != nil {
		t.Fatalf("回退路径不应失败，实际 %v", err)
	}
	if resp.GetMessageId() != "mid-plain" || resp.GetError() != "" {
		t.Fatalf("响应 = %+v，期望成功且带 message_id", resp)
	}
	// 回退时仍必须真正把消息发出去，只是丢失引用关系；bot_id 必须仍然生效。
	got := fake.last(t)
	if got.target.ChannelID != "room-1" {
		t.Fatalf("回退发送目标 = %+v", got.target)
	}
	if got.target.BotID != "mock-b" {
		t.Fatalf("回退路径 Target.BotID = %q，期望 mock-b", got.target.BotID)
	}
}

func TestSendMessageCarriesBotIDOnPlainSend(t *testing.T) {
	fake := &fakeBot{result: &bot.SendResult{MessageID: "mid-b"}}
	env := newTestEnv(t, fake)

	if _, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenOK,
		Target:  &pluginpb.Target{Platform: platformGroup, BotId: "mock-b", ChannelId: "room-1"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"x"}`}}},
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := fake.last(t); got.target.BotID != "mock-b" {
		t.Fatalf("bot.Send 收到 Target.BotID = %q，期望 mock-b", got.target.BotID)
	}
}

func TestSendMessageCarriesBotIDOnReplyToPath(t *testing.T) {
	fake := &requestBot{fakeBot: fakeBot{result: &bot.SendResult{MessageID: "mid-b"}}}
	env := newTestEnv(t, fake)

	if _, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenOK,
		Target:  &pluginpb.Target{Platform: platformGroup, BotId: "mock-b", ChannelId: "room-1"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"x"}`}}},
		ReplyTo: "m-quoted",
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	got := fake.lastRequest(t)
	if got.Target.BotID != "mock-b" {
		t.Fatalf("SendRequest.Target.BotID = %q，期望 mock-b", got.Target.BotID)
	}
	if got.BotID != "mock-b" {
		t.Fatalf("SendRequest.BotID = %q，期望 mock-b", got.BotID)
	}
}

func TestTargetProtoRoundTrip(t *testing.T) {
	orig := &bot.Target{
		Platform:  "feishu",
		BotID:     "mock-b",
		ChannelID: "c-1",
		UserID:    "u-1",
		Kind:      bot.MessageGroup,
	}
	if got := TargetFromProto(TargetToProto(orig)); got != *orig {
		t.Fatalf("Target 往返不一致: got=%+v want=%+v", got, *orig)
	}

	// BotID 为空是合法用法（由核心按平台自动选择实例），必须原样保留为空。
	noBot := &bot.Target{Platform: "mock", ChannelID: "c-1", Kind: bot.MessagePrivate}
	if got := TargetFromProto(TargetToProto(noBot)); got != *noBot {
		t.Fatalf("无 BotID 的 Target 往返不一致: got=%+v want=%+v", got, *noBot)
	}
	if TargetToProto(nil) != nil {
		t.Fatal("nil Target 应转换为 nil")
	}
	if got := TargetFromProto(nil); got != (bot.Target{}) {
		t.Fatalf("nil proto Target 应返回零值，实际 %+v", got)
	}
}

func TestSendMessageEmptyReplyToUsesPlainSend(t *testing.T) {
	fake := &requestBot{fakeBot: fakeBot{result: &bot.SendResult{MessageID: "mid-plain"}}}
	env := newTestEnv(t, fake)

	resp, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenOK,
		Target:  &pluginpb.Target{Platform: platformGroup, ChannelId: "room-1"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"hi"}`}}},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp.GetMessageId() != "mid-plain" {
		t.Fatalf("message_id = %q", resp.GetMessageId())
	}
	// reply_to 为空时行为保持不变：走 bot.Send，不碰 SendRequest。
	if len(fake.sent) != 1 {
		t.Fatalf("bot.Send 调用次数 = %d，期望 1", len(fake.sent))
	}
	if len(fake.requests) != 0 {
		t.Fatalf("reply_to 为空时不应调用 SendRequest，实际 %d 次", len(fake.requests))
	}
}

func TestSendMessagePrivateTargetNeedsUserID(t *testing.T) {
	fake := &fakeBot{result: &bot.SendResult{MessageID: "mid-1"}}
	env := newTestEnv(t, fake)

	_, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenOK,
		Target:  &pluginpb.Target{Platform: platformGroup, Kind: string(bot.MessagePrivate)},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"x"}`}}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v，期望 InvalidArgument", status.Code(err))
	}
}

func TestSendMessageRejectsMalformedSegmentData(t *testing.T) {
	fake := &fakeBot{result: &bot.SendResult{MessageID: "mid-1"}}
	env := newTestEnv(t, fake)

	_, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenOK,
		Target:  &pluginpb.Target{Platform: platformGroup, UserId: "u-1"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: "not-json"}}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v，期望 InvalidArgument（err=%v）", status.Code(err), err)
	}
}

func TestSendMessageUnauthenticated(t *testing.T) {
	env := newTestEnv(t, &fakeBot{result: &bot.SendResult{MessageID: "mid-1"}})

	for _, token := range []string{"", "unknown-token"} {
		_, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
			Token:   token,
			Target:  &pluginpb.Target{Platform: platformGroup, ChannelId: "room-1"},
			Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"x"}`}}},
		})
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("token=%q code = %v，期望 Unauthenticated", token, status.Code(err))
		}
	}
}

func TestSendMessagePermissionDenied(t *testing.T) {
	fake := &fakeBot{result: &bot.SendResult{MessageID: "mid-1"}}
	env := newTestEnv(t, fake)

	_, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenNoPerm,
		Target:  &pluginpb.Target{Platform: platformGroup, ChannelId: "room-1"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"x"}`}}},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v，期望 PermissionDenied", status.Code(err))
	}
	if len(fake.sent) != 0 {
		t.Fatalf("无权限时不应调用 BotAPI.Send，实际调用 %d 次", len(fake.sent))
	}
}

func TestSendMessageReportsBotError(t *testing.T) {
	env := newTestEnv(t, &fakeBot{err: errors.New("平台拒绝")})

	resp, err := env.client.SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenOK,
		Target:  &pluginpb.Target{Platform: platformGroup, ChannelId: "room-1"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"x"}`}}},
	})
	if err != nil {
		t.Fatalf("平台业务失败不应返回 RPC error，实际 %v", err)
	}
	if resp.GetError() == "" || resp.GetMessageId() != "" {
		t.Fatalf("响应 = %+v，期望仅含 error", resp)
	}
}

func TestGetConfig(t *testing.T) {
	env := newTestEnv(t, &fakeBot{})

	hit, err := env.client.GetConfig(context.Background(), &pluginpb.GetConfigRequest{Token: tokenOK, Key: "greeting"})
	if err != nil {
		t.Fatalf("GetConfig(hit): %v", err)
	}
	if !hit.GetFound() || hit.GetValueJson() != `"hi"` {
		t.Fatalf("命中响应 = %+v，期望 found 且 value_json=\"hi\"", hit)
	}

	miss, err := env.client.GetConfig(context.Background(), &pluginpb.GetConfigRequest{Token: tokenOK, Key: "absent"})
	if err != nil {
		t.Fatalf("GetConfig(miss): %v", err)
	}
	if miss.GetFound() || miss.GetValueJson() != "" {
		t.Fatalf("未命中响应 = %+v，期望 found=false 且空值", miss)
	}

	all, err := env.client.GetConfig(context.Background(), &pluginpb.GetConfigRequest{Token: tokenOK})
	if err != nil {
		t.Fatalf("GetConfig(all): %v", err)
	}
	want := `{"greeting":"hi","retries":3}`
	if !all.GetFound() || all.GetValueJson() != want {
		t.Fatalf("整份配置 = %+v，期望 %s", all, want)
	}

	if _, err := env.client.GetConfig(context.Background(), &pluginpb.GetConfigRequest{Token: "nope"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("未知 token code = %v，期望 Unauthenticated", status.Code(err))
	}
}

func TestGetConfigRespectsPluginScope(t *testing.T) {
	env := newTestEnv(t, &fakeBot{})

	// pluginNoPerm 没有配置项，即使 key 在别的插件配置里存在也不应泄漏。
	resp, err := env.client.GetConfig(context.Background(), &pluginpb.GetConfigRequest{Token: tokenNoPerm, Key: "greeting"})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if resp.GetFound() {
		t.Fatalf("跨插件读取应未命中，实际 %+v", resp)
	}
}

func TestLogWritesToSlog(t *testing.T) {
	env := newTestEnv(t, &fakeBot{})

	cases := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"verbose", slog.LevelInfo},
	}
	for _, tc := range cases {
		if _, err := env.client.Log(context.Background(), &pluginpb.LogRequest{
			Token:      tokenOK,
			Level:      tc.level,
			Message:    "来自插件的日志",
			FieldsJson: `{"count":1}`,
		}); err != nil {
			t.Fatalf("Log(%s): %v", tc.level, err)
		}
	}

	recs := env.logs.withMessage("来自插件的日志")
	if len(recs) != len(cases) {
		t.Fatalf("记录条数 = %d，期望 %d", len(recs), len(cases))
	}
	for i, tc := range cases {
		r := recs[i]
		if r.Level != tc.want {
			t.Errorf("level %q 映射为 %v，期望 %v", tc.level, r.Level, tc.want)
		}
		if r.Message != "来自插件的日志" {
			t.Errorf("第 %d 条消息 = %q", i, r.Message)
		}
		if v, ok := attrValue(r, "plugin"); !ok || v.String() != pluginOK {
			t.Errorf("第 %d 条 plugin 字段 = %v（ok=%v），期望 %q", i, v, ok, pluginOK)
		}
		if v, ok := attrValue(r, "fields"); !ok || v.Any() == nil {
			t.Errorf("第 %d 条 fields 字段缺失（ok=%v）", i, ok)
		}
	}
}

func TestLogBadFieldsDoesNotFail(t *testing.T) {
	env := newTestEnv(t, &fakeBot{})

	if _, err := env.client.Log(context.Background(), &pluginpb.LogRequest{
		Token: tokenOK, Level: "info", Message: "raw", FieldsJson: "{",
	}); err != nil {
		t.Fatalf("FieldsJson 非法时不应报错，实际 %v", err)
	}
	recs := env.logs.withMessage("raw")
	if len(recs) != 1 {
		t.Fatalf("记录条数 = %d，期望 1", len(recs))
	}
	if _, ok := attrValue(recs[0], "fields_raw"); !ok {
		t.Fatalf("非法 fields_json 应降级为 fields_raw 字段: %+v", recs[0])
	}
}

func TestEventProtoRoundTrip(t *testing.T) {
	ts := time.Date(2026, 9, 23, 10, 11, 12, 345*int(time.Millisecond), time.UTC)
	orig := &bot.Event{
		ID:       "evt-1",
		Type:     bot.EventMessage,
		Platform: "feishu",
		BotID:    "bot-a",
		Time:     ts,
		Message: &bot.Message{
			ID:   "m-1",
			Kind: bot.MessageGroup,
			Segments: []bot.Segment{
				{Type: bot.SegText, Data: map[string]any{bot.KeyText: "你好"}},
				{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "u-1", bot.KeyUserName: "阿黄"}},
				{Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: "m-0"}},
			},
		},
		Sender:  &bot.User{ID: "u-1", Name: "阿黄", IsBot: false},
		Channel: &bot.Channel{ID: "c-1", Name: "测试群", Kind: bot.MessageGroup},
		Command: &bot.Command{Name: "echo", Args: []string{"hello", "world"}, Raw: "/echo hello world"},
		Raw:     map[string]any{"event": "message", "seq": "9"},
	}

	proto := EventToProto(orig)
	if proto == nil {
		t.Fatal("EventToProto 返回 nil")
	}
	if proto.GetTimeUnixMs() != ts.UnixMilli() {
		t.Fatalf("time_unix_ms = %d，期望 %d", proto.GetTimeUnixMs(), ts.UnixMilli())
	}
	if got := EventFromProto(proto); !reflect.DeepEqual(got, orig) {
		t.Fatalf("往返不一致:\n got = %#v\nwant = %#v", got, orig)
	}

	// 只保留 ID 的最小事件也必须往返一致（nil 子结构与零值时间）。
	minimal := &bot.Event{ID: "evt-2", Type: bot.EventMeta, Platform: "mock", BotID: "b"}
	if got := EventFromProto(EventToProto(minimal)); !reflect.DeepEqual(got, minimal) {
		t.Fatalf("最小事件往返不一致: %#v", got)
	}
	// BotID 是同平台多机器人部署的实例标识，往返必须保留。
	if got := EventFromProto(EventToProto(orig)); got.BotID != orig.BotID {
		t.Fatalf("往返后 BotID = %q，期望 %q", got.BotID, orig.BotID)
	}
	if EventToProto(nil) != nil || EventFromProto(nil) != nil {
		t.Fatal("nil 事件应返回 nil")
	}

	// 空映射(非 nil)的段数据必须与 nil 区分开，否则插件无法区分「无参数」与「空参数对象」。
	empty := &bot.Event{
		ID: "evt-3", Type: bot.EventMessage, Platform: "mock", BotID: "b",
		Message: &bot.Message{Segments: []bot.Segment{{Type: bot.SegCard, Data: map[string]any{}}}},
	}
	back := EventFromProto(EventToProto(empty))
	if !reflect.DeepEqual(back, empty) {
		t.Fatalf("空映射段往返不一致: %#v", back)
	}
	if back.Message.Segments[0].Data == nil {
		t.Fatal("空 data_json 段应还原为非 nil 空映射")
	}
}

func TestServerTCPStartStop(t *testing.T) {
	fake := &fakeBot{result: &bot.SendResult{MessageID: "mid-tcp"}}
	srv, err := New(Options{
		Addr:   "127.0.0.1:0",
		Bot:    fake,
		Tokens: map[string]TokenInfo{tokenOK: {Plugin: pluginOK, Permissions: []bot.Permission{bot.PermAll}}},
		Logger: slog.New(&captureHandler{sink: &sink{}}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if addr := srv.Addr(); addr != "" {
		t.Fatalf("Start 之前 Addr() = %q，期望空串", addr)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := srv.Addr()
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" || port == "0" {
		t.Fatalf("Addr() = %q，不是真实监听地址", addr)
	}
	if err := srv.Start(); err == nil {
		t.Fatal("重复 Start 应返回错误")
	}

	// 通过真实 TCP 连接证明 Addr() 可用且服务真的在跑。
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()
	resp, err := pluginpb.NewBotServiceClient(conn).SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenOK,
		Target:  &pluginpb.Target{Platform: platformGroup, ChannelId: "room"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"tcp"}`}}},
	})
	if err != nil {
		t.Fatalf("TCP SendMessage: %v", err)
	}
	if resp.GetMessageId() != "mid-tcp" {
		t.Fatalf("message_id = %q，期望 mid-tcp", resp.GetMessageId())
	}

	// Stop 幂等：连续两次都返回 nil，且端口不再接受连接。
	for i := range 2 {
		if err := srv.Stop(context.Background()); err != nil {
			t.Fatalf("第 %d 次 Stop: %v", i+1, err)
		}
	}
	if err := srv.Start(); err == nil {
		t.Fatal("Stop 之后不应允许再次 Start")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Fatalf("Stop 之后 %s 仍可连接", addr)
	}
}

func TestStartStopDoesNotLeakGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()

	for range 5 {
		srv, err := New(Options{
			Addr:   "127.0.0.1:0",
			Bot:    &fakeBot{},
			Logger: slog.New(&captureHandler{sink: &sink{}}),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := srv.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := srv.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}

	// Stop 返回即代表 Serve goroutine 已退出；给调度器少量时间做最终回收。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n := runtime.NumGoroutine(); n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine 数 %d 未回落到基线 %d，存在泄漏", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// selfSignedTLS 生成一对自签名证书，用于验证 TLS 链路（标准库，无外部依赖）。
func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kei-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析证书: %v", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
	}
}

func TestTLSListenerAcceptsTrustedClient(t *testing.T) {
	serverTLS := selfSignedTLS(t)
	fake := &fakeBot{result: &bot.SendResult{MessageID: "mid-tls"}}

	srv, err := New(Options{
		Addr:   "127.0.0.1:0",
		Bot:    fake,
		TLS:    serverTLS,
		Tokens: map[string]TokenInfo{tokenOK: {Plugin: pluginOK, Permissions: []bot.Permission{bot.PermSendMessage}}},
		Logger: slog.New(&captureHandler{sink: &sink{}}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	pool := x509.NewCertPool()
	pool.AddCert(serverTLS.Certificates[0].Leaf)
	conn, err := grpc.NewClient("passthrough:///"+srv.Addr(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "localhost"})))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := pluginpb.NewBotServiceClient(conn).SendMessage(context.Background(), &pluginpb.SendRequest{
		Token:   tokenOK,
		Target:  &pluginpb.Target{Platform: platformGroup, ChannelId: "room"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"tls"}`}}},
	})
	if err != nil {
		t.Fatalf("TLS SendMessage: %v", err)
	}
	if resp.GetMessageId() != "mid-tls" {
		t.Fatalf("message_id = %q，期望 mid-tls", resp.GetMessageId())
	}
}

func TestStopBeforeStartIsSafe(t *testing.T) {
	srv, err := New(Options{Addr: "127.0.0.1:0", Bot: &fakeBot{}, Logger: slog.New(&captureHandler{sink: &sink{}})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("未启动时 Stop: %v", err)
	}
}

func TestStopBeforeStartClosesInjectedListener(t *testing.T) {
	ln := bufconn.Listen(1 << 16)
	srv, err := newServer(Options{
		Bot:    &fakeBot{},
		Logger: slog.New(&captureHandler{sink: &sink{}}),
	}, ln)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 归还的监听器必须已关闭，否则调用方无法回收 bufconn 资源。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if conn, err := ln.DialContext(ctx); err == nil {
		_ = conn.Close()
		t.Fatal("Stop 之后注入的监听器仍可拨号")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	if _, err := New(Options{Bot: &fakeBot{}}); err == nil {
		t.Fatal("缺少 Addr 时应返回错误")
	}
	if _, err := New(Options{Addr: "127.0.0.1:0"}); err == nil {
		t.Fatal("缺少 Bot 时应返回错误")
	}
}

func TestStopCancelledContextForcesStop(t *testing.T) {
	srv, err := New(Options{
		Addr:   "127.0.0.1:0",
		Bot:    &fakeBot{},
		Logger: slog.New(&captureHandler{sink: &sink{}}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop(已取消 ctx) 错误 = %v，期望 context.Canceled", err)
	}
	if err := srv.Stop(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop 幂等应返回首次结果，实际 %v", err)
	}
}
