package feishu

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/pkg/message"
)

// testSink 是测试用的事件收集器。
type testSink struct {
	mu     sync.Mutex
	events []*bot.Event
	ch     chan *bot.Event
}

// newTestSink 创建事件收集器。
func newTestSink() *testSink {
	return &testSink{ch: make(chan *bot.Event, 16)}
}

// Emit 收集事件并通知等待方。
func (s *testSink) Emit(_ context.Context, ev *bot.Event) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	select {
	case s.ch <- ev:
	default:
	}
	return nil
}

// wait 等待一个事件，超时即失败。
func (s *testSink) wait(t *testing.T) *bot.Event {
	t.Helper()
	select {
	case ev := <-s.ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("等待事件投递超时")
		return nil
	}
}

// count 返回已收集事件数量。
func (s *testSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// baseOptions 返回一份必填字段齐全的配置。
func baseOptions() Options {
	return Options{
		Name:              "feishu-main",
		AppID:             "cli_test",
		AppSecret:         "secret",
		VerificationToken: "vtoken",
		ListenAddr:        "127.0.0.1:0",
	}
}

// newAdapter 创建测试适配器，失败即终止测试。
func newAdapter(t *testing.T, mutate func(*Options)) *Adapter {
	t.Helper()
	opts := baseOptions()
	if mutate != nil {
		mutate(&opts)
	}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return a
}

// running 是在真实监听端口上运行中的测试适配器。
type running struct {
	adapter *Adapter
	sink    *testSink
	url     string
	stop    func()
}

// startAdapter 启动适配器（真实监听 + worker），返回其回调地址。
func startAdapter(t *testing.T, mutate func(*Options)) *running {
	t.Helper()
	a := newAdapter(t, mutate)
	sink := newTestSink()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx, sink) }()

	deadline := time.Now().Add(5 * time.Second)
	for a.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("Start 未在超时前完成监听")
		}
		time.Sleep(time.Millisecond)
	}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Start 返回错误: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("Start 未随 ctx 取消而退出")
			}
		})
	}
	t.Cleanup(stop)
	return &running{adapter: a, sink: sink, url: "http://" + a.Addr() + a.opts.Path, stop: stop}
}

// post 向回调地址发送一次请求并返回响应码与响应体。
func (r *running) post(t *testing.T, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造回调请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取回调响应失败: %v", err)
	}
	return resp.StatusCode, raw
}

// padPKCS7 为测试构造 PKCS7 填充。
func padPKCS7(b []byte, size int) []byte {
	pad := size - len(b)%size
	return append(b, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

// encryptPayload 用飞书约定的 AES-256-CBC 构造加密事件密文。
func encryptPayload(t *testing.T, encryptKey, plain string) string {
	t.Helper()
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("构造 AES cipher 失败: %v", err)
	}
	iv := []byte("0123456789abcdef")
	padded := padPKCS7([]byte(plain), aes.BlockSize)
	out := make([]byte, aes.BlockSize+len(padded))
	copy(out, iv)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[aes.BlockSize:], padded)
	return base64.StdEncoding.EncodeToString(out)
}

// messageEventBody 构造一条 schema 2.0 消息事件。
func messageEventBody(eventID, chatType, messageType, content string) []byte {
	body, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id":    eventID,
			"event_type":  "im.message.receive_v1",
			"create_time": "1700000000000",
			"token":       "vtoken",
			"app_id":      "cli_test",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id":   map[string]any{"open_id": "ou_sender", "union_id": "on_sender"},
				"sender_type": "user",
			},
			"message": map[string]any{
				"message_id":   "om_1",
				"chat_id":      "oc_1",
				"chat_type":    chatType,
				"message_type": messageType,
				"content":      content,
			},
		},
	})
	return body
}

// decodeJSON 解析响应体为 map，失败即终止测试。
func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (body=%s)", err, raw)
	}
	return out
}

// assertSegments 比较消息段序列。
func assertSegments(t *testing.T, got, want []bot.Segment) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("消息段数量 = %d, 期望 %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Type != want[i].Type {
			t.Errorf("第 %d 段类型 = %q, 期望 %q", i, got[i].Type, want[i].Type)
			continue
		}
		for k, v := range want[i].Data {
			if got[i].Data[k] != v {
				t.Errorf("第 %d 段 %s = %v, 期望 %v", i, k, got[i].Data[k], v)
			}
		}
	}
}

