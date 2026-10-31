package engine

import (
	"testing"

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/pkg/bot"
)

// TestIsAdminPlatformScope 验证多平台部署下管理员 ID 必须限定平台；
// 单平台部署保持旧的未限定条目语义。
func TestIsAdminPlatformScope(t *testing.T) {
	multi := &Engine{
		cfg: &config.Config{Auth: config.AuthConfig{AdminUsers: []string{
			"123", "telegram:123", "qq:456",
		}}},
		platforms: map[string][]string{
			"mock":     {"mock-main"},
			"telegram": {"tg-main"},
		},
	}

	tests := []struct {
		name     string
		platform string
		userID   string
		want     bool
	}{
		{"多平台忽略未限定条目", "mock", "123", false},
		{"限定平台命中", "telegram", "123", true},
		{"限定平台其它用户不命中", "telegram", "456", false},
		{"限定平台不匹配", "mock", "456", false},
		{"限定平台精确匹配", "qq", "456", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := &bot.Event{Platform: tc.platform, Sender: &bot.User{ID: tc.userID}}
			if got := multi.isAdmin(ev); got != tc.want {
				t.Fatalf("isAdmin(%s:%s) = %v, want %v", tc.platform, tc.userID, got, tc.want)
			}
		})
	}

	single := &Engine{
		cfg:       &config.Config{Auth: config.AuthConfig{AdminUsers: []string{"123"}}},
		platforms: map[string][]string{"mock": {"mock-main"}},
	}
	if !single.isAdmin(&bot.Event{Platform: "mock", Sender: &bot.User{ID: "123"}}) {
		t.Fatal("单平台部署下未限定条目应生效")
	}
	if single.isAdmin(nil) || single.isAdmin(&bot.Event{Platform: "mock"}) {
		t.Fatal("nil 事件或缺少 Sender 不应是管理员")
	}
}
