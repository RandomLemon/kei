package storage

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Opener 依据配置构造一种 Storage 实现。
type Opener func(cfg *bot.Config) (bot.Storage, error)

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
func Open(kind string, cfg *bot.Config) (bot.Storage, error) {
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
	return opener(cfg)
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