func TestNewValidation(t *testing.T) {
	required := map[string]func(*Options){
		"Name":              func(o *Options) { o.Name = "" },
		"AppID":             func(o *Options) { o.AppID = "" },
		"AppSecret":         func(o *Options) { o.AppSecret = "" },
		"VerificationToken": func(o *Options) { o.VerificationToken = "" },
		"ListenAddr":        func(o *Options) { o.ListenAddr = "" },
	}
	for name, mutate := range required {
		opts := baseOptions()
		mutate(&opts)
		if _, err := New(opts); err == nil {
			t.Errorf("缺少 %s 时应当返回错误", name)
		} else if !strings.Contains(err.Error(), name) {
			t.Errorf("缺少 %s 的错误信息应包含字段名，实际: %v", name, err)
		}
	}

	a := newAdapter(t, nil)
	if a.opts.Path != defaultPath {
		t.Errorf("Path 默认值 = %q, 期望 %q", a.opts.Path, defaultPath)
	}
	if a.opts.BaseURL != defaultBaseURL {
		t.Errorf("BaseURL 默认值 = %q, 期望 %q", a.opts.BaseURL, defaultBaseURL)
	}
	if a.client.Timeout != defaultHTTPTimeout {
		t.Errorf("HTTPClient 超时 = %v, 期望 %v", a.client.Timeout, defaultHTTPTimeout)
	}

	// 自定义 BaseURL 末尾斜杠应被裁剪，Path 必须有前导斜杠。
	custom := newAdapter(t, func(o *Options) { o.BaseURL = "https://example.com/" })
	if custom.opts.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q, 期望去掉末尾斜杠", custom.opts.BaseURL)
	}
	bad := baseOptions()
	bad.Path = "feishu/event"
	if _, err := New(bad); err == nil {
		t.Error("Path 缺少前导 / 时应当返回错误")
	}
}

func TestNameAndCapabilities(t *testing.T) {
	a := newAdapter(t, nil)
	if a.Name() != PlatformName {
		t.Errorf("Name() = %q, 期望 %q", a.Name(), PlatformName)
	}
	if a.Addr() != "" {
		t.Errorf("未启动时 Addr() 应为空，实际 %q", a.Addr())
	}
	caps := a.Capabilities()
	want := bot.Capabilities{
		Text: true, Markdown: true, Image: true, At: true, Card: true,
		File: true, Reply: true, Private: true, Group: true,
	}
	if caps != want {
		t.Errorf("Capabilities() = %+v, 期望 %+v", caps, want)
	}
	for _, seg := range []bot.SegmentType{bot.SegText, bot.SegMarkdown, bot.SegImage, bot.SegAt, bot.SegCard, bot.SegFile, bot.SegReply} {
		if !caps.Supports(seg) {
			t.Errorf("飞书应支持消息段 %s", seg)
		}
	}
}

func TestChallenge(t *testing.T) {
	r := startAdapter(t, nil)

	body, _ := json.Marshal(map[string]string{
		"type":      "url_verification",
		"token":     "vtoken",
		"challenge": "ch-123",
	})
	status, raw := r.post(t, body, nil)
	if status != http.StatusOK {
		t.Fatalf("challenge 响应码 = %d, 期望 200", status)
	}
	got := decodeJSON(t, raw)
	if got["challenge"] != "ch-123" {
		t.Errorf("challenge 响应 = %v, 期望原样回写 ch-123", got)
	}
	if _, ok := got["code"]; ok {
		t.Errorf("challenge 响应不应包含 code 包装，实际: %v", got)
	}

	// token 不匹配。
	bad, _ := json.Marshal(map[string]string{"type": "url_verification", "token": "wrong", "challenge": "ch"})
	if status, _ := r.post(t, bad, nil); status != http.StatusUnauthorized {
		t.Errorf("token 不匹配响应码 = %d, 期望 401", status)
	}

	// 非法 JSON 与非法方法。
	if status, _ := r.post(t, []byte("{oops"), nil); status != http.StatusBadRequest {
		t.Errorf("非法 body 响应码 = %d, 期望 400", status)
	}
	resp, err := http.Get(r.url)
	if err != nil {
		t.Fatalf("GET 请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET 响应码 = %d, 期望 405", resp.StatusCode)
	}
}

func TestSignatureVerification(t *testing.T) {
	const key = "encrypt-key"
	r := startAdapter(t, func(o *Options) { o.EncryptKey = key })
	body := messageEventBody("ev_sig", "group", "text", `{"text":"hello"}`)
	ts, nonce := "1700000000", "abc"

	// 缺少签名 → 401。
	if status, _ := r.post(t, body, nil); status != http.StatusUnauthorized {
		t.Errorf("缺少签名响应码 = %d, 期望 401", status)
	}
	// 签名错误 → 401。
	if status, _ := r.post(t, body, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Request-Nonce":     nonce,
		"X-Lark-Signature":         "deadbeef",
	}); status != http.StatusUnauthorized {
		t.Errorf("签名错误响应码 = %d, 期望 401", status)
	}
	// timestamp/nonce 不同也会导致签名不匹配。
	if status, _ := r.post(t, body, map[string]string{
		"X-Lark-Request-Timestamp": "1",
		"X-Lark-Request-Nonce":     nonce,
		"X-Lark-Signature":         signature(ts, nonce, key, body),
	}); status != http.StatusUnauthorized {
		t.Errorf("篡改时间戳响应码 = %d, 期望 401", status)
	}
	if r.sink.count() != 0 {
		t.Fatalf("校验失败不应投递事件，实际 %d 条", r.sink.count())
	}

	// 正确签名 → 200 并投递。
	status, raw := r.post(t, body, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Request-Nonce":     nonce,
		"X-Lark-Signature":         signature(ts, nonce, key, body),
	})
	if status != http.StatusOK {
		t.Fatalf("签名正确响应码 = %d, 期望 200 (body=%s)", status, raw)
	}
	if got := decodeJSON(t, raw); got["code"] != float64(0) {
		t.Errorf("ACK = %v, 期望 {\"code\":0}", got)
	}
	if ev := r.sink.wait(t); ev.ID != "ev_sig" {
		t.Errorf("投递事件 ID = %q, 期望 ev_sig", ev.ID)
	}
}

