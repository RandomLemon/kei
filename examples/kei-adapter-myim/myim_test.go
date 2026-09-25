package myim

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// sampleEvent 是一条完整的平台回调，两条内容相同的回调应产生相同的 Event.ID。
const sampleEvent = `{"id":"evt-1","type":"message","time":1750000000,"message_id":"m-1",` +
	`"user_id":"u-1","user_name":"小明","group_id":"g-1","group_name":"测试群","text":"hello"}`

// recordingSink 是测试用的事件出口，Emit 只入队，由测试侧读取。
type recordingSink struct {
	events chan *bot.Event

	mu   sync.Mutex
	seen []*bot.Event
}

// Emit 记录并投递事件。
func (s *recordingSink) Emit(_ context.Context, ev *bot.Event) error {
	s.mu.Lock()
	s.seen = append(s.seen, ev)
	s.mu.Unlock()
	s.events <- ev
	return nil
}

// wait 等待 n 条事件，超时报错。
func (s *recordingSink) wait(t *testing.T, n int) []*bot.Event {
	t.Helper()
	out := make([]*bot.Event, 0, n)
	for len(out) < n {
		select {
		case ev := <-s.events:
			out = append(out, ev)
		case <-time.After(5 * time.Second):
			t.Fatalf("等待事件超时：期望 %d 条，得到 %d 条", n, len(out))
		}
	}
	return out
}

// gateSink 的 Emit 会阻塞到 release 关闭，用于验证回调线程不等待投递。
type gateSink struct {
	entered chan *bot.Event
	release chan struct{}
	once    sync.Once
}

// Emit 通知已进入投递，然后阻塞到释放或 ctx 结束。
func (s *gateSink) Emit(ctx context.Context, ev *bot.Event) error {
	s.entered <- ev
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil
}

// unblock 释放被阻塞的 Emit，可重复调用。
func (s *gateSink) unblock() { s.once.Do(func() { close(s.release) }) }

// running 是一个已启动的适配器及其测试侧依赖。
type running struct {
	ad    *Adapter
	sink  *recordingSink
	errCh chan error
}

// newTestAdapter 用自选参数构造实例，Logger 指向丢弃输出，避免测试噪音。
func newTestAdapter(t *testing.T, opts Options) *Adapter {
	t.Helper()
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ad, err := NewFromOptions(opts)
	if err != nil {
		t.Fatalf("NewFromOptions 返回错误: %v", err)
	}
	return ad
}

// startRunning 在后台启动适配器，等端口可连接后返回；测试结束自动停止。
func startRunning(t *testing.T, ad *Adapter) *running {
	t.Helper()
	sink := &recordingSink{events: make(chan *bot.Event, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- ad.Start(ctx, sink) }()

	deadline := time.Now().Add(5 * time.Second)
	for ad.Addr() == "" {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Start 未在期限内完成监听")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitDial(t, ad.Addr())

	t.Cleanup(func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := ad.Stop(stopCtx); err != nil {
			t.Errorf("Stop 返回错误: %v", err)
		}
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Start 返回错误: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("Start 在 ctx 结束后未返回")
		}
	})
	return &running{ad: ad, sink: sink, errCh: errCh}
}

