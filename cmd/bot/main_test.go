package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomLemon/kei/internal/config"
	"github.com/RandomLemon/kei/pkg/bot"
)

// TestExampleConfigLoads 校验仓库自带示例配置仍然完整可用：它是 README
// 快速开始与 CLI 联调流程的输入。
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "configs", "config.yaml"))
	if err != nil {
		t.Fatalf("示例配置无法加载: %v", err)
	}
	if len(cfg.Bots) == 0 {
		t.Fatal("示例配置应至少配置一个 bot")
	}
	adapters := map[string]bool{}
	for _, b := range cfg.Bots {
		adapters[b.Adapter] = true
	}
	for _, want := range []string{"mock", "feishu", "onebot"} {
		if !adapters[want] {
			t.Fatalf("示例配置缺少 %s 适配器示例: %v", want, adapters)
		}
	}
	for _, name := range []string{"echo", "manage"} {
		if pc := cfg.Plugin(name); !pc.Enabled {
			t.Fatalf("示例配置应启用插件 %s", name)
		}
	}

	// 示例配置里的 mock bot 必须是 mock 适配器可用的设置。
	var mockBot config.BotConfig
	for _, b := range cfg.Bots {
		if b.Adapter == "mock" {
			mockBot = b
		}
	}
	// README 里的 curl 联调流程依赖 mock bot 的 listen_addr 与 platform 设置。
	if got, _ := mockBot.Settings["listen_addr"].(string); got == "" {
		t.Fatal("示例配置的 mock bot 必须配置 listen_addr")
	}
	if got, _ := mockBot.Settings["platform"].(string); got != "mock" {
		t.Fatalf("mock bot 的 platform = %q, want mock", mockBot.Settings["platform"])
	}

	// 示例配置用实例级 enabled: false 停用飞书 bot：快速开始依赖「只跑 mock」，
	// 该开关必须真的生效（而不是像早期那样落入 Settings 静默失效）。
	var feishuBot config.BotConfig
	for _, b := range cfg.Bots {
		if b.Adapter == "feishu" {
			feishuBot = b
		}
	}
	if feishuBot.IsEnabled() {
		t.Fatal("示例配置的飞书 bot 应通过 enabled: false 停用")
	}
	if _, ok := feishuBot.Settings["enabled"]; ok {
		t.Fatalf("enabled 不应落入 Settings: %+v", feishuBot.Settings)
	}
}

// TestVersionFlag 覆盖 CLI 薄壳的 -version：只打印版本，不加载配置。
func TestVersionFlag(t *testing.T) {
	out := captureStdout(t, func() {
		if err := run([]string{"-version"}); err != nil {
			t.Fatalf("run(-version) = %v", err)
		}
	})
	if want := "kei v" + bot.Version; !strings.Contains(out, want) {
		t.Fatalf("输出 = %q, want 包含 %q", out, want)
	}
}

// TestUnknownFlag 覆盖 CLI 薄壳对未知 flag 的错误处理。
func TestUnknownFlag(t *testing.T) {
	if err := run([]string{"-nope"}); err == nil {
		t.Fatal("未知 flag 应返回错误")
	}
}

// captureStdout 在 fn 执行期间把 os.Stdout 重定向到管道，返回 fn 的输出。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatalf("关闭管道: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读取输出: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("关闭读取端: %v", err)
	}
	return string(out)
}