func TestConvertTextAndMentions(t *testing.T) {
	r := startAdapter(t, nil)
	content := `{"text":"hi @_user_1 你好 @_user_2"}`
	body, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "ev_text", "event_type": "im.message.receive_v1",
			"create_time": "1700000000000", "token": "vtoken",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id":   map[string]any{"open_id": "ou_sender"},
				"sender_type": "user",
			},
			"message": map[string]any{
				"message_id": "om_1", "chat_id": "oc_1", "chat_type": "group",
				"message_type": "text", "content": content,
				"mentions": []map[string]any{
					{"key": "@_user_1", "id": map[string]any{"open_id": "ou_a"}, "name": "小明"},
					{"key": "@_user_2", "id": map[string]any{"open_id": "ou_b"}, "name": "小红"},
				},
			},
		},
	})
	if status, raw := r.post(t, body, nil); status != http.StatusOK {
		t.Fatalf("事件回调响应码 = %d (body=%s)", status, raw)
	}
	ev := r.sink.wait(t)

	if ev.ID != "ev_text" {
		t.Errorf("Event.ID = %q, 期望 ev_text", ev.ID)
	}
	if ev.Type != bot.EventMessage {
		t.Errorf("Event.Type = %q, 期望 message", ev.Type)
	}
	if ev.Platform != PlatformName || ev.BotID != "feishu-main" {
		t.Errorf("Platform/BotID = %q/%q, 期望 feishu/feishu-main", ev.Platform, ev.BotID)
	}
	if !ev.Time.Equal(time.UnixMilli(1700000000000).UTC()) {
		t.Errorf("Event.Time = %v, 期望 1700000000000ms 的 UTC 时间", ev.Time)
	}
	if ev.Sender == nil || ev.Sender.ID != "ou_sender" || ev.Sender.Name != "ou_sender" || ev.Sender.IsBot {
		t.Errorf("Sender = %+v, 期望 open_id 补昵称且非机器人", ev.Sender)
	}
	if ev.Channel == nil || ev.Channel.ID != "oc_1" || ev.Channel.Kind != bot.MessageGroup {
		t.Errorf("Channel = %+v, 期望 oc_1/group", ev.Channel)
	}
	if ev.Message == nil || ev.Message.ID != "om_1" || ev.Message.Kind != bot.MessageGroup {
		t.Fatalf("Message = %+v, 期望 om_1/group", ev.Message)
	}
	if ev.Command != nil {
		t.Errorf("适配器不应解析 Command，实际 %+v", ev.Command)
	}
	raw, ok := ev.Raw.(json.RawMessage)
	if !ok || !bytes.Contains(raw, []byte(`"ev_text"`)) {
		t.Errorf("Raw 应为原始 body，实际 %T %v", ev.Raw, ev.Raw)
	}
	if ev.SessionKey() != "feishu:feishu-main:oc_1:ou_sender" {
		t.Errorf("SessionKey() = %q", ev.SessionKey())
	}

	assertSegments(t, ev.Message.Segments, []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi "}},
		{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "ou_a", bot.KeyUserName: "小明"}},
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: " 你好 "}},
		{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "ou_b", bot.KeyUserName: "小红"}},
	})
	if ev.Text() != "hi  你好 " {
		t.Errorf("Event.Text() = %q, 期望 %q", ev.Text(), "hi  你好 ")
	}
}

func TestConvertImageAndFallbackMention(t *testing.T) {
	r := startAdapter(t, nil)

	// 未在 mentions 中出现的占位符应被丢弃，不泄露内部标记。
	if status, _ := r.post(t, messageEventBody("ev_txt", "p2p", "text", `{"text":"[图片]@_user_9"}`), nil); status != http.StatusOK {
		t.Fatalf("响应码 = %d", status)
	}
	ev := r.sink.wait(t)
	if ev.Message.Kind != bot.MessagePrivate {
		t.Errorf("p2p 会话 Kind = %q, 期望 private", ev.Message.Kind)
	}
	if ev.Channel == nil || ev.Channel.Kind != bot.MessagePrivate {
		t.Errorf("Channel = %+v, 期望 private", ev.Channel)
	}
	assertSegments(t, ev.Message.Segments, []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: "[图片]"}},
	})

	if status, _ := r.post(t, messageEventBody("ev_img", "group", "image", `{"image_key":"img_abc"}`), nil); status != http.StatusOK {
		t.Fatalf("响应码 = %d", status)
	}
	imgEv := r.sink.wait(t)
	assertSegments(t, imgEv.Message.Segments, []bot.Segment{
		{Type: bot.SegImage, Data: map[string]any{bot.KeyFile: "img_abc"}},
	})
}