// waitDial 轮询等待端口可建立 TCP 连接。
func waitDial(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("端口 %s 未就绪: %v", addr, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// postCallback 向适配器入站地址投递一条回调，返回状态码与响应体。
func postCallback(t *testing.T, addr, path, body string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post("http://"+addr+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("投递回调失败: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取回调响应失败: %v", err)
	}
	return resp.StatusCode, string(data)
}

// TestRegistryLookup 验证 init() 注册生效，且元信息与 register.go 一致。
func TestRegistryLookup(t *testing.T) {
	t.Parallel()

	meta, factory, ok := bot.LookupAdapter("myim")
	if !ok {
		t.Fatalf("LookupAdapter(\"myim\") 未命中，已注册: %v", registeredNames())
	}
	if factory == nil {
		t.Fatal("LookupAdapter 返回的工厂为 nil")
	}
	if meta.Name != "myim" || meta.Version != "v0.1.0" || meta.Author != "third-party" || meta.Description == "" {
		t.Errorf("元信息不匹配: %+v", meta)
	}
	if got, want := meta.Platforms, []string{"myim"}; !equalStrings(got, want) {
		t.Errorf("Platforms = %v，期望 %v", got, want)
	}
	wantPerms := []bot.Permission{bot.PermNetwork, bot.PermNetListen}
	if len(meta.Permissions) != len(wantPerms) {
		t.Fatalf("Permissions = %v，期望 %v", meta.Permissions, wantPerms)
	}
	for i, p := range wantPerms {
		if meta.Permissions[i] != p {
			t.Errorf("Permissions[%d] = %q，期望 %q", i, meta.Permissions[i], p)
		}
	}
	if !meta.HasPermission(bot.PermNetwork) || !meta.HasPermission(bot.PermNetListen) || meta.HasPermission(bot.PermAll) {
		t.Errorf("权限判定不符合声明: %v", meta.Permissions)
	}
	wantOpts := []string{"api_base", "listen_addr", "path", "self_id"}
	if !equalStrings(meta.Options, wantOpts) {
		t.Errorf("Options = %v，期望 %v", meta.Options, wantOpts)
	}
}

// registeredNames 列出注册表中的适配器名，仅用于失败信息。
func registeredNames() []string {
	all := bot.RegisteredAdapters()
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m.Name)
	}
	return out
}

// equalStrings 比较两个字符串切片是否逐项相等。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFactoryRequiresHTTPClient 验证未声明 network 权限时工厂报错。
func TestFactoryRequiresHTTPClient(t *testing.T) {
	t.Parallel()

	_, factory, ok := bot.LookupAdapter("myim")
	if !ok {
		t.Fatal("适配器未注册")
	}
	ad, err := factory(bot.AdapterContext{
		BotID:  "myim-main",
		Config: bot.NewConfig(map[string]any{"listen_addr": "127.0.0.1:0"}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		t.Fatalf("HTTPClient 为 nil 时应报错，实际得到实例 %v", ad)
	}
	if !strings.Contains(err.Error(), "network") {
		t.Errorf("错误信息应提示缺少 network 权限，实际: %v", err)
	}
}

// TestNewFromOptionsValidation 验证构造参数校验。
func TestNewFromOptionsValidation(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"缺少 BotID", Options{ListenAddr: "127.0.0.1:0", Logger: log}, "BotID"},
		{"缺少 ListenAddr", Options{BotID: "b", Logger: log}, "ListenAddr"},
		{"Path 不以 / 开头", Options{BotID: "b", ListenAddr: "127.0.0.1:0", Path: "callback", Logger: log}, "Path"},
	}
	for _, tc := range cases {
		if _, err := NewFromOptions(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: 期望提示 %q 的错误，实际: %v", tc.name, tc.want, err)
		}
	}
}

