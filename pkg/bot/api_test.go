package bot

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConfigAccessors(t *testing.T) {
	cfg := NewConfig(map[string]any{
		"api_key":  "secret",
		"enabled":  true,
		"retries":  3,
		"timeout":  "2s",
		"timeout2": 5,
		"list":     []any{"a", "b"},
		"nested":   map[string]any{"deep": "value"},
		"bad_bool": "not-a-bool",
	})

	if got := cfg.String("api_key", "def"); got != "secret" {
		t.Fatalf("String = %q", got)
	}
	if got := cfg.String("missing", "def"); got != "def" {
		t.Fatalf("缺失键 String = %q, want def", got)
	}
	if got := cfg.Bool("enabled", false); !got {
		t.Fatal("Bool 应为 true")
	}
	if got := cfg.Bool("bad_bool", true); !got {
		t.Fatal("无法解析的布尔应回退默认值 true")
	}
	if got := cfg.Int("retries", 0); got != 3 {
		t.Fatalf("Int = %d, want 3", got)
	}
	if got := cfg.Duration("timeout", 0); got != 2*time.Second {
		t.Fatalf("Duration = %v, want 2s", got)
	}
	if got := cfg.Duration("timeout2", 0); got != 5*time.Second {
		t.Fatalf("数字 Duration = %v, want 5s", got)
	}
	if got := cfg.Strings("list"); len(got) != 2 || got[0] != "a" {
		t.Fatalf("Strings = %v", got)
	}
	if got := cfg.String("nested.deep", ""); got != "value" {
		t.Fatalf("嵌套 String = %q", got)
	}
	if _, ok := cfg.Get("nested.missing"); ok {
		t.Fatal("Get 不应命中不存在的路径")
	}

	type decoded struct {
		APIKey  string `json:"api_key"`
		Retries int    `json:"retries"`
	}
	var d decoded
	if err := cfg.Unmarshal(&d); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if d.APIKey != "secret" || d.Retries != 3 {
		t.Fatalf("Unmarshal = %+v", d)
	}

	var nilCfg *Config
	if got := nilCfg.String("x", "def"); got != "def" {
		t.Fatalf("nil Config String = %q", got)
	}
	if got := nilCfg.Bool("x", true); !got {
		t.Fatal("nil Config Bool 应返回默认值")
	}
	if got := nilCfg.Raw(); got != nil {
		t.Fatalf("nil Config Raw = %v", got)
	}
}

type fakeStorage struct {
	set map[string][]byte
}

func (f *fakeStorage) Get(context.Context, string) ([]byte, error) { return nil, ErrNotFound }
func (f *fakeStorage) Set(_ context.Context, k string, v []byte, _ time.Duration) error {
	f.set[k] = v
	return nil
}
func (f *fakeStorage) Delete(context.Context, string) error { return nil }

func TestBotAPIAndStorageInterfaces(t *testing.T) {
	var st Storage = &fakeStorage{set: map[string][]byte{}}
	if err := st.Set(context.Background(), "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := st.Get(context.Background(), "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get 应返回 ErrNotFound, got %v", err)
	}
}

func TestPluginContextRoundTrip(t *testing.T) {
	want := PluginContext{Name: "demo", Config: NewConfig(map[string]any{"a": 1})}
	ctx := WithPluginContext(context.Background(), want)

	got, ok := PluginContextFrom(ctx)
	if !ok || got.Name != "demo" {
		t.Fatalf("PluginContextFrom = %+v, ok=%v", got, ok)
	}
	if _, ok := PluginContextFrom(context.Background()); ok {
		t.Fatal("未注入时应返回 false")
	}

	route := &Route{Plugin: "demo", RuleID: "demo:cmd", Trigger: "command:demo", Priority: 5, AdminOnly: true}
	ctx = WithRoute(ctx, route)
	gotRoute, ok := RouteFrom(ctx)
	if !ok || gotRoute.RuleID != "demo:cmd" || !gotRoute.AdminOnly {
		t.Fatalf("RouteFrom = %+v, ok=%v", gotRoute, ok)
	}
	if ctx := WithRoute(context.Background(), nil); ctx == nil {
		t.Fatal("WithRoute(nil) 应返回原 ctx")
	}
}

func TestNoopReply(t *testing.T) {
	r := NewNoopReply().SetKind(MessageGroup)
	if err := r.Text("hello ").Markdown("**w**").Image("http://x").At("u1").Card(map[string]any{"k": "v"}).Send(context.Background()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := r.PlainText(); got != "hello **w**" {
		t.Fatalf("PlainText = %q", got)
	}
	if got := r.Message(); got.Kind != MessageGroup || len(got.Segments) != 5 {
		t.Fatalf("Message = %+v", got)
	}
	if segs := r.Segments(); len(segs) != 5 {
		t.Fatalf("Segments 长度 = %d", len(segs))
	}
	// 空值不得产生段。
	empty := NewNoopReply()
	empty.Text("").Image("").At("").Card(nil)
	if len(empty.Segments()) != 0 {
		t.Fatalf("空参数不应追加段: %+v", empty.Segments())
	}
}

func TestTargetFromEvent(t *testing.T) {
	ev := &Event{
		Platform: "feishu", BotID: "feishu-main",
		Message: &Message{Kind: MessageGroup},
		Channel: &Channel{ID: "chat1", Kind: MessageGroup},
		Sender:  &User{ID: "ou_1"},
	}
	got := TargetFromEvent(ev)
	want := Target{Platform: "feishu", BotID: "feishu-main", ChannelID: "chat1", UserID: "ou_1", Kind: MessageGroup}
	if got != want {
		t.Fatalf("TargetFromEvent = %+v, want %+v", got, want)
	}
	if got := TargetFromEvent(nil); got != (Target{}) {
		t.Fatalf("TargetFromEvent(nil) = %+v", got)
	}
}