func TestConvertUnknownEventTypeIsNotice(t *testing.T) {
	r := startAdapter(t, nil)
	body, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "ev_add", "event_type": "im.chat.member.user.added_v1",
			"create_time": "1700000000000", "token": "vtoken",
		},
		"event": map[string]any{
			"sender": map[string]any{"sender_id": map[string]any{"open_id": "ou_x"}, "sender_type": "app"},
		},
	})
	if status, raw := r.post(t, body, nil); status != http.StatusOK {
		t.Fatalf("通知事件响应码 = %d (body=%s)", status, raw)
	}
	ev := r.sink.wait(t)
	if ev.Type != bot.EventNotice {
		t.Errorf("未知事件类型 Event.Type = %q, 期望 notice", ev.Type)
	}
	if ev.Message != nil {
		t.Errorf("通知事件不应带 Message，实际 %+v", ev.Message)
	}
	if ev.Sender == nil || !ev.Sender.IsBot {
		t.Errorf("sender_type=app 应判定为机器人，实际 %+v", ev.Sender)
	}
}

func TestInvalidTokenRejected(t *testing.T) {
	r := startAdapter(t, nil)
	body := messageEventBody("ev_bad", "group", "text", `{"text":"x"}`)
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	env["header"].(map[string]any)["token"] = "nope"
	bad, _ := json.Marshal(env)
	if status, _ := r.post(t, bad, nil); status != http.StatusUnauthorized {
		t.Errorf("token 不匹配响应码 = %d, 期望 401", status)
	}
	if r.sink.count() != 0 {
		t.Errorf("校验失败不应投递事件，实际 %d 条", r.sink.count())
	}
}

func TestEncryptedEvent(t *testing.T) {
	const key = "encrypt-key"
	r := startAdapter(t, func(o *Options) { o.EncryptKey = key })

	plain := messageEventBody("ev_enc", "group", "text", `{"text":"encrypted hi"}`)
	outer, _ := json.Marshal(map[string]string{"encrypt": encryptPayload(t, key, string(plain))})
	ts, nonce := "1700000001", "n1"

	status, raw := r.post(t, outer, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Request-Nonce":     nonce,
		"X-Lark-Signature":         signature(ts, nonce, key, outer),
	})
	if status != http.StatusOK {
		t.Fatalf("加密事件响应码 = %d, 期望 200 (body=%s)", status, raw)
	}
	ev := r.sink.wait(t)
	if ev.ID != "ev_enc" {
		t.Errorf("解密后 Event.ID = %q, 期望 ev_enc", ev.ID)
	}
	if ev.Text() != "encrypted hi" {
		t.Errorf("解密后文本 = %q, 期望 \"encrypted hi\"", ev.Text())
	}
	if ev.Type != bot.EventMessage {
		t.Errorf("解密后 Event.Type = %q, 期望 message", ev.Type)
	}

	// 加密的 url_verification 也应可用。
	challenge := `{"type":"url_verification","token":"vtoken","challenge":"ch-enc"}`
	outerC, _ := json.Marshal(map[string]string{"encrypt": encryptPayload(t, key, challenge)})
	status, raw = r.post(t, outerC, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Request-Nonce":     nonce,
		"X-Lark-Signature":         signature(ts, nonce, key, outerC),
	})
	if status != http.StatusOK {
		t.Fatalf("加密 challenge 响应码 = %d (body=%s)", status, raw)
	}
	if got := decodeJSON(t, raw); got["challenge"] != "ch-enc" {
		t.Errorf("加密 challenge 响应 = %v", got)
	}
}

func TestEncryptedEventFailure(t *testing.T) {
	const key = "encrypt-key"
	r := startAdapter(t, func(o *Options) { o.EncryptKey = key })
	ts, nonce := "1700000002", "n2"

	// 用另一把 key 加密 → 解密后填充非法 → 400。
	outer, _ := json.Marshal(map[string]string{
		"encrypt": encryptPayload(t, "other-key", `{"header":{"event_id":"x","token":"vtoken"}}`),
	})
	if status, _ := r.post(t, outer, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Request-Nonce":     nonce,
		"X-Lark-Signature":         signature(ts, nonce, key, outer),
	}); status != http.StatusBadRequest {
		t.Errorf("解密失败响应码 = %d, 期望 400", status)
	}

	// 合法签名但密文非法 base64。
	bad, _ := json.Marshal(map[string]string{"encrypt": "!!!not-base64!!!"})
	if status, _ := r.post(t, bad, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Request-Nonce":     nonce,
		"X-Lark-Signature":         signature(ts, nonce, key, bad),
	}); status != http.StatusBadRequest {
		t.Errorf("非法密文响应码 = %d, 期望 400", status)
	}
	if r.sink.count() != 0 {
		t.Errorf("解密失败不应投递事件，实际 %d 条", r.sink.count())
	}
}

func TestStartStopEndToEnd(t *testing.T) {
	r := startAdapter(t, nil)
	if r.adapter.Addr() == "" {
		t.Fatal("启动后 Addr() 不应为空")
	}
	if status, raw := r.post(t, messageEventBody("ev_http", "group", "text", `{"text":"hello http"}`), nil); status != http.StatusOK {
		t.Fatalf("事件回调响应码 = %d (body=%s)", status, raw)
	} else if got := decodeJSON(t, raw); got["code"] != float64(0) {
		t.Errorf("ACK = %v, 期望 {\"code\":0}", got)
	}
	if ev := r.sink.wait(t); ev.ID != "ev_http" {
		t.Errorf("投递事件 ID = %q, 期望 ev_http", ev.ID)
	}
	r.stop()
	if r.adapter.Addr() != "" {
		t.Errorf("停止后 Addr() 应为空，实际 %q", r.adapter.Addr())
	}
}