// TestStartStopLifecycle 验证 Start 返回前完成监听、Stop 幂等。
func TestStartStopLifecycle(t *testing.T) {
	t.Parallel()

	ad := newTestAdapter(t, Options{
		BotID:      "lifecycle",
		ListenAddr: "127.0.0.1:0",
		HTTPClient: &http.Client{Timeout: time.Second},
	})
	r := startRunning(t, ad)

	select {
	case err := <-r.errCh:
		t.Fatalf("Start 已返回 %v，但契约要求它阻塞到 ctx 结束", err)
	default:
	}

	// 端口可连接即证明「Start 完成监听后才进入阻塞」。
	waitDial(t, ad.Addr())
	status, _ := postCallback(t, ad.Addr(), "/", sampleEvent)
	if status != http.StatusAccepted {
		t.Errorf("回调状态码 = %d，期望 %d", status, http.StatusAccepted)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ad.Stop(ctx); err != nil {
		t.Errorf("首次 Stop 返回错误: %v", err)
	}
	if err := ad.Stop(ctx); err != nil {
		t.Errorf("再次 Stop 应返回 nil，实际: %v", err)
	}
	if got := ad.Addr(); got == "" {
		t.Error("Addr 在 Stop 后仍应保留实际监听地址")
	}
	if _, err := net.DialTimeout("tcp", ad.Addr(), 200*time.Millisecond); err == nil {
		t.Error("Stop 后端口仍可连接")
	}
}

// TestStopBeforeStart 验证未启动时的 Stop 不报错。
func TestStopBeforeStart(t *testing.T) {
	t.Parallel()

	ad := newTestAdapter(t, Options{BotID: "idle", ListenAddr: "127.0.0.1:0"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ad.Stop(ctx); err != nil {
		t.Errorf("未启动时 Stop 应返回 nil，实际: %v", err)
	}
}

// TestCallbackAcksBeforeEmit 验证回调先得到 202，投递在后台进行。
func TestCallbackAcksBeforeEmit(t *testing.T) {
	t.Parallel()

	ad := newTestAdapter(t, Options{BotID: "ack", ListenAddr: "127.0.0.1:0", Path: "/myim/callback"})
	sink := &gateSink{entered: make(chan *bot.Event, 1), release: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- ad.Start(ctx, sink) }()
	t.Cleanup(func() {
		sink.unblock()
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := ad.Stop(stopCtx); err != nil {
			t.Errorf("Stop 返回错误: %v", err)
		}
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Start 返回错误: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Start 在 ctx 结束后未返回")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for ad.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("Start 未在期限内完成监听")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitDial(t, ad.Addr())

	// 投递被 gateSink 卡住，回调仍须立即得到 202。
	status, body := postCallback(t, ad.Addr(), "/myim/callback", sampleEvent)
	if status != http.StatusAccepted {
		t.Fatalf("回调状态码 = %d，期望 %d（body=%s）", status, http.StatusAccepted, body)
	}
	select {
	case ev := <-sink.entered:
		if ev.ID != "evt-1" {
			t.Errorf("事件 ID = %q，期望 %q", ev.ID, "evt-1")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("事件未被投递到 sink")
	}
	sink.unblock()
}

// TestEventUplink 验证回调上行事件的平台、BotID 与 ID 稳定性。
func TestEventUplink(t *testing.T) {
	t.Parallel()

	const path = "/myim/callback"
	ad := newTestAdapter(t, Options{
		BotID:      "uplink",
		ListenAddr: "127.0.0.1:0",
		Path:       path,
		HTTPClient: &http.Client{Timeout: time.Second},
	})
	r := startRunning(t, ad)

	status, _ := postCallback(t, ad.Addr(), path, sampleEvent)
	if status != http.StatusAccepted {
		t.Fatalf("回调状态码 = %d，期望 %d", status, http.StatusAccepted)
	}
	second := strings.Replace(sampleEvent, "evt-1", "evt-2", 1)
	if status, _ := postCallback(t, ad.Addr(), path, second); status != http.StatusAccepted {
		t.Fatalf("第二条回调状态码 = %d，期望 %d", status, http.StatusAccepted)
	}

	events := r.sink.wait(t, 2)
	if events[0].ID != "evt-1" || events[1].ID != "evt-2" {
		t.Fatalf("相同平台事件 ID 应原样保留: %q, %q", events[0].ID, events[1].ID)
	}
	for _, ev := range events {
		if ev.Platform != "myim" {
			t.Errorf("Platform = %q，期望 myim", ev.Platform)
		}
		if ev.BotID != "uplink" {
			t.Errorf("BotID = %q，期望 uplink", ev.BotID)
		}
		if ev.ID == "" {
			t.Error("Event.ID 不能为空")
		}
		if ev.Type != bot.EventMessage {
			t.Errorf("Type = %q，期望 message", ev.Type)
		}
		if ev.Message == nil || ev.Message.PlainText() != "hello" {
			t.Errorf("消息内容不正确: %+v", ev.Message)
		}
		if ev.Sender == nil || ev.Sender.ID != "u-1" {
			t.Errorf("Sender 不正确: %+v", ev.Sender)
		}
		if ev.Channel == nil || ev.Channel.ID != "g-1" {
			t.Errorf("Channel 不正确: %+v", ev.Channel)
		}
		if !ev.Time.Equal(time.Unix(1750000000, 0).UTC()) {
			t.Errorf("Time = %v，期望 1750000000 UTC", ev.Time)
		}
	}

	// 同一实例重复投递同一平台事件得到相同 ID，可用于核心去重。
	if status, _ := postCallback(t, ad.Addr(), path, sampleEvent); status != http.StatusAccepted {
		t.Fatalf("重复回调状态码 = %d", status)
	}
	third := r.sink.wait(t, 1)[0]
	if third.ID != events[0].ID {
		t.Errorf("相同输入的事件 ID 应稳定: %q vs %q", events[0].ID, third.ID)
	}
}

// TestEventIDFallback 验证平台未提供事件 ID 时生成稳定的兜底 ID。
func TestEventIDFallback(t *testing.T) {
	t.Parallel()

	ad := newTestAdapter(t, Options{
		BotID:      "fallback",
		SelfID:     "10001",
		ListenAddr: "127.0.0.1:0",
		HTTPClient: &http.Client{Timeout: time.Second},
	})
	r := startRunning(t, ad)

	// 同样的输出（去掉 id/time）连续两条也必须能区分。
	const body = `{"type":"message","user_id":"u-1","text":"hi"}`
	for i := 0; i < 2; i++ {
		if status, _ := postCallback(t, ad.Addr(), "/", body); status != http.StatusAccepted {
			t.Fatalf("第 %d 条回调状态码 = %d", i+1, status)
		}
	}
	events := r.sink.wait(t, 2)
	if events[0].ID == "" || events[1].ID == "" {
		t.Fatalf("兜底 ID 不能为空: %q, %q", events[0].ID, events[1].ID)
	}
	if !strings.HasPrefix(events[0].ID, "myim-10001-") || !strings.HasPrefix(events[1].ID, "myim-10001-") {
		t.Errorf("兜底 ID 前缀不正确: %q, %q", events[0].ID, events[1].ID)
	}
	if events[0].ID == events[1].ID {
		t.Errorf("兜底 ID 必须唯一，实际都是 %q", events[0].ID)
	}
	if events[0].Time.IsZero() {
		t.Error("平台未给时间时 Event.Time 应填当前时间")
	}
}

// freeAddrs 连续预留 n 个互不相同的本地地址并立即释放，
// 供需要「配置里就写清不同端口」的测试使用。
func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	addrs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("预留端口失败: %v", err)
		}
		listeners = append(listeners, ln)
		addrs = append(addrs, ln.Addr().String())
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
	return addrs
}

// TestMultiInstanceIsolation 验证同平台多实例互不影响。
func TestMultiInstanceIsolation(t *testing.T) {
	t.Parallel()

	_, factory, ok := bot.LookupAdapter("myim")
	if !ok {
		t.Fatal("适配器未注册")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	addrs := freeAddrs(t, 2)
	makeInstance := func(t *testing.T, botID, listenAddr, selfID string) *Adapter {
		t.Helper()
		ad, err := factory(bot.AdapterContext{
			BotID: botID,
			Config: bot.NewConfig(map[string]any{
				"api_base":    "https://" + botID + ".example.com",
				"listen_addr": listenAddr,
				"self_id":     selfID,
			}),
			Logger:     log,
			HTTPClient: &http.Client{Timeout: time.Second},
		})
		if err != nil {
			t.Fatalf("工厂构造 %s 失败: %v", botID, err)
		}
		typed, ok := ad.(*Adapter)
		if !ok {
			t.Fatalf("工厂返回类型 %T，期望 *Adapter", ad)
		}
		return typed
	}

	first := makeInstance(t, "myim-a", addrs[0], "10001")
	second := makeInstance(t, "myim-b", addrs[1], "10002")
	if first == second {
		t.Fatal("工厂必须每次返回独立实例")
	}
	want := bot.Capabilities{Text: true, At: true, Reply: true, Private: true, Group: true}
	if first.Capabilities() != want {
		t.Errorf("实例一能力 = %+v，期望 %+v", first.Capabilities(), want)
	}

	r1 := startRunning(t, first)
	r2 := startRunning(t, second)

	if first.Addr() == second.Addr() {
		t.Fatalf("两个实例监听了同一地址 %s", first.Addr())
	}
	if first.Name() != "myim" || second.Name() != "myim" {
		t.Errorf("Name 应为平台名 myim: %q, %q", first.Name(), second.Name())
	}
	if first.Capabilities() != want || second.Capabilities() != want {
		t.Errorf("启动后能力被改变: %+v, %+v", first.Capabilities(), second.Capabilities())
	}

	if status, _ := postCallback(t, first.Addr(), "/", sampleEvent); status != http.StatusAccepted {
		t.Fatalf("实例一回调状态码 = %d", status)
	}
	if status, _ := postCallback(t, second.Addr(), "/", sampleEvent); status != http.StatusAccepted {
		t.Fatalf("实例二回调状态码 = %d", status)
	}
	ev1 := r1.sink.wait(t, 1)[0]
	ev2 := r2.sink.wait(t, 1)[0]
	if ev1.BotID != "myim-a" || ev2.BotID != "myim-b" {
		t.Errorf("事件 BotID 未按实例隔离: %q, %q", ev1.BotID, ev2.BotID)
	}
	if len(r2.sink.seen) != 1 || len(r1.sink.seen) != 1 {
		t.Errorf("事件串到了别的实例: 实例一 %d 条，实例二 %d 条", len(r1.sink.seen), len(r2.sink.seen))
	}
	if first.Addr() != addrs[0] || second.Addr() != addrs[1] {
		t.Errorf("实例应各自监听配置的地址: %s vs %s, %s vs %s", first.Addr(), addrs[0], second.Addr(), addrs[1])
	}

	// 停掉其中一个实例不应影响另一个。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.Stop(ctx); err != nil {
		t.Fatalf("停止实例一失败: %v", err)
	}
	if status, _ := postCallback(t, second.Addr(), "/", sampleEvent); status != http.StatusAccepted {
		t.Errorf("实例一停止后实例二回调状态码 = %d，期望 %d", status, http.StatusAccepted)
	}
	if ev := r2.sink.wait(t, 1)[0]; ev.BotID != "myim-b" {
		t.Errorf("实例二事件 BotID = %q，期望 myim-b", ev.BotID)
	}
}

// sendPayloadFor 用于在测试侧解析适配器发出的请求体。
type sendPayloadFor struct {
	Target struct {
		Kind      string `json:"kind"`
		UserID    string `json:"user_id"`
		ChannelID string `json:"channel_id"`
	} `json:"target"`
	Message struct {
		Kind     string `json:"kind"`
		Segments []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			UserID    string `json:"user_id"`
			MessageID string `json:"message_id"`
		} `json:"segments"`
	} `json:"message"`
}

// TestSend 验证发送请求的路径、请求体与降级行为。
func TestSend(t *testing.T) {
	t.Parallel()

	type captured struct {
		path string
		body sendPayloadFor
	}
	got := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var payload sendPayloadFor
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("请求体不是合法 JSON: %v (%s)", err, data)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
		got <- captured{path: r.URL.Path, body: payload}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message_id":"platform-42"}`))
	}))
	defer srv.Close()

	ad := newTestAdapter(t, Options{
		BotID:      "sender",
		APIBase:    srv.URL,
		ListenAddr: "127.0.0.1:0",
		HTTPClient: srv.Client(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := ad.Send(ctx, &bot.SendRequest{
		BotID:  "sender",
		Target: bot.Target{Platform: "myim", BotID: "sender", ChannelID: "g-1", UserID: "u-1", Kind: bot.MessageGroup},
		Message: &bot.Message{
			Kind: bot.MessageGroup,
			Segments: []bot.Segment{
				{Type: bot.SegText, Data: map[string]any{bot.KeyText: "你好"}},
				{Type: bot.SegImage, Data: map[string]any{bot.KeyURL: "https://cdn.example.com/1.png"}},
				{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "u-2"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Send 返回错误: %v", err)
	}
	if res.MessageID != "platform-42" {
		t.Errorf("SendResult.MessageID = %q，期望 platform-42", res.MessageID)
	}

	var c captured
	select {
	case c = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("平台未收到发送请求")
	}
	if c.path != "/message/send" {
		t.Errorf("请求路径 = %q，期望 /message/send", c.path)
	}
	if c.body.Target.Kind != "group" || c.body.Target.ChannelID != "g-1" || c.body.Target.UserID != "u-1" {
		t.Errorf("请求体里的 target 不正确: %+v", c.body.Target)
	}
	if c.body.Message.Kind != "group" {
		t.Errorf("请求体里的 kind = %q，期望 group", c.body.Message.Kind)
	}
	segs := c.body.Message.Segments
	if len(segs) != 3 {
		t.Fatalf("段数 = %d，期望 3: %+v", len(segs), segs)
	}
	if segs[0].Type != "text" || segs[0].Text != "你好" {
		t.Errorf("文本段不正确: %+v", segs[0])
	}
	// Capabilities.Image 为 false，图片段在发送前已降级为文本。
	if segs[1].Type != "text" || segs[1].Text != "[image] https://cdn.example.com/1.png" {
		t.Errorf("图片段未按能力降级: %+v", segs[1])
	}
	if segs[2].Type != "at" || segs[2].UserID != "u-2" {
		t.Errorf("@ 段不正确（At 能力为 true 应原样保留）: %+v", segs[2])
	}
}

// TestSendErrors 验证发送链路的错误路径，不静默丢消息。
func TestSendErrors(t *testing.T) {
	t.Parallel()

	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message_id":"x"}`))
	}))
	defer srv.Close()
	client := srv.Client()

	cases := []struct {
		name string
		opts Options
		req  *bot.SendRequest
		want string
	}{
		{
			name: "缺少 api_base",
			opts: Options{BotID: "no-api", ListenAddr: "127.0.0.1:0", HTTPClient: client},
			req:  &bot.SendRequest{Message: textMessage("hi")},
			want: "api_base",
		},
		{
			name: "缺少 HTTPClient",
			opts: Options{BotID: "no-client", APIBase: srv.URL, ListenAddr: "127.0.0.1:0"},
			req:  &bot.SendRequest{Message: textMessage("hi")},
			want: "HTTPClient",
		},
		{
			name: "空消息",
			opts: Options{BotID: "empty", APIBase: srv.URL, ListenAddr: "127.0.0.1:0", HTTPClient: client},
			req:  &bot.SendRequest{},
			want: "降级",
		},
		{
			name: "降级后为空",
			opts: Options{BotID: "degraded-empty", APIBase: srv.URL, ListenAddr: "127.0.0.1:0", HTTPClient: client},
			req: &bot.SendRequest{Message: &bot.Message{Segments: []bot.Segment{
				{Type: bot.SegMarkdown, Data: map[string]any{bot.KeyText: ""}},
			}}},
			want: "降级",
		},
		{
			name: "空请求",
			opts: Options{BotID: "nil-req", APIBase: srv.URL, ListenAddr: "127.0.0.1:0", HTTPClient: client},
			req:  nil,
			want: "不能为空",
		},
	}
	for _, tc := range cases {
		ad := newTestAdapter(t, tc.opts)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := ad.Send(ctx, tc.req)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: 期望提示 %q 的错误，实际: %v", tc.name, tc.want, err)
		}
	}
	if called.Load() {
		t.Error("错误路径不应发起平台请求")
	}
}

// textMessage 构造一条仅含文本的消息。
func textMessage(text string) *bot.Message {
	return &bot.Message{Kind: bot.MessageGroup, Segments: []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
	}}
}
