package storage

import (
	"errors"
	"fmt"
)

// MaxKeyBytes 是 Storage 键的最大字节数。
//
// 该值来自 SQL 后端的列定义（Entry.Key 的 size:191，即 MySQL utf8mb4 索引前缀
// 的安全上限）。内存实现本身不限长：若不在共享层校验，同一个超长键会在
// memory/SQLite 上静默可用、只在 MySQL 上失败。所有内置实现都在读写前调用
// CheckKey，保证一个键要么在任何后端都能用，要么都返回同一个错误。
const MaxKeyBytes = 191

// ErrKeyTooLong 表示键超过 MaxKeyBytes。错误可用 errors.Is 识别。
var ErrKeyTooLong = errors.New("storage: 键超过长度上限")

// CheckKey 校验键可被任何内置后端存取；超长时返回包装 ErrKeyTooLong 的错误。
//
// 键是平台/插件提供的任意字符串，这里限制的是存储键的字节长度，不是字符数：
// 按字节计算比 MySQL 按字符数的列约束更保守，多字节键不会出现「MySQL 能存、
// 别处也能存」之外的差异。
func CheckKey(key string) error {
	if len(key) <= MaxKeyBytes {
		return nil
	}
	return fmt.Errorf("%w: %d 字节（上限 %d）: %q", ErrKeyTooLong, len(key), MaxKeyBytes, keyPreview(key))
}

// keyPreview 截断超长键用于错误信息，避免单条错误被长键撑爆。
func keyPreview(key string) string {
	const limit = 64
	if len(key) <= limit {
		return key
	}
	return key[:limit] + "..."
}