func TestStopIdempotent(t *testing.T) {
	a := newAdapter(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("未启动时 Stop 应返回 nil，实际 %v", err)
	}

	r := startAdapter(t, nil)
	// 先由 Stop 主动停止，再重复 Stop 验证幂等。
	if err := r.adapter.Stop(ctx); err != nil {
		t.Fatalf("首次 Stop 失败: %v", err)
	}
	if err := r.adapter.Stop(ctx); err != nil {
		t.Fatalf("重复 Stop 应幂等，实际 %v", err)
	}
	r.stop()
	if err := r.adapter.Stop(context.Background()); err != nil {
		t.Fatalf("Start 退出后 Stop 仍应幂等，实际 %v", err)
	}
}

func TestStopStopsWorkers(t *testing.T) {
	r := startAdapter(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 入队一条事件，随后 Stop；Stop 返回即为投递完成。
	r.adapter.enqueue(&bot.Event{ID: "ev_drain", Type: bot.EventMessage})
	if err := r.adapter.Stop(ctx); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if got := r.sink.count(); got != 1 {
		t.Errorf("Stop 应等待已入队事件投递完毕，实际 %d 条", got)
	}
	r.stop()
}

func TestQueueOverflowDropsWithoutBlocking(t *testing.T) {
	a := newAdapter(t, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil)) })
	// 未启动 worker：队列填满后必须立即返回而不是阻塞。
	for range eventQueueCapacity + 10 {
		a.enqueue(&bot.Event{ID: "ev", Type: bot.EventMessage})
	}
	if len(a.queue) != eventQueueCapacity {
		t.Errorf("队列长度 = %d, 期望填满至 %d", len(a.queue), eventQueueCapacity)
	}
}

func TestStartRequiresSink(t *testing.T) {
	a := newAdapter(t, nil)
	err := a.Start(context.Background(), nil)
	if err == nil {
		t.Fatal("sink 为 nil 时 Start 应当返回错误")
	}
	if !errors.Is(err, errNilSink) {
		t.Errorf("Start 错误应可识别，实际 %v", err)
	}
}

func TestStartTwiceReturnsError(t *testing.T) {
	r := startAdapter(t, nil)
	if err := r.adapter.Start(context.Background(), newTestSink()); err == nil {
		t.Error("重复 Start 应当返回错误")
	}
}

func TestFallbackEventID(t *testing.T) {
	r := startAdapter(t, nil)
	body, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{"event_type": "im.message.receive_v1", "token": "vtoken"},
		"event": map[string]any{
			"sender":  map[string]any{"sender_id": map[string]any{"open_id": "ou_x"}, "sender_type": "user"},
			"message": map[string]any{"message_id": "om_1", "chat_type": "p2p", "message_type": "text", "content": `{"text":"a"}`},
		},
	})
	if status, _ := r.post(t, body, nil); status != http.StatusOK {
		t.Fatalf("响应码 = %d", status)
	}
	ev := r.sink.wait(t)
	if ev.ID == "" {
		t.Error("缺少平台事件 ID 时应生成兜底 ID")
	}
	if !strings.Contains(ev.ID, "feishu-main") {
		t.Errorf("兜底 ID = %q, 期望包含 bot 名称", ev.ID)
	}
}

func TestParsePostContent(t *testing.T) {
	r := startAdapter(t, nil)
	content := `{"title":"标题","content":[[{"tag":"text","text":"正文"},{"tag":"a","text":"链接","href":"https://example.com"}],` +
		`[{"tag":"at","user_name":"小明","user_id":"ou_a"}]]}`
	if status, raw := r.post(t, messageEventBody("ev_post", "group", "post", content), nil); status != http.StatusOK {
		t.Fatalf("富文本事件响应码 = %d (body=%s)", status, raw)
	}
	ev := r.sink.wait(t)
	if len(ev.Message.Segments) != 1 || ev.Message.Segments[0].Type != bot.SegMarkdown {
		t.Fatalf("富文本应转为单个 Markdown 段，实际 %+v", ev.Message.Segments)
	}
	text := ev.Text()
	for _, want := range []string{"标题", "正文", "[链接](https://example.com)", "@小明"} {
		if !strings.Contains(text, want) {
			t.Errorf("富文本转换结果 %q 缺少 %q", text, want)
		}
	}

	// 带 locale 包装的富文本。
	locale := `{"zh_cn":{"title":"","content":[[{"tag":"text","text":"本地化"}]]}}`
	if status, _ := r.post(t, messageEventBody("ev_post2", "group", "post", locale), nil); status != http.StatusOK {
		t.Fatalf("响应码 = %d", status)
	}
	if ev2 := r.sink.wait(t); ev2.Text() != "本地化" {
		t.Errorf("locale 富文本转换 = %q, 期望 \"本地化\"", ev2.Text())
	}
}

