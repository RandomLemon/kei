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

// CheckTableName 校验表名可被任何内置后端使用：非空且不超过 MaxKeyBytes。
//
// 空名单独报错而不是允许：表名是命名集合的标识，空名没有可表达的含义，且 SQL
// 后端的唯一索引会把所有空名并成一张表。命名空间前缀同样计入长度上限。
func CheckTableName(name string) error {
	if name == "" {
		return errors.New("storage: 表名不能为空")
	}
	if len(name) > MaxKeyBytes {
		return nameTooLong{kind: "表名", n: len(name)}
	}
	return nil
}

// nameTooLong 报出具体的名字类型（表名/行键），同时可用 errors.Is 识别为
// ErrKeyTooLong：调用方既能得到可读原因，也能按统一哨兵判定。
type nameTooLong struct {
	kind string
	n    int
}

func (e nameTooLong) Error() string {
	return fmt.Sprintf("storage: %s超过长度上限: %d 字节（上限 %d）", e.kind, e.n, MaxKeyBytes)
}

func (e nameTooLong) Unwrap() error { return ErrKeyTooLong }

// CheckRowKey 校验行键可被任何内置后端使用：非空且不超过 MaxKeyBytes。
//
// 行键为空时无法区分「没有行」与「空键的行」，因此与表名一样要求非空。
func CheckRowKey(key string) error {
	if key == "" {
		return errors.New("storage: 行键不能为空")
	}
	return CheckKey(key)
}

// keyPreview 截断超长键用于错误信息，避免单条错误被长键撑爆。
func keyPreview(key string) string {
	const limit = 64
	if len(key) <= limit {
		return key
	}
	return key[:limit] + "..."
}
