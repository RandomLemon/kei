package bot

import "testing"

func TestEventSessionKey(t *testing.T) {
	tests := []struct {
		name string
		ev   *Event
		want string
	}{
		{
			name: "群聊使用频道 ID",
			ev: &Event{
				Platform: "mock", BotID: "b1",
				Channel: &Channel{ID: "g1"},
				Sender:  &User{ID: "u1"},
			},
			want: "mock:b1:g1:u1",
		},
		{
			name: "私聊回退到用户 ID",
			ev: &Event{
				Platform: "mock", BotID: "b1",
				Sender: &User{ID: "u1"},
			},
			want: "mock:b1:u1:u1",
		},
		{
			name: "缺少发送者",
			ev:   &Event{Platform: "mock", BotID: "b1", Channel: &Channel{ID: "g1"}},
			want: "mock:b1:g1:",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.SessionKey(); got != tc.want {
				t.Fatalf("SessionKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		prefixes []string
		want     *Command
	}{
		{name: "斜杠命令", text: "/echo hello world", prefixes: []string{"/"}, want: &Command{Name: "echo", Args: []string{"hello", "world"}, Raw: "hello world"}},
		{name: "多个前缀", text: "!ping", prefixes: []string{"/", "!"}, want: &Command{Name: "ping"}},
		{name: "保留参数内部空白", text: "/say  a  b ", prefixes: []string{"/"}, want: &Command{Name: "say", Args: []string{"a", "b"}, Raw: "a  b"}},
		{name: "无前缀", text: "echo hi", prefixes: []string{"/"}, want: nil},
		{name: "只有前缀", text: "/", prefixes: []string{"/"}, want: nil},
		{name: "前缀后只有空白", text: "/   ", prefixes: []string{"/"}, want: nil},
		{name: "空文本", text: "   ", prefixes: []string{"/"}, want: nil},
		{name: "无前缀列表", text: "/echo", prefixes: nil, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseCommand(tc.text, tc.prefixes)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("ParseCommand() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseCommand() = nil, want %+v", tc.want)
			}
			if got.Name != tc.want.Name || got.Raw != tc.want.Raw {
				t.Fatalf("ParseCommand() = {Name:%q Raw:%q}, want {Name:%q Raw:%q}", got.Name, got.Raw, tc.want.Name, tc.want.Raw)
			}
			if len(got.Args) != len(tc.want.Args) {
				t.Fatalf("Args = %v, want %v", got.Args, tc.want.Args)
			}
			for i := range got.Args {
				if got.Args[i] != tc.want.Args[i] {
					t.Fatalf("Args[%d] = %q, want %q", i, got.Args[i], tc.want.Args[i])
				}
			}
		})
	}
}

func TestEventText(t *testing.T) {
	ev := &Event{Message: &Message{Segments: []Segment{
		{Type: SegText, Data: map[string]any{KeyText: "hello "}},
		{Type: SegImage, Data: map[string]any{KeyURL: "http://x/y.png"}},
		{Type: SegText, Data: map[string]any{KeyText: "world"}},
	}}}
	if got, want := ev.Text(), "hello world"; got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
	if got := (*Event)(nil).Text(); got != "" {
		t.Fatalf("nil event Text() = %q, want empty", got)
	}
}