func TestMalformedContentYieldsNoSegments(t *testing.T) {
	r := startAdapter(t, nil)
	if status, _ := r.post(t, messageEventBody("ev_bad_content", "group", "text", `not-json`), nil); status != http.StatusOK {
		t.Fatalf("响应码 = %d, 期望 200", status)
	}
	ev := r.sink.wait(t)
	if ev.Message == nil || len(ev.Message.Segments) != 0 {
		t.Errorf("非法 content 应产生空消息段，实际 %+v", ev.Message)
	}
	if ev.Type != bot.EventMessage {
		t.Errorf("事件仍应为消息类型，实际 %q", ev.Type)
	}
}

func TestFileMessageConversion(t *testing.T) {
	r := startAdapter(t, nil)
	content := `{"file_key":"file_abc","file_name":"报告.pdf"}`
	if status, _ := r.post(t, messageEventBody("ev_file", "group", "file", content), nil); status != http.StatusOK {
		t.Fatalf("响应码 = %d", status)
	}
	ev := r.sink.wait(t)
	assertSegments(t, ev.Message.Segments, []bot.Segment{
		{Type: bot.SegFile, Data: map[string]any{bot.KeyFile: "file_abc", bot.KeyFileName: "报告.pdf"}},
	})
}

// fakeAPI 是飞书开放平台的测试替身。
type fakeAPI struct {
	mu         sync.Mutex
	tokenCalls int
	sendCalls  []sendCall
	imageCalls int
	imageBody  string
	tokenCode  int
	sendCode   int
	expire     int
}

// sendCall 记录一次发送请求。
type sendCall struct {
	path  string
	query string
	body  map[string]string
	auth  string
}

// newFakeAPI 启动测试服务。
func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{expire: 7200}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

// handle 实现飞书接口的测试行为。
func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, tokenPath):
		f.mu.Lock()
		f.tokenCalls++
		code, expire := f.tokenCode, f.expire
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": code, "msg": "ok", "tenant_access_token": "t-abc", "expire": expire,
		})

	case strings.HasSuffix(r.URL.Path, imagesPath):
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.imageCalls++
		f.imageBody = string(raw)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "ok", "data": map[string]any{"image_key": "img_uploaded"},
		})

	case r.URL.Path == "/image.png":
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fakepng"))

	case strings.Contains(r.URL.Path, messagesPath):
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		f.mu.Lock()
		f.sendCalls = append(f.sendCalls, sendCall{
			path: r.URL.Path, query: r.URL.RawQuery, body: payload, auth: r.Header.Get("Authorization"),
		})
		code := f.sendCode
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": code, "msg": "failed", "data": map[string]any{"message_id": "om_sent"},
		})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// calls 返回已记录的发送请求副本。
func (f *fakeAPI) calls() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendCall(nil), f.sendCalls...)
}

// tokenCount 返回令牌接口被调用次数。
func (f *fakeAPI) tokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

// imageCount 返回图片上传次数。
func (f *fakeAPI) imageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageCalls
}

func TestSendGroupTextAndTokenCache(t *testing.T) {
	f, srv := newFakeAPI(t)
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	req := &bot.SendRequest{
		BotID:  "feishu-main",
		Target: bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(
			message.Text("你好 "),
			message.At("ou_user"),
			message.Text(" 世界"),
		),
	}
	for range 3 {
		res, err := a.Send(context.Background(), req)
		if err != nil {
			t.Fatalf("Send 失败: %v", err)
		}
		if res.MessageID != "om_sent" {
			t.Errorf("SendResult.MessageID = %q, 期望 om_sent", res.MessageID)
		}
	}

	if got := f.tokenCount(); got != 1 {
		t.Errorf("令牌接口调用次数 = %d, 期望缓存后只调用 1 次", got)
	}
	calls := f.calls()
	if len(calls) != 3 {
		t.Fatalf("发送请求数量 = %d, 期望 3", len(calls))
	}
	c := calls[0]
	if c.path != messagesPath {
		t.Errorf("发送路径 = %q, 期望 %q", c.path, messagesPath)
	}
	if c.query != "receive_id_type=chat_id" {
		t.Errorf("查询串 = %q, 期望 receive_id_type=chat_id", c.query)
	}
	if c.auth != "Bearer t-abc" {
		t.Errorf("Authorization = %q, 期望 Bearer t-abc", c.auth)
	}
	if c.body["receive_id"] != "oc_1" {
		t.Errorf("receive_id = %q, 期望 oc_1", c.body["receive_id"])
	}
	if c.body["msg_type"] != msgTypeText {
		t.Errorf("msg_type = %q, 期望 text", c.body["msg_type"])
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(c.body["content"]), &content); err != nil {
		t.Fatalf("content 不是合法 JSON: %v", err)
	}
	if content.Text != `你好 <at user_id="ou_user"></at> 世界` {
		t.Errorf("文本内容 = %q", content.Text)
	}
}

