package storage

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Opener 依据配置与日志器构造一种 Storage 实现。
//
// logger 由调用方（Open）保证非 nil；实现可自行决定是否使用。
type Opener func(cfg *bot.Config, logger *slog.Logger) (bot.Storage, error)

// registry 保存已注册的存储类型，并发安全。
var registry = struct {
	mu      sync.RWMutex
	openers map[string]Opener
}{openers: map[string]Opener{}}

// Register 注册一种存储类型，由各后端包在 init() 中调用。
//
// kind 重复或为空、opener 为 nil 属编程错误，直接 panic。
func Register(kind string, opener Opener) {
	if kind == "" {
		panic("storage: 注册类型名不能为空")
	}
	if opener == nil {
		panic(fmt.Sprintf("storage: 存储类型 %q 的 opener 为 nil", kind))
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, dup := registry.openers[kind]; dup {
		panic(fmt.Sprintf("storage: 重复注册存储类型 %q", kind))
	}
	registry.openers[kind] = opener
}

// Open 按 kind 构造 Storage；未知类型返回错误，错误文本列出已注册类型（升序、逗号分隔）。
//
// logger 透传给具体实现（用于后端可观测性，如 sqlstore 的清理失败日志）；nil 时使用
// slog.Default()。
func Open(kind string, cfg *bot.Config, logger *slog.Logger) (bot.Storage, error) {
	if logger == nil {
		logger = slog.Default()
	}
	registry.mu.RLock()
	opener, ok := registry.openers[kind]
	registry.mu.RUnlock()
	if !ok {
		kinds := Kinds()
		registered := strings.Join(kinds, ", ")
		if registered == "" {
			registered = "无"
		}
		return nil, fmt.Errorf("storage: 未知存储类型 %q（已注册: %s）", kind, registered)
	}
	return opener(cfg, logger)
}

// Kinds 返回已注册类型名，升序。
func Kinds() []string {
	registry.mu.RLock()
	out := make([]string, 0, len(registry.openers))
	for kind := range registry.openers {
		out = append(out, kind)
	}
	registry.mu.RUnlock()
	sort.Strings(out)
	return out
}