func TestSendConcurrentTokenSingleFlight(t *testing.T) {
	f, srv := newFakeAPI(t)
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Send(context.Background(), &bot.SendRequest{
				Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
				Message: message.Plain(bot.MessageGroup, "x"),
			})
			if err != nil {
				t.Errorf("并发 Send 失败: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := f.tokenCount(); got != 1 {
		t.Errorf("并发首发令牌接口调用次数 = %d, 期望 1", got)
	}
	if got := len(f.calls()); got != 8 {
		t.Errorf("发送请求数量 = %d, 期望 8", got)
	}
}

func TestSendPrivateUsesOpenID(t *testing.T) {
	f, srv := newFakeAPI(t)
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	_, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, UserID: "ou_user", Kind: bot.MessagePrivate},
		Message: message.Plain(bot.MessagePrivate, "hi"),
	})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("发送请求数量 = %d, 期望 1", len(calls))
	}
	if calls[0].query != "receive_id_type=open_id" {
		t.Errorf("私聊查询串 = %q, 期望 receive_id_type=open_id", calls[0].query)
	}
	if calls[0].body["receive_id"] != "ou_user" {
		t.Errorf("私聊 receive_id = %q, 期望 ou_user", calls[0].body["receive_id"])
	}

	// 缺少 UserID 应报错，且不应发起请求。
	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessagePrivate},
		Message: message.Plain(bot.MessagePrivate, "hi"),
	}); err == nil {
		t.Error("私聊缺少 UserID 应当返回错误")
	}
	if got := len(f.calls()); got != 1 {
		t.Errorf("目标非法时不应发送，实际 %d 条", got)
	}
}

func TestSendReplyUsesReplyEndpoint(t *testing.T) {
	f, srv := newFakeAPI(t)
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	_, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		ReplyTo: "om_parent",
		Message: message.Plain(bot.MessageGroup, "reply"),
	})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("发送请求数量 = %d, 期望 1", len(calls))
	}
	if calls[0].path != messagesPath+"/om_parent/reply" {
		t.Errorf("回复路径 = %q, 期望 %q", calls[0].path, messagesPath+"/om_parent/reply")
	}
	if _, ok := calls[0].body["receive_id"]; ok {
		t.Errorf("回复请求不应带 receive_id，实际 %v", calls[0].body)
	}
	if calls[0].body["msg_type"] != msgTypeText {
		t.Errorf("回复 msg_type = %q, 期望 text", calls[0].body["msg_type"])
	}

	// ReplyTo 为空但消息内带引用段时，同样走回复端点。
	f.mu.Lock()
	f.sendCalls = nil
	f.mu.Unlock()
	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(message.Reply("om_quoted"), message.Text("hi")),
	}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	calls = f.calls()
	if len(calls) != 1 || calls[0].path != messagesPath+"/om_quoted/reply" {
		t.Errorf("引用段应走回复端点，实际 %+v", calls)
	}
}

func TestSendCardSegment(t *testing.T) {
	f, srv := newFakeAPI(t)
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	card := map[string]any{"config": map[string]any{"wide_screen_mode": true}}
	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(message.Card(card)),
	}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("发送请求数量 = %d, 期望 1", len(calls))
	}
	if calls[0].body["msg_type"] != msgTypeInteractive {
		t.Errorf("卡片 msg_type = %q, 期望 interactive", calls[0].body["msg_type"])
	}
	want, _ := json.Marshal(card)
	if calls[0].body["content"] != string(want) {
		t.Errorf("卡片 content = %q, 期望 %q", calls[0].body["content"], want)
	}

	// 字符串卡片原样透传。
	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(message.Card(`{"raw":true}`)),
	}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	all := f.calls()
	if all[len(all)-1].body["content"] != `{"raw":true}` {
		t.Errorf("字符串卡片 content = %q, 期望原样透传", all[len(all)-1].body["content"])
	}
}

func TestSendImageFromURLUploads(t *testing.T) {
	f, srv := newFakeAPI(t)
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(message.Image(srv.URL + "/image.png")),
	}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if got := f.imageCount(); got != 1 {
		t.Fatalf("图片上传次数 = %d, 期望 1", got)
	}
	f.mu.Lock()
	body := f.imageBody
	f.mu.Unlock()
	if !strings.Contains(body, "image_type") || !strings.Contains(body, "message") || !strings.Contains(body, "fakepng") {
		t.Errorf("上传表单缺少字段或内容: %q", body)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("发送请求数量 = %d, 期望 1", len(calls))
	}
	if calls[0].body["msg_type"] != msgTypeImage {
		t.Errorf("图片 msg_type = %q, 期望 image", calls[0].body["msg_type"])
	}
	if !strings.Contains(calls[0].body["content"], "img_uploaded") {
		t.Errorf("图片 content = %q, 期望包含上传得到的 image_key", calls[0].body["content"])
	}

	// KeyFile 直接当 image_key 使用，不再上传。
	before := f.imageCount()
	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(message.ImageFile("img_existing")),
	}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if got := f.imageCount(); got != before {
		t.Errorf("KeyFile 场景不应上传图片，上传次数从 %d 变为 %d", before, got)
	}
	all := f.calls()
	if !strings.Contains(all[len(all)-1].body["content"], "img_existing") {
		t.Errorf("KeyFile 图片 content = %q, 期望直接使用 img_existing", all[len(all)-1].body["content"])
	}

	// 既无 file 也无合法 URL 应报错。
	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(bot.Segment{Type: bot.SegImage, Data: map[string]any{bot.KeyURL: "/local/path.png"}}),
	}); err == nil {
		t.Error("图片段无有效 file/url 时应当返回错误")
	}
}

func TestSendImageTooLarge(t *testing.T) {
	f, srv := newFakeAPI(t)
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxImageBytes+1))
	}))
	defer big.Close()

	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(message.Image(big.URL + "/big.png")),
	}); err == nil {
		t.Fatal("超过 5MB 的图片应当返回错误")
	}
	if got := f.imageCount(); got != 0 {
		t.Errorf("超限图片不应上传，实际上传 %d 次", got)
	}
}

func TestSendMultiSegmentSplitsMessages(t *testing.T) {
	f, srv := newFakeAPI(t)
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	res, err := a.Send(context.Background(), &bot.SendRequest{
		Target: bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(
			message.Text("before"),
			message.ImageFile("img_1"),
			message.Text("after"),
		),
	})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.MessageID != "om_sent" {
		t.Errorf("MessageID = %q, 期望最后一条消息 ID", res.MessageID)
	}
	calls := f.calls()
	if len(calls) != 3 {
		t.Fatalf("发送请求数量 = %d, 期望 text/image/text 共 3 条", len(calls))
	}
	got := []string{calls[0].body["msg_type"], calls[1].body["msg_type"], calls[2].body["msg_type"]}
	want := []string{msgTypeText, msgTypeImage, msgTypeText}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("msg_type 顺序 = %v, 期望 %v", got, want)
	}
	var first struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(calls[0].body["content"]), &first); err != nil || first.Text != "before" {
		t.Errorf("首条文本 = %q (err=%v), 期望 before", first.Text, err)
	}
}

func TestSendMidwayFailureReturnsError(t *testing.T) {
	_, srv := newFakeAPI(t)
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	// 第一次发送失败即中止后续分块。
	var n int
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, tokenPath) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "t-abc", "expire": 7200,
			})
			return
		}
		n++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 230002, "msg": "blocked"})
	})

	_, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Group(message.Text("a"), message.ImageFile("img_1"), message.Text("b")),
	})
	if err == nil {
		t.Fatal("发送失败时应当返回错误")
	}
	if !strings.Contains(err.Error(), "230002") {
		t.Errorf("错误信息应包含 code，实际 %v", err)
	}
	if n != 1 {
		t.Errorf("首次发送失败后不应继续发送，实际请求 %d 次", n)
	}
}

func TestSendAPIError(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.sendCode = 230002
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	_, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Plain(bot.MessageGroup, "x"),
	})
	if err == nil {
		t.Fatal("code!=0 时应当返回错误")
	}
	if !strings.Contains(err.Error(), "230002") || !strings.Contains(err.Error(), "failed") {
		t.Errorf("错误信息应包含 code 与 msg，实际: %v", err)
	}
}

func TestSendTokenAPIError(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.tokenCode = 10003
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	_, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Plain(bot.MessageGroup, "x"),
	})
	if err == nil {
		t.Fatal("令牌接口失败时应当返回错误")
	}
	if !strings.Contains(err.Error(), "10003") {
		t.Errorf("错误信息应包含令牌接口 code，实际: %v", err)
	}
	if len(f.calls()) != 0 {
		t.Errorf("令牌获取失败时不应发送消息，实际 %d 条", len(f.calls()))
	}
}

func TestSendRequiresMessageAndTarget(t *testing.T) {
	a := newAdapter(t, nil)
	if _, err := a.Send(context.Background(), nil); err == nil {
		t.Error("nil SendRequest 应当返回错误")
	}
	if _, err := a.Send(context.Background(), &bot.SendRequest{}); err == nil {
		t.Error("nil Message 应当返回错误")
	}
	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Kind: bot.MessageGroup},
		Message: message.Plain(bot.MessageGroup, "x"),
	}); err == nil {
		t.Error("群聊缺少 ChannelID 应当返回错误")
	}
	if _, err := a.Send(context.Background(), &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: &bot.Message{},
	}); err == nil {
		t.Error("空消息应当返回错误")
	}
}

func TestTokenRefreshMargin(t *testing.T) {
	f, srv := newFakeAPI(t)
	// 令牌有效期很短：应按 expire/2 提前刷新，从而触发第二次刷新。
	f.expire = 2
	a := newAdapter(t, func(o *Options) { o.BaseURL = srv.URL; o.HTTPClient = srv.Client() })

	req := &bot.SendRequest{
		Target:  bot.Target{Platform: PlatformName, ChannelID: "oc_1", Kind: bot.MessageGroup},
		Message: message.Plain(bot.MessageGroup, "x"),
	}
	if _, err := a.Send(context.Background(), req); err != nil {
		t.Fatalf("首次 Send 失败: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := a.Send(context.Background(), req); err != nil {
		t.Fatalf("第二次 Send 失败: %v", err)
	}
	if got := f.tokenCount(); got != 2 {
		t.Errorf("令牌接口调用次数 = %d, 期望短有效期下刷新 2 次", got)
	}
}
